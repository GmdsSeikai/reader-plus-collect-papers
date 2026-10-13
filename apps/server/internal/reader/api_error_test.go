package reader

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"reader.local/server/internal/diagnostics"
)

func TestProviderErrorsAreClassifiedAndRedacted(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	for _, tt := range []struct {
		url    string
		status int
		body   string
		kind   ErrorKind
		param  string
	}{
		{"https://api.openai.com/v1", 400, `{"error":{"type":"invalid_request_error","code":"unsupported_value","param":"reasoning_effort","message":"SECRET_KEY private paper"}}`, ErrorConfiguration, "reasoning_effort"},
		{"https://open.bigmodel.cn/api/paas/v4", 429, `{"error":{"code":"1113","message":"private paper"}}`, ErrorBilling, ""},
		{"https://open.bigmodel.cn/api/paas/v4", 429, `{"error":{"code":"1302"}}`, ErrorLimit, ""},
		{"https://api.openai.com/v1", 429, `{"error":{"code":"insufficient_quota","type":"invalid_request_error"}}`, ErrorBilling, ""},
		{"https://api.anthropic.com/v1", 200, `{"type":"error","error":{"type":"overloaded_error","message":"private paper"}}`, ErrorUpstream, ""},
		{"https://generativelanguage.googleapis.com/v1beta", 400, `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"Invalid thinking.type private paper"}}`, ErrorConfiguration, "thinking.type"},
		{"https://api.minimax.io/v1", 200, `{"base_resp":{"status_code":2013,"status_msg":"Invalid reasoning_effort private paper"}}`, ErrorConfiguration, "reasoning_effort"},
		{"https://relay.test/v1", 429, `{"error":{"code":"1113"}}`, ErrorLimit, ""},
		{"https://relay.test/v1", 502, `<html>SECRET_KEY private paper</html>`, ErrorUpstream, ""},
		{"https://relay.test/v1", 403, `{"error":{"type":"permission_denied_error"}}`, ErrorPermission, ""},
		{"https://relay.test/v1", 400, `{"error":{"code":"private-paper","param":"private-paper","type":"private-paper"}}`, ErrorConfiguration, ""},
	} {
		t.Run(fmt.Sprint(tt.status)+tt.body, func(t *testing.T) {
			c := APIConnection{URL: tt.url, Key: "SECRET_KEY"}
			ctx, _ := traceAPI(t.Context(), c, "model")
			err := providerFailure(ctx, c, tt.status, http.Header{"X-Request-Id": []string{"req_123"}}, []byte(tt.body), "http")
			if errorKind(err) != tt.kind {
				t.Fatalf("%v want %s", err, tt.kind)
			}
			if err.(*apiFailure).Param != tt.param {
				t.Fatal(err)
			}
			if strings.Contains(err.Error(), "SECRET_KEY") || strings.Contains(err.Error(), "private paper") {
				t.Fatal("error leaks content")
			}
		})
	}
	for _, secret := range []string{"SECRET_KEY", "private paper", "private-paper"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("log leaks", secret)
		}
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 11 {
		t.Fatal(len(lines))
	}
	for _, line := range lines {
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) != nil || event["apiRequestId"] == "" || event["upstreamRequestId"] != "req_123" {
			t.Fatal(line)
		}
	}
	// Exercise the real diagnostics writer's redaction too.
	redacted := string(diagnostics.RedactLog(logs.Bytes(), false, "SECRET_KEY"))
	if !strings.Contains(redacted, "unsupported_value") {
		t.Fatal("lost useful error code")
	}
}
func TestAPIErrorDoesNotRetryAndLogsEffort(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("X-Request-ID", "req_test")
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"code":"unsupported_parameter","param":"reasoning_effort","message":"SECRET_KEY private question"}}`)
	}))
	defer server.Close()
	ctx := context.WithValue(t.Context(), apiAttemptKey{}, "attempt_test")
	_, err := streamCompletion(ctx, APIConnection{URL: server.URL, Key: "SECRET_KEY"}, completionRequest{Model: "claude-custom", Effort: "max"}, AIInput{Prompt: "private question"}, nil)
	if calls != 1 || errorKind(err) != ErrorConfiguration {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	for _, value := range []string{`"requestedEffort":"max"`, `"sentEffort":"high"`, `"attemptId":"attempt_test"`, `"upstreamCode":"unsupported_parameter"`, `"upstreamRequestId":"req_test"`, `"stage":"http"`} {
		if !strings.Contains(logs.String(), value) {
			t.Fatal("missing diagnostic", value)
		}
	}
	for _, value := range []string{"SECRET_KEY", "private question", server.URL} {
		if strings.Contains(logs.String(), value) {
			t.Fatal("leaked", value)
		}
	}
}

func TestHTTP200BusinessErrorIsNotSuccess(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"error\":{\"code\":\"unsupported_parameter\",\"param\":\"reasoning_effort\"}}\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"error":{"code":"unsupported_parameter","param":"reasoning_effort"}}`)
				}
			}))
			defer upstream.Close()
			result, err := streamCompletion(t.Context(), APIConnection{URL: upstream.URL}, completionRequest{Model: "test", Effort: "high"}, AIInput{Prompt: "private"}, nil)
			if result.Text != "" || errorKind(err) != ErrorConfiguration {
				t.Fatalf("%+v %v", result, err)
			}
		})
	}
}
