package reader

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDrainLifecycleHTTP(t *testing.T) {
	s := testServer(t)
	w := request(t, s, "POST", "/api/lifecycle/drain", strings.NewReader("{}"))
	if w.Code != 202 {
		t.Fatalf("drain start = %d body=%s", w.Code, w.Body.String())
	}
	var start struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	if start.ID == "" {
		t.Fatal("drain id missing")
	}
	w = request(t, s, "GET", "/api/lifecycle/drain/"+start.ID, nil)
	var state struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Status != "drained" {
		t.Fatalf("idle drain status = %q, want drained", state.Status)
	}
	// A second drain request is idempotent while the first is still current.
	w2 := request(t, s, "POST", "/api/lifecycle/drain", strings.NewReader("{}"))
	if w2.Code != 202 {
		t.Fatalf("second drain = %d", w2.Code)
	}
}

func TestDrainRejectsNewUploads(t *testing.T) {
	s := testServer(t)
	w := request(t, s, "POST", "/api/lifecycle/drain", strings.NewReader("{}"))
	if w.Code != 202 {
		t.Fatalf("drain = %d", w.Code)
	}
	up := uploadTo(t, s, "papers", "paper.pdf", pdfBytes(t))
	if up.Code != 503 {
		t.Fatalf("upload during drain = %d body=%s", up.Code, up.Body.String())
	}
}

func TestDrainCancelRestoresService(t *testing.T) {
	s := testServer(t)
	w := request(t, s, "POST", "/api/lifecycle/drain", strings.NewReader("{}"))
	var start struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	w = request(t, s, "POST", "/api/lifecycle/drain/"+start.ID+"/cancel", nil)
	var cancelled struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &cancelled); err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != "cancelled" {
		t.Fatalf("cancel status = %q", cancelled.Status)
	}
	up := uploadTo(t, s, "papers", "paper.pdf", pdfBytes(t))
	if up.Code != 201 {
		t.Fatalf("upload after cancel = %d body=%s", up.Code, up.Body.String())
	}
}

func TestDrainWaitsForInflightUpload(t *testing.T) {
	s := testServer(t)
	// Hold the write gate as an in-flight import would, then drain.
	release, err := s.importGate.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	w := request(t, s, "POST", "/api/lifecycle/drain", strings.NewReader("{}"))
	var start struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	w = request(t, s, "GET", "/api/lifecycle/drain/"+start.ID, nil)
	var state struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Status == "drained" {
		t.Fatal("drain reported drained while a write was in flight")
	}
	release()
	w = request(t, s, "GET", "/api/lifecycle/drain/"+start.ID, nil)
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Status != "drained" {
		t.Fatalf("after release status = %q, want drained", state.Status)
	}
}

func TestLifecycleDrainRequiresAuth(t *testing.T) {
	s := testServer(t)
	r := httptest.NewRequest("POST", "http://127.0.0.1:17840/api/lifecycle/drain", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("unauthenticated drain = %d, want 401", w.Code)
	}
}

// TestDrainResumeWaitsForBackground (Q03) proves a cancelled drain reopens
// admission and restarts scheduling only after the old background stopped.
func TestDrainResumeWaitsForBackground(t *testing.T) {
	s := testServer(t)
	backgroundDone := make(chan struct{})
	started := make(chan struct{}, 1)
	s.SetDrainHooks(func() <-chan struct{} {
		return backgroundDone
	}, func() {
		started <- struct{}{}
	})
	// Hold a library write so the drain cannot finish early.
	release, err := s.importGate.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	w := request(t, s, "POST", "/api/lifecycle/drain", strings.NewReader("{}"))
	var start struct {
		ID string `json:"id"`
	}
	json.Unmarshal(w.Body.Bytes(), &start)
	w = request(t, s, "POST", "/api/lifecycle/drain/"+start.ID+"/cancel", nil)
	if w.Code != 200 {
		t.Fatalf("cancel = %d", w.Code)
	}
	release()
	// Background has not stopped yet: admission must stay closed.
	select {
	case <-started:
		t.Fatal("resume started before the old background stopped")
	case <-time.After(150 * time.Millisecond):
	}
	if _, err := s.importGate.Acquire(); err == nil {
		t.Fatal("admission reopened before the old background stopped")
	}
	close(backgroundDone)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("resume never started after the background stopped")
	}
	if _, err := s.importGate.Acquire(); err != nil {
		t.Fatalf("admission not reopened: %v", err)
	}
}

// TestStaleDrainResumeCannotReopenNewerDrain (Q03) proves an old cancelled
// intent never reopens the gate of a newer active drain.
func TestStaleDrainResumeCannotReopenNewerDrain(t *testing.T) {
	s := testServer(t)
	backgroundDone := make(chan struct{})
	s.SetDrainHooks(func() <-chan struct{} {
		return backgroundDone
	}, func() {})
	release, err := s.importGate.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	// Drain A is held open by the writer; cancel A, then start drain B.
	w := request(t, s, "POST", "/api/lifecycle/drain", strings.NewReader("{}"))
	var a struct {
		ID string `json:"id"`
	}
	json.Unmarshal(w.Body.Bytes(), &a)
	w = request(t, s, "POST", "/api/lifecycle/drain/"+a.ID+"/cancel", nil)
	if w.Code != 200 {
		t.Fatalf("cancel A = %d", w.Code)
	}
	w = request(t, s, "POST", "/api/lifecycle/drain", strings.NewReader("{}"))
	var b struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	json.Unmarshal(w.Body.Bytes(), &b)
	if b.ID == a.ID {
		t.Fatal("new drain reused the cancelled intent")
	}
	close(backgroundDone)
	release()
	// Give A's stale resume a chance to run, then check B still owns the gate.
	time.Sleep(150 * time.Millisecond)
	w = request(t, s, "GET", "/api/lifecycle/drain/"+b.ID, nil)
	var state struct {
		Status string `json:"status"`
	}
	json.Unmarshal(w.Body.Bytes(), &state)
	if state.Status != "drained" {
		t.Fatalf("drain B status = %q", state.Status)
	}
	if !s.importGate.Draining() {
		t.Fatal("stale resume reopened the gate of the newer drained intent")
	}
}
