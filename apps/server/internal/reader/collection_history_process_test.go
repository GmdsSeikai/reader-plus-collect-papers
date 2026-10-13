package reader

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"reader.local/server/internal/scholar"
)

// A separate test process uses the service's library lock, recovery and worker
// assembly. Its clock and TLS source are fixtures; it never contacts a model or
// public source. Killing it releases the real OS lock and database connection.
func TestCollectionHistoryProcessWorker(t *testing.T) {
	root := os.Getenv("READER_HISTORY_PROCESS_ROOT")
	if root == "" {
		return
	}
	unlock, err := LockLibrary(root)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	s := NewServer(store, "process-fixture", "")
	defer s.Close()
	if err := s.RecoverImports(); err != nil {
		t.Fatal(err)
	}
	fixed, err := time.Parse(time.RFC3339, os.Getenv("READER_HISTORY_PROCESS_CLOCK"))
	if err != nil {
		t.Fatal(err)
	}
	der, err := base64.StdEncoding.DecodeString(os.Getenv("READER_HISTORY_PROCESS_CA"))
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := scholar.New(maxPaperBytes)
	url := os.Getenv("READER_HISTORY_PROCESS_SOURCE")
	client.ArXivAPI, client.SemanticScholar = url+"/query", url+"/batch"
	client.AllowPrivateHosts = true
	client.HTTP = &http.Client{Transport: transport, Timeout: 20 * time.Second}
	s.scholar = client
	m := s.Collection()
	m.clock = func() time.Time { return fixed }
	m.gate = scholar.NewSourceGate("process-fixture", time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	stop := m.StartCollection(ctx)
	defer func() { cancel(); stop() }()
	rounds, err := strconv.Atoi(os.Getenv("READER_HISTORY_PROCESS_ROUNDS"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < rounds; i++ {
		m.tickOnce(ctx)
		cycle, _, err := loadHotHistoryCycle(store)
		if err != nil {
			t.Fatal(err)
		}
		if cycle.Phase == "idle" {
			break
		}
	}
}

func TestCollectionHistoryCrashRestartAndClock(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultCollectionTaskConfig()
	cfg.KeywordGroups, cfg.HotWindowMonths = [][]string{{"detection"}}, 12
	task, err := store.SaveCollectionTask(CollectionTask{Name: "history", Priority: 2, Enabled: true, Config: cfg}, -1)
	if err != nil {
		store.DB.Close()
		t.Fatal(err)
	}
	if _, err = store.DB.Exec("UPDATE collection_tasks SET next_run_at='2099-01-01T00:00:00Z' WHERE task_id=?", task.ID); err != nil {
		store.DB.Close()
		t.Fatal(err)
	}
	store.DB.Close()
	fixed := time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	var block atomic.Bool
	block.Store(true)
	reached := make(chan struct{}, 1)
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/batch" {
			w.Write([]byte("[]"))
			return
		}
		if calls.Add(1) == 3 && block.Load() {
			reached <- struct{}{}
			<-r.Context().Done()
			return
		}
		w.Write([]byte(atomPage(0)))
	}))
	defer source.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	start := func(clock time.Time, rounds int) (*exec.Cmd, *bytes.Buffer, <-chan error) {
		t.Helper()
		cmd := exec.Command(executable, "-test.run=^TestCollectionHistoryProcessWorker$", "-test.timeout=30s")
		cmd.Env = append(os.Environ(), "READER_HISTORY_PROCESS_ROOT="+root, "READER_HISTORY_PROCESS_CLOCK="+clock.Format(time.RFC3339), "READER_HISTORY_PROCESS_SOURCE="+source.URL, "READER_HISTORY_PROCESS_CA="+base64.StdEncoding.EncodeToString(source.Certificate().Raw), "READER_HISTORY_PROCESS_ROUNDS="+strconv.Itoa(rounds))
		output := &bytes.Buffer{}
		cmd.Stdout, cmd.Stderr = output, output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		return cmd, output, done
	}
	wait := func(cmd *exec.Cmd, output *bytes.Buffer, done <-chan error) {
		t.Helper()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("worker failed: %v\n%s", err, output.String())
			}
		case <-time.After(35 * time.Second):
			cmd.Process.Kill()
			<-done
			t.Fatal("worker did not exit")
		}
	}
	read := func() hotHistoryCycle {
		t.Helper()
		s, err := OpenStore(root)
		if err != nil {
			t.Fatal(err)
		}
		defer s.DB.Close()
		cycle, _, err := loadHotHistoryCycle(s)
		if err != nil {
			t.Fatal(err)
		}
		return cycle
	}
	cmd, _, done := start(fixed, 1)
	select {
	case <-reached:
	case err := <-done:
		t.Fatalf("worker exited before barrier: %v", err)
	case <-time.After(25 * time.Second):
		cmd.Process.Kill()
		<-done
		t.Fatal("source barrier not reached")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-done
	interrupted := read()
	_, hot := PlanWindows(fixed, cfg.LatestWindowDays, cfg.HotWindowMonths)
	expected := len(weeklyHistoryRanges(hot.From, fixed.Add(-hotHistoryRecentZone)))
	if interrupted.Phase != "running" || len(interrupted.Pending) != expected-2 {
		t.Fatalf("lost crash checkpoint: %+v, expected %d", interrupted, expected-2)
	}
	block.Store(false)
	cmd, output, done := start(fixed, 1)
	wait(cmd, output, done)
	resumed := read()
	if len(resumed.Pending) != len(interrupted.Pending)-hotHistoryRoundBudget || resumed.WindowFrom != interrupted.WindowFrom || resumed.WindowTo != interrupted.WindowTo {
		t.Fatalf("restart rebuilt or overran queue: before=%d after=%d", len(interrupted.Pending), len(resumed.Pending))
	}
	store, err = OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	var abandoned int
	err = store.DB.QueryRow("SELECT count(*) FROM collection_runs WHERE status='interrupted' AND failure_class='startup'").Scan(&abandoned)
	store.DB.Close()
	if err != nil || abandoned != 1 {
		t.Fatalf("startup did not reconcile killed run: %d %v", abandoned, err)
	}
	cmd, output, done = start(fixed, 100)
	wait(cmd, output, done)
	completed := read()
	if completed.Phase != "idle" || len(completed.Pending) != 0 || completed.NextStartAt != fixed.Add(7*24*time.Hour).Format(time.RFC3339) {
		t.Fatalf("wrong completed schedule: %+v", completed)
	}
	before := calls.Load()
	cmd, output, done = start(fixed.Add(7*24*time.Hour-time.Second), 1)
	wait(cmd, output, done)
	if calls.Load() != before {
		t.Fatal("history restarted before its persisted deadline")
	}
	cmd, output, done = start(fixed.Add(7*24*time.Hour), 1)
	wait(cmd, output, done)
	if calls.Load()-before != hotHistoryRoundBudget || read().Phase != "running" {
		t.Fatal("history did not restart at its seven-day deadline")
	}
}
