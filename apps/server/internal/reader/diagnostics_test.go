package reader

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"reader.local/server/internal/diagnostics"
)

func TestDiagnosticsAuthenticationAndRequestCorrelation(t *testing.T) {
	s := testServer(t)
	logs, _ := diagnostics.New(filepath.Join(s.Store.Root, "logs"), "session", "test-secret")
	s.SetDiagnostics(logs)
	denied := httptest.NewRecorder()
	s.Handler().ServeHTTP(denied, httptest.NewRequest("GET", "http://127.0.0.1:17840/api/diagnostics/export", nil))
	if denied.Code != 401 {
		t.Fatal(denied.Code)
	}
	first := request(t, s, "GET", "/api/documents", nil)
	rid := first.Header().Get("X-Reader-Request-ID")
	if len(rid) != 32 {
		t.Fatal("missing request ID")
	}
	w := request(t, s, "POST", "/api/diagnostics/events", strings.NewReader(`{"level":"info","event":"api.finish","requestId":"`+rid+`","status":200}`))
	if w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	w = request(t, s, "POST", "/api/diagnostics/events", strings.NewReader(`{"level":"error","event":"bad","body":"PRIVATE"}`))
	if w.Code != 400 {
		t.Fatal("accepted unknown body")
	}
	bundle := request(t, s, "GET", "/api/diagnostics/export", nil)
	if bundle.Code != 200 {
		t.Fatal(bundle.Body.String())
	}
	z, err := zip.NewReader(bytes.NewReader(bundle.Body.Bytes()), int64(bundle.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, f := range z.File {
		r, _ := f.Open()
		b, _ := io.ReadAll(r)
		r.Close()
		all.Write(b)
	}
	if !strings.Contains(all.String(), rid) || !strings.Contains(all.String(), "renderer.api.finish") || strings.Contains(all.String(), "test-secret") {
		t.Fatal("missing correlation or leaked credential")
	}
}

func TestDiagnosticsWrapperPreservesFlushAndCancellation(t *testing.T) {
	s := testServer(t)
	recorder := httptest.NewRecorder()
	s.diagnosticHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requestID(r.Context()) == "" {
			t.Fatal("missing context ID")
		}
		io.WriteString(w, "event: delta\ndata: {}\n\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Fatal(err)
		}
	})).ServeHTTP(recorder, httptest.NewRequest("GET", "http://localhost/stream", nil))
	if !recorder.Flushed || !strings.Contains(recorder.Body.String(), "delta") {
		t.Fatal("stream wrapper broke Flush")
	}
}
