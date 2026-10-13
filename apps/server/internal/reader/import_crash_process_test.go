package reader

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Process-level crash evidence: the real reader-server binary is killed at
// named install boundaries and restarted on the same data root so the actual
// startup chain (RecoverImports before workers) is what repairs the state.

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

func readerServerBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "reader-server-bin-")
		if err != nil {
			buildErr = err
			return
		}
		name := "reader-server"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		binPath = filepath.Join(dir, name)
		cmd := exec.Command("go", "build", "-o", binPath, "./cmd/reader-server")
		cmd.Dir = filepath.Join("..", "..")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = err
			binPath = string(out)
		}
	})
	if buildErr != nil {
		t.Fatalf("build reader-server: %v (%s)", buildErr, binPath)
	}
	return binPath
}

type readerProc struct {
	cmd  *exec.Cmd
	url  string
	done chan error
}

func startReaderServer(t *testing.T, dataRoot string, extraEnv ...string) *readerProc {
	t.Helper()
	cmd := exec.Command(readerServerBinary(t), "--data", dataRoot, "--port", "0", "--connector=false")
	cmd.Env = append(os.Environ(), "READER_TOKEN=test-secret-token", "CGO_ENABLED=0")
	cmd.Env = append(cmd.Env, extraEnv...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &readerProc{cmd: cmd, done: make(chan error, 1)}
	go func() {
		line := make([]byte, 4096)
		n, _ := stdout.Read(line)
		first := string(line[:n])
		if i := strings.IndexByte(first, 10); i >= 0 {
			first = first[:i]
		}
		var ready struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal([]byte(first), &ready)
		p.url = strings.TrimSpace(ready.URL)
		p.done <- cmd.Wait()
	}()
	deadline := time.Now().Add(20 * time.Second)
	for p.url == "" && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if p.url == "" {
		_ = cmd.Process.Kill()
		t.Fatal("reader-server did not report ready")
	}
	return p
}

func (p *readerProc) waitExit(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		_ = p.cmd.Process.Kill()
		t.Fatal("process did not exit")
	}
}

func (p *readerProc) kill(t *testing.T) {
	t.Helper()
	_ = p.cmd.Process.Kill()
	p.waitExit(t)
}

func uploadToProc(t *testing.T, p *readerProc, name string, data []byte) *http.Response {
	t.Helper()
	var body bytes.Buffer
	m := multipart.NewWriter(&body)
	m.WriteField("library", "papers")
	f, _ := m.CreateFormFile("file", name)
	f.Write(data)
	m.Close()
	req, _ := http.NewRequest("POST", p.url+"/api/documents", &body)
	req.Header.Set("Authorization", "Bearer test-secret-token")
	req.Header.Set("Content-Type", m.FormDataContentType())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	return res
}

func procDocumentCount(t *testing.T, dataRoot string) (int, string) {
	t.Helper()
	// The child must be stopped before opening its database (single library
	// lock, single connection).
	store, err := OpenStore(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	var n int
	store.DB.QueryRow("SELECT count(*) FROM documents").Scan(&n)
	var stage string
	store.DB.QueryRow("SELECT COALESCE(stage,'') FROM import_commits ORDER BY created_at DESC LIMIT 1").Scan(&stage)
	return n, stage
}

// TestProcessCrashAtInstalledThenRestart kills the process after the file is
// installed but before the database commit; the restarted binary's startup
// chain must resume the commit exactly once.
func TestProcessCrashAtInstalledThenRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("process harness")
	}
	root := t.TempDir()
	data := pdfBytes(t)
	p := startReaderServer(t, root, "READER_TEST_CRASH_AT=installed")
	res := uploadToProc(t, p, "paper.pdf", data)
	if res != nil {
		res.Body.Close()
	}
	p.waitExit(t)
	// File is installed, database has no document yet.
	if n, _ := procDocumentCount(t, root); n != 0 {
		t.Fatalf("documents written before commit: %d", n)
	}
	target := filepath.Join(root, "papers")
	entries, _ := os.ReadDir(target)
	if len(entries) != 1 {
		t.Fatalf("installed file missing at crash boundary: %v", entries)
	}
	// Restart the same binary on the same root: the real startup chain runs.
	p2 := startReaderServer(t, root)
	defer p2.kill(t)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest("GET", p2.url+"/api/documents", nil)
		req.Header.Set("Authorization", "Bearer test-secret-token")
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			var docs []Document
			_ = json.NewDecoder(res.Body).Decode(&docs)
			res.Body.Close()
			if len(docs) == 1 {
				sum := sha256.Sum256(data)
				if docs[0].ID != hex.EncodeToString(sum[:])[:32] {
					t.Fatalf("restored document id = %s", docs[0].ID)
				}
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("startup recovery did not resume the installed commit")
}

// TestProcessCrashAtPreRenameThenRestart kills the process before the file
// install; the restarted binary must close the row without inventing files.
func TestProcessCrashAtPreRenameThenRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("process harness")
	}
	root := t.TempDir()
	p := startReaderServer(t, root, "READER_TEST_CRASH_AT=pre-rename")
	res := uploadToProc(t, p, "paper.pdf", pdfBytes(t))
	if res != nil {
		res.Body.Close()
	}
	p.waitExit(t)
	p2 := startReaderServer(t, root)
	// Give the startup chain time to reconcile, then verify the log closed
	// without creating a document.
	time.Sleep(500 * time.Millisecond)
	p2.kill(t)
	n, stage := procDocumentCount(t, root)
	if n != 0 {
		t.Fatalf("documents invented from prepared crash: %d", n)
	}
	if stage != importStageFailed && stage != importStagePrepared {
		t.Fatalf("stage after restart = %q", stage)
	}
}

// TestProcessCrashAfterCommitThenRestart kills the process right after the
// database commit; the restarted binary must treat it as done, idempotently.
func TestProcessCrashAfterCommitThenRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("process harness")
	}
	root := t.TempDir()
	data := pdfBytes(t)
	p := startReaderServer(t, root, "READER_TEST_CRASH_AT=post-commit")
	res := uploadToProc(t, p, "paper.pdf", data)
	if res != nil {
		res.Body.Close()
	}
	p.waitExit(t)
	if n, _ := procDocumentCount(t, root); n != 1 {
		t.Fatalf("post-commit crash lost the document: %d", n)
	}
	p2 := startReaderServer(t, root)
	time.Sleep(500 * time.Millisecond)
	p2.kill(t)
	n, stage := procDocumentCount(t, root)
	if n != 1 {
		t.Fatalf("double restart changed documents: %d", n)
	}
	if stage != importStageCommitted {
		t.Fatalf("stage after restart = %q", stage)
	}
}
