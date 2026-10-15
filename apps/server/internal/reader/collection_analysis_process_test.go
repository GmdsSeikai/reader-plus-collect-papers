package reader

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestPaperAnalysisProcessWorker(t *testing.T) {
	root := os.Getenv("READER_ANALYSIS_PROCESS_ROOT")
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
	s := NewServer(store, "analysis-process-fixture", "")
	defer s.Close()
	if err := s.RecoverImports(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("READER_ANALYSIS_PROCESS_RECOVER") == "1" {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	testCrashEnabled = "true"
	stop := s.Collection().StartCollection(ctx)
	defer func() { cancel(); stop() }()
	if _, err := s.Collection().submitAnalysis("2301.12345"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(15 * time.Second):
	}
	t.Fatal("controlled crash boundary was not reached")
}

func TestPaperAnalysisProcessCrashMetering(t *testing.T) {
	for _, point := range []string{"analysis-dispatching", "analysis-sent"} {
		t.Run(point, func(t *testing.T) {
			s := testServer(t)
			seedAnalysisCandidate(t, s)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/models" {
					respond(w, 200, map[string]any{"data": []any{map[string]string{"id": "test-model"}}})
					return
				}
				<-r.Context().Done()
			}))
			defer upstream.Close()
			ai := AIConfig{Primary: "api", API: APIConnection{URL: upstream.URL + "/v1", Key: "process-fixture-key"}, Models: map[string]string{}, APIModels: []string{"test-model"}}
			passModels(&ai, map[string]bool{"test-model": false})
			if err := s.writeAIConfig(ai); err != nil {
				t.Fatal(err)
			}
			cfg := defaultCollectionSettings()
			cfg.AnalysisEnabled, cfg.AnalysisProvider, cfg.AnalysisModel, cfg.DailyLimit = true, "api", "test-model", 1
			if err := s.Store.SaveCollectionSettings(cfg); err != nil {
				t.Fatal(err)
			}
			root := s.Store.Root
			// The parent closes its isolated DB before the child takes the real
			// library lock. No user process, source or account is involved.
			s.Store.DB.Close()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			run := func(recover bool) {
				t.Helper()
				cmd := exec.Command(executable, "-test.run=^TestPaperAnalysisProcessWorker$", "-test.timeout=20s")
				cmd.Env = append(os.Environ(), "READER_ANALYSIS_PROCESS_ROOT="+root, "READER_TEST_CRASH_AT="+point)
				if recover {
					cmd.Env = append(cmd.Env, "READER_ANALYSIS_PROCESS_RECOVER=1")
				}
				output, diagnostics := &bytes.Buffer{}, &bytes.Buffer{}
				cmd.Stdout, cmd.Stderr = output, diagnostics
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- cmd.Wait() }()
				select {
				case err := <-done:
					if recover && err != nil {
						t.Fatalf("startup failed: %v %s %s", err, output.String(), diagnostics.String())
					}
					if !recover && (err == nil || cmd.ProcessState.ExitCode() != 1 || output.Len() != 0) {
						t.Fatalf("wrong abrupt exit: %v %s %s", err, output.String(), diagnostics.String())
					}
				case <-time.After(25 * time.Second):
					cmd.Process.Kill()
					<-done
					t.Fatal("owned child did not stop")
				}
			}
			run(false)
			run(true)
			run(true)
			store, err := OpenStore(root)
			if err != nil {
				t.Fatal(err)
			}
			defer store.DB.Close()
			var sent, reserved, uncertain int
			if err := store.DB.QueryRow("SELECT sent,reserved,uncertain FROM collection_quota_days").Scan(&sent, &reserved, &uncertain); err != nil {
				t.Fatal(err)
			}
			wantUncertain := 0
			if point == "analysis-dispatching" {
				wantUncertain = 1
			}
			if sent != 1 || reserved != 0 || uncertain != wantUncertain {
				t.Fatalf("restart refunded or duplicated quota: %d %d %d", sent, reserved, uncertain)
			}
			var active int
			if err := store.DB.QueryRow("SELECT count(*) FROM collection_analysis_jobs WHERE status='interrupted' AND finished_at IS NOT NULL").Scan(&active); err != nil || active != 1 {
				t.Fatalf("restart lost interrupted persistent job: %d %v", active, err)
			}
			if err := store.DB.QueryRow("SELECT count(*) FROM collection_attempts WHERE status IN ('reserved','dispatching','sent')").Scan(&active); err != nil || active != 0 {
				t.Fatalf("restart left open attempts: %d %v", active, err)
			}
			if ok, err := store.ReserveAnalysisAttempt("post-restart", "req", "cache", utcDay(time.Now()), 1); err != nil || ok {
				t.Fatalf("restart enabled excess request: %v %v", ok, err)
			}
		})
	}
}
