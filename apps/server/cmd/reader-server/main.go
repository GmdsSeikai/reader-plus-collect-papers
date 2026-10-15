package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reader.local/server/internal/diagnostics"
	"reader.local/server/internal/networkprobe"
	"reader.local/server/internal/reader"
	"runtime"
	"sync"
	"syscall"
	"time"
)

var readerVersion = "dev"
var buildCommit = "unknown"

func main() {
	data := flag.String("data", ".reader", "local library directory")
	port := flag.String("port", "17840", "loopback port, 0 for automatic")
	web := flag.String("web", "", "built web directory")
	connector := flag.Bool("connector", true, "enable Reader Connector listener")
	probeNetwork := flag.Bool("probe-network", false, "check ChatGPT reachability and exit without opening a library")
	logs := flag.String("logs", "", "diagnostic log directory")
	session := flag.String("session", "", "diagnostic session ID")
	flag.Parse()
	if *probeNetwork {
		_ = json.NewEncoder(os.Stdout).Encode(networkprobe.Run(context.Background()))
		return
	}
	root, err := filepath.Abs(*data)
	if err != nil {
		log.Fatal(err)
	}
	if *logs == "" {
		*logs = filepath.Join(root, "logs")
	}
	recorder, logErr := diagnostics.New(*logs, *session, os.Getenv("READER_TOKEN"))
	if logErr == nil {
		slog.SetDefault(recorder.Logger())
	} else {
		log.Printf("local logging unavailable: %v", logErr)
	}
	recorder.Environment = map[string]any{"mode": "server", "appVersion": readerVersion, "buildCommit": buildCommit, "os": runtime.GOOS, "architecture": runtime.GOARCH, "goVersion": runtime.Version()}
	slog.Info("server.start", "appVersion", readerVersion, "buildCommit", buildCommit, "goVersion", runtime.Version())
	unlock, err := reader.LockLibrary(root)
	if err != nil {
		log.Fatal(err)
	}
	defer unlock()
	store, err := reader.OpenStore(root)
	if err != nil {
		log.Fatal(err)
	}
	defer store.DB.Close()
	token := os.Getenv("READER_TOKEN")
	if token == "" {
		b := make([]byte, 32)
		if _, err = rand.Read(b); err != nil {
			log.Fatal(err)
		}
		token = hex.EncodeToString(b)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:"+*port)
	if err != nil {
		log.Fatal(err)
	}
	recorder.Secrets = append(recorder.Secrets, token)
	server := reader.NewServer(store, token, *web)
	server.SetDiagnostics(recorder)
	defer server.Close()
	// Startup recovery reconciles crash-interrupted imports and runs before
	// any worker starts or request is admitted.
	if err := server.RecoverImports(); err != nil {
		log.Fatal(err)
	}
	if !*connector {
		server.SetConnectorUnavailable("disabled")
	} else {
		connectorListener, connectorError := net.Listen("tcp", "127.0.0.1:"+reader.ConnectorPort)
		if connectorError != nil {
			server.SetConnectorUnavailable("port-in-use")
			log.Printf("Reader Connector unavailable: %v", connectorError)
		} else {
			server.SetConnectorAvailable(true)
			connectorHTTP := &http.Server{Handler: server.ConnectorHandler(), ReadHeaderTimeout: 10 * time.Second}
			go func() { _ = connectorHTTP.Serve(connectorListener) }()
			defer connectorHTTP.Shutdown(context.Background())
		}
	}
	runContext, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	// Background workers use their own context so a cancelled drain can rebuild
	// them without cancelling in-flight HTTP (the drain status API must work
	// while workers stop). The drain owner covers processing AND collection.
	var bgMu sync.Mutex
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	stopProcessing := server.StartProcessing(workerCtx)
	stopCollection := server.Collection().StartCollection(workerCtx)
	server.SetDrainHooks(func() <-chan struct{} {
		done := make(chan struct{})
		go func() {
			bgMu.Lock()
			cancel, stopP, stopC := cancelWorkers, stopProcessing, stopCollection
			bgMu.Unlock()
			cancel()
			// Processing and collection share the cancelled context; each
			// stop waits for its own owned work to unwind.
			stopC()
			stopP()
			close(done)
		}()
		return done
	}, func() {
		bgMu.Lock()
		defer bgMu.Unlock()
		workerCtx, cancelWorkers = context.WithCancel(context.Background())
		stopProcessing = server.StartProcessing(workerCtx)
		stopCollection = server.Collection().StartCollection(workerCtx)
	})
	defer func() {
		bgMu.Lock()
		cancel, stopP, stopC := cancelWorkers, stopProcessing, stopCollection
		bgMu.Unlock()
		cancel()
		stopC()
		stopP()
	}()
	httpServer := &http.Server{BaseContext: func(net.Listener) context.Context { return runContext }, Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	removeDiscovery, discoveryErr := reader.WriteDiscovery(root, reader.Discovery{URL: "http://" + listener.Addr().String(), Token: token, PID: os.Getpid(), Version: readerVersion})
	if discoveryErr != nil {
		log.Printf("command line discovery unavailable: %v", discoveryErr)
	} else {
		defer removeDiscovery()
	}
	ready, _ := json.Marshal(map[string]string{"url": "http://" + listener.Addr().String(), "token": token})
	fmt.Println(string(ready))
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		slog.Info("server.stopping")
		cancelRun()
		// Once the process decides to exit, a drain cannot be cancelled.
		server.MarkClosing()
		bgMu.Lock()
		cancel, bgStopP, bgStopC := cancelWorkers, stopProcessing, stopCollection
		bgMu.Unlock()
		cancel()
		bgStopC()
		bgStopP()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}()
	if err = httpServer.Serve(listener); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
