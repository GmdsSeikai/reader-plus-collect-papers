package reader

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runAnalysisSessionProcess() {
	provider := os.Getenv("READER_AGENT_HELPER")
	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 4096), maxProviderFrame)
	enc := json.NewEncoder(os.Stdout)
	for scan.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scan.Bytes(), &req) != nil {
			os.Exit(2)
		}
		cwd, _ := os.Getwd()
		capture, err := os.OpenFile(os.Getenv("READER_ANALYSIS_CAPTURE"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(3)
		}
		json.NewEncoder(capture).Encode(map[string]any{"pid": os.Getpid(), "cwd": cwd, "argv": os.Args, "method": req.Method, "params": req.Params})
		capture.Close()
		reply := func(v any) { enc.Encode(map[string]any{"id": req.ID, "result": v}) }
		var prompt string
		switch req.Method {
		case "initialize":
			reply(map[string]any{"protocolVersion": 1})
		case "initialized":
			continue
		case "config/read":
			reply(map[string]any{"config": map[string]string{"model_provider": "fixture-provider"}})
		case "model/list":
			reply(map[string]any{"data": []any{map[string]any{"model": "test-model", "isDefault": true}}})
		case "thread/start":
			reply(map[string]any{"thread": map[string]string{"id": "analysis"}, "model": "test-model"})
		case "session/new":
			reply(map[string]any{"sessionId": "analysis", "models": map[string]string{"currentModelId": "test-model"}, "configOptions": []any{map[string]any{"id": "model", "currentValue": "test-model", "options": []any{map[string]string{"value": "test-model", "name": "Test"}}}}})
		case "session/set_config_option":
			reply(map[string]any{})
		case "turn/start":
			var p struct {
				Input []struct {
					Text string `json:"text"`
				} `json:"input"`
			}
			json.Unmarshal(req.Params, &p)
			if len(p.Input) == 0 {
				os.Exit(4)
			}
			prompt = p.Input[0].Text
		case "session/prompt":
			var p struct {
				Prompt []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			}
			json.Unmarshal(req.Params, &p)
			if len(p.Prompt) == 0 {
				os.Exit(4)
			}
			prompt = p.Prompt[0].Text
		default:
			if provider != "claude" {
				os.Exit(5)
			}
			if strings.Contains(string(scan.Bytes()), `"subtype":"initialize"`) {
				enc.Encode(map[string]any{"type": "control_response", "response": map[string]any{"request_id": "1", "subtype": "success", "response": map[string]any{"models": []any{map[string]string{"value": "test-model", "resolvedModel": "test-model"}}}}})
				continue
			}
			prompt = scan.Text()
		}
		if prompt == "" {
			continue
		}
		var input AnalysisInput
		if json.Unmarshal([]byte(prompt), &input) != nil || len(input.Tasks) == 0 {
			os.Exit(6)
		}
		answer := mustJSONText(fixtureAnalysisResult(input))
		switch provider {
		case "codex":
			reply(map[string]any{"turn": map[string]string{"id": "turn"}})
			enc.Encode(map[string]any{"method": "item/agentMessage/delta", "params": map[string]string{"threadId": "analysis", "turnId": "turn", "itemId": "answer", "delta": answer}})
			enc.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "analysis", "turnId": "turn", "turn": map[string]string{"id": "turn", "status": "completed"}}})
		case "kimi":
			enc.Encode(map[string]any{"method": "session/update", "params": map[string]any{"sessionId": "analysis", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": answer}}}})
			reply(map[string]string{"stopReason": "end_turn"})
		case "claude":
			enc.Encode(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]string{"type": "text_delta", "text": answer}}})
			enc.Encode(map[string]any{"type": "result", "subtype": "success", "result": answer})
		}
	}
}

func TestPaperAnalysisNativeCLIAdapters(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "kimi"} {
		t.Run(provider, func(t *testing.T) {
			fakeAgent(t, provider, "analysis-session")
			capture := filepath.Join(t.TempDir(), "analysis-requests.jsonl")
			t.Setenv("READER_ANALYSIS_CAPTURE", capture)
			s := testServer(t)
			candidate := seedAnalysisCandidate(t, s)
			ai := AIConfig{Primary: provider, Models: map[string]string{provider: "reader-chat"}, Capabilities: map[string]Capability{}}
			ai.Capabilities[provider] = Capability{Text: true, CheckedAt: "fixture", Fingerprint: configPrint(ai, provider)}
			if err := s.writeAIConfig(ai); err != nil {
				t.Fatal(err)
			}
			cfg := defaultCollectionSettings()
			cfg.AnalysisEnabled, cfg.AnalysisProvider, cfg.AnalysisModel = true, provider, "test-model"
			if err := s.Store.SaveCollectionSettings(cfg); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				job := submitAnalysisHTTP(t, s, candidate.ArXivID)
				if job := waitAnalysisJobHTTP(t, s, job.ID); job.Status != runSucceeded {
					t.Fatalf("native analysis: %+v", job)
				}
			}
			var sent int
			if err := s.Store.DB.QueryRow("SELECT sent FROM collection_quota_days").Scan(&sent); err != nil || sent != 1 {
				t.Fatalf("native metering/cache: %d %v", sent, err)
			}
			body, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			if provider == "codex" && (!strings.Contains(string(body), "fixture-provider.request_max_retries") || !strings.Contains(string(body), "fixture-provider.stream_max_retries")) {
				t.Fatal("native retry limits not applied to the resolved provider")
			}
			for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
				var record struct {
					CWD string `json:"cwd"`
				}
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				if filepath.Dir(filepath.Dir(record.CWD)) != s.Store.Root {
					t.Fatalf("CLI escaped its isolated workspace: %s", record.CWD)
				}
			}
		})
	}
}
