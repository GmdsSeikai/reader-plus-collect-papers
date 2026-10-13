package reader

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func probeAPIDocumentTools(ctx context.Context, c APIConnection, model string) (string, error) {
	req := completionRequest{Model: model}
	tool := map[string]any{"type": "function", "function": map[string]any{"name": "reader_probe", "description": "Get the hidden random nonce. Call this to obtain it.", "parameters": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}}
	messages := []toolCompletionMessage{{Role: "user", Content: "Call reader_probe once, then reply with its exact returned nonce. Do not guess."}}
	result, e := toolCompletion(ctx, c, req, messages, []any{tool})
	if e != nil {
		return "failed", e
	}
	if len(result.Calls) != 1 || result.Calls[0].Function.Name != "reader_probe" || strings.TrimSpace(result.Calls[0].Function.Arguments) != "{}" {
		return "unsupported", fmt.Errorf("模型未返回合法工具调用")
	}
	nonce := "reader-" + id()
	messages = append(messages, toolCompletionMessage{Role: "assistant", Content: result.Text, Calls: result.Calls, Reasoning: result.Reasoning}, toolCompletionMessage{Role: "tool", CallID: result.Calls[0].ID, Content: nonce})
	result, e = toolCompletion(ctx, c, req, messages, nil)
	if e != nil {
		return "failed", e
	}
	if len(result.Calls) != 0 || !strings.Contains(result.Text, nonce) {
		return "unsupported", fmt.Errorf("模型未根据工具结果完成回答")
	}
	return "passed", nil
}
func (s *Server) testDocumentToolConnection(w http.ResponseWriter, r *http.Request, key string) {
	model, ok := strings.CutPrefix(key, "api:")
	if !ok {
		fail(w, 400, "当前工具检测仅支持 API 模型")
		return
	}
	c := s.aiConfig()
	if !capable(c, key, false) {
		fail(w, 409, "请先完成文本检测")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	status, e := probeAPIDocumentTools(ctx, c.API, model)
	if r.Context().Err() != nil {
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	latest := s.readAIConfig()
	if configPrint(latest, key) != configPrint(c, key) || latest.Capabilities[key].CheckedAt != c.Capabilities[key].CheckedAt {
		fail(w, 409, "配置已改变，请重新检测")
		return
	}
	cap := latest.Capabilities[key]
	cap.Tools = status
	cap.ToolSchemaVersion = documentToolSchemaVersion
	cap.ToolError = ""
	if e != nil {
		cap.ToolError = e.Error()
	}
	cap.CheckedAt = now()
	latest.Capabilities[key] = cap
	if e = s.writeAIConfig(latest); e != nil {
		fail(w, 500, "无法保存工具检测结果")
		return
	}
	respond(w, 200, cap)
}
