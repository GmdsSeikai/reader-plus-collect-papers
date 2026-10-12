package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reader.local/server/internal/reader"
	"sync"
	"syscall"
	"time"
)

func main() {
	data := flag.String("data", ".reader", "local library directory")
	port := flag.String("port", "17840", "loopback port, 0 for automatic")
	web := flag.String("web", "", "built web directory")
	connector := flag.Bool("connector", true, "enable Reader Connector listener")
	flag.Parse()
	root, err := filepath.Abs(*data)
	if err != nil {
		log.Fatal(err)
	}
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
	server := reader.NewServer(store, token, *web)
	defer server.Close()
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
	// Background workers use their own context so a cancelled drain can rebuild
	// them without cancelling in-flight HTTP (the drain status API must work
	// while workers stop).
	var bgMu sync.Mutex
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	stopProcessing := server.StartProcessing(workerCtx)
	// The drain owner stops background work before writes may finish, and a
	// cancelled exit rebuilds scheduling on a fresh context.
	server.SetDrainHooks(func() <-chan struct{} {
		done := make(chan struct{})
		go func() {
			bgMu.Lock()
			cancel, stop := cancelWorkers, stopProcessing
			bgMu.Unlock()
			cancel()
			stop()
			close(done)
		}()
		return done
	}, func() {
		bgMu.Lock()
		defer bgMu.Unlock()
		workerCtx, cancelWorkers = context.WithCancel(context.Background())
		stopProcessing = server.StartProcessing(workerCtx)
	})
	defer func() {
		bgMu.Lock()
		cancel, stop := cancelWorkers, stopProcessing
		bgMu.Unlock()
		cancel()
		stop()
	}()
	httpServer := &http.Server{Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	ready, _ := json.Marshal(map[string]string{"url": "http://" + listener.Addr().String(), "token": token})
	fmt.Println(string(ready))
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		// Once the process decides to exit, a drain cannot be cancelled.
		server.MarkClosing()
		bgMu.Lock()
		cancel, bgStop := cancelWorkers, stopProcessing
		bgMu.Unlock()
		cancel()
		bgStop()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}()
	if err = httpServer.Serve(listener); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
