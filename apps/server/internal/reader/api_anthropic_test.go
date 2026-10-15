package reader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func anthropicTestWire(text string) string {
	return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		fmt.Sprintf("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", text) +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
}
func TestAnthropicStreamBoundaries(t *testing.T) {
	wire := anthropicTestWire("你好")
	for _, tt := range []struct {
		name, wire string
		kind       ErrorKind
	}{
		{"complete", wire, ""},
		{"missing-stop", strings.ReplaceAll(wire, "data: {\"type\":\"message_stop\"}\n\n", ""), ErrorProtocol},
		{"missing-reason", strings.ReplaceAll(wire, "\"end_turn\"", "null"), ErrorProtocol},
		{"empty", anthropicTestWire(""), ErrorProtocol},
		{"length", strings.ReplaceAll(wire, "end_turn", "max_tokens"), ErrorLimit},
		{"tool", strings.ReplaceAll(wire, "end_turn", "tool_use"), ErrorCapability},
		{"error-after-output", strings.ReplaceAll(wire, "data: {\"type\":\"message_stop\"}", "data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"private\"}}"), ErrorUpstream},
		{"text-without-start", strings.ReplaceAll(wire, "\"index\":0,\"delta\"", "\"index\":1,\"delta\""), ErrorProtocol},
		{"duplicate-start", strings.ReplaceAll(wire, "data: {\"type\":\"content_block_stop\",\"index\":0}", "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}"), ErrorProtocol},
	} {
		t.Run(tt.name, func(t *testing.T) {
			text := ""
			var tokens *TokenCounts
			result, err := readAnthropicStream(t.Context(), oneByteReader{strings.NewReader(tt.wire)}, APIConnection{}, func(e ProviderEvent) error {
				text += e.Text
				if len(e.Metrics) > 0 {
					tokens = e.Metrics[0].Tokens
				}
				return nil
			})
			if tt.kind == "" {
				if err != nil || result.Text != text || text != "你好" || tokens == nil || tokens.TotalTokens != 12 {
					t.Fatalf("%+v %v %+v", result, err, tokens)
				}
			} else if err == nil || errorKind(err) != tt.kind {
				t.Fatalf("%v want %s", err, tt.kind)
			}
		})
	}
}

func TestAnthropicThinkingIsNotAnswerText(t *testing.T) {
	thinking := "data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"private reasoning\"}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"private signature\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n"
	wire := strings.Replace(anthropicTestWire("answer"), "data: {\"type\":\"content_block_start\"", thinking+"data: {\"type\":\"content_block_start\"", 1)
	text := ""
	result, err := readAnthropicStream(t.Context(), strings.NewReader(wire), APIConnection{}, func(e ProviderEvent) error { text += e.Text; return nil })
	if err != nil || result.Text != "answer" || text != "answer" {
		t.Fatalf("%+v %v %s", result, err, text)
	}
}

func TestAnthropicDocumentRetrievalConversation(t *testing.T) {
	s, _ := prepareConversationFixture(t)
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			respond(w, 200, map[string]any{"data": []any{map[string]string{"id": "test"}}})
			return
		}
		calls++
		if r.URL.Path != "/messages" {
			t.Errorf("wrong protocol: %s", r.URL.Path)
		}
		var payload struct {
			System   string
			Messages []completionMessage
			Tools    []any
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if len(payload.Tools) > 0 || payload.System == "" {
			t.Error("invalid retrieval payload")
		}
		if !strings.Contains(jsonText(payload.Messages), "first-turn-marker") {
			t.Error("lost conversation history")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, anthropicTestWire("answer"))
	}))
	defer upstream.Close()
	configureDocumentAPI(t, s, upstream.URL)
	c := s.aiConfig()
	c.API.Protocol = "anthropic"
	passModels(&c, map[string]bool{"test": false})
	if err := s.writeAIConfig(c); err != nil {
		t.Fatal(err)
	}
	for _, question := range []string{"first-turn-marker", "继续"} {
		r := conversationRequest(t, s, "api", question)
		if strings.Count(r.Body.String(), "event: done") != 1 {
			t.Fatal(r.Body)
		}
	}
	if calls != 2 {
		t.Fatalf("unexpected capability calls: %d", calls)
	}
	var raw string
	if err := s.Store.DB.QueryRow("SELECT body FROM document_conversations WHERE document_id='doc'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var state documentConversation
	if err := json.Unmarshal([]byte(raw), &state); err != nil || len(state.Messages) != 5 {
		t.Fatalf("conversation not persisted: %v %d", err, len(state.Messages))
	}
}

func TestAnthropicAnalysisDispatch(t *testing.T) {
	for _, denied := range []bool{false, true} {
		name := "sent"
		if denied {
			name = "quota-denied"
		}
		t.Run(name, func(t *testing.T) {
			var requests, before, sent atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				var payload struct{ System string }
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if r.URL.Path != "/messages" || payload.System != "analysis-only" {
					t.Errorf("analysis request lost its contract: %s %+v", r.URL.Path, payload)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, anthropicTestWire("result"))
			}))
			defer upstream.Close()
			blocked := errors.New("quota denied")
			hook := &generationDispatch{
				Before: func(context.Context) error {
					before.Add(1)
					if denied {
						return blocked
					}
					return nil
				},
				Sent: func() error { sent.Add(1); return nil },
			}
			adapter := paperAnalysisAPIAdapter{APIConnection{URL: upstream.URL, Protocol: "anthropic", Key: "fixture"}, completionRequest{Model: "claude-sonnet-4-6"}}
			result, err := runAttempt(t.Context(), adapter, AIInput{System: "analysis-only", Prompt: "abstract", Dispatch: hook, NoRetry: true}, nil)
			if before.Load() != 1 {
				t.Fatalf("dispatch admission calls = %d", before.Load())
			}
			if denied {
				if !errors.Is(err, blocked) || requests.Load() != 0 || sent.Load() != 0 {
					t.Fatalf("denied request was sent: %v requests=%d sent=%d", err, requests.Load(), sent.Load())
				}
			} else if err != nil || result.Text != "result" || requests.Load() != 1 || sent.Load() != 1 {
				t.Fatalf("analysis dispatch = %+v %v requests=%d sent=%d", result, err, requests.Load(), sent.Load())
			}
		})
	}
}

func TestDocumentCompletionDispatchAdmission(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) }))
			defer server.Close()
			blocked := errors.New("RPM admission cancelled")
			var before atomic.Int32
			req := completionRequest{Model: "test", Dispatch: &generationDispatch{Before: func(context.Context) error { before.Add(1); return blocked }}}
			_, err := toolCompletion(t.Context(), APIConnection{URL: server.URL, Protocol: protocol}, req, []toolCompletionMessage{{Role: "user", Content: "question"}}, nil)
			if !errors.Is(err, blocked) || before.Load() != 1 || requests.Load() != 0 {
				t.Fatalf("document dispatch bypassed admission: %v before=%d requests=%d", err, before.Load(), requests.Load())
			}
		})
	}
}
