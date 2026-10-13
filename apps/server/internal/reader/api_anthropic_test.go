package reader

import (
	"fmt"
	"strings"
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
