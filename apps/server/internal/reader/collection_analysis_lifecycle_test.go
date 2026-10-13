package reader

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPaperAnalysisSettingsRejectLateResults(t *testing.T) {
	for _, kind := range []string{"disabled", "paused", "model-changed"} {
		t.Run(kind, func(t *testing.T) {
			s := testServer(t)
			candidate := seedAnalysisCandidate(t, s)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			calls := analysisAPIFixture(t, s, func(input AnalysisInput) string {
				close(entered)
				<-release
				return mustJSONText(fixtureAnalysisResult(input))
			})
			done := make(chan error, 1)
			go func() {
				_, err := s.Collection().AnalyzeCandidate(context.Background(), candidate.ArXivID, true)
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("native request not received")
			}
			cfg, err := s.Store.CollectionSettings()
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "disabled":
				cfg.AnalysisEnabled = false
			case "paused":
				cfg.Paused = true
			case "model-changed":
				cfg.AnalysisModel = "new-model"
			}
			response := request(t, s, "PUT", "/api/collection/settings", strings.NewReader(mustJSONText(cfg)))
			if response.Code != 200 {
				t.Fatalf("settings: %d %s", response.Code, response.Body.String())
			}
			unblock()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("obsolete or cancelled result reported success")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("analysis cancellation did not release its owner")
			}
			if calls.Load() != 1 {
				t.Fatal("settings cancellation caused a fallback or retry")
			}
			var count int
			if err := s.Store.DB.QueryRow("SELECT count(*) FROM collection_matches WHERE ai_score IS NOT NULL").Scan(&count); err != nil || count != 0 {
				t.Fatalf("late result projected score: %d %v", count, err)
			}
			if err := s.Store.DB.QueryRow("SELECT count(*) FROM collection_analyses WHERE status='succeeded'").Scan(&count); err != nil || count != 0 {
				t.Fatalf("late result saved success: %d %v", count, err)
			}
			var sent, reserved int
			if err := s.Store.DB.QueryRow("SELECT sent,reserved FROM collection_quota_days").Scan(&sent, &reserved); err != nil || sent != 1 || reserved != 0 {
				t.Fatalf("sent cancellation refunded quota: %d %d %v", sent, reserved, err)
			}
		})
	}
}
