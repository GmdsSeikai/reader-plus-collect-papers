package reader

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"strings"
	"unicode/utf8"
)

type completionToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type toolCompletionMessage struct {
	Reasoning string               `json:"reasoning_content,omitempty"`
	Role      string               `json:"role"`
	Content   any                  `json:"content"`
	Calls     []completionToolCall `json:"tool_calls,omitempty"`
	CallID    string               `json:"tool_call_id,omitempty"`
}
type toolCompletionResult struct {
	Reasoning string
	Text      string
	Calls     []completionToolCall
	Model     string
	Tokens    *TokenCounts
}

func documentAPITools() []any {
	var tools []any
	for _, d := range documentToolDefinitions {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": d.Name, "description": d.Description, "parameters": d.Schema}})
	}
	return tools
}

// Tool arguments are assembled completely before execution. This decoder never
// emits assistant text: tool-round prose is not the final answer.
func readToolCompletion(ctx context.Context, r io.Reader) (toolCompletionResult, error) {
	var result toolCompletionResult
	calls := map[int]*completionToolCall{}
	finish := ""
	protocol := func() (toolCompletionResult, error) {
		return result, generationError(ErrorProtocol, "API 工具流无效或缺少成功终态")
	}
	dispatch := func(data string) (bool, error) {
		if data == "" {
			return false, nil
		}
		if data == "[DONE]" {
			if finish == "" {
				return false, generationError(ErrorProtocol, "API 工具流缺少结束原因")
			}
			return true, nil
		}
		if !utf8.ValidString(data) {
			return false, generationError(ErrorProtocol, "API 工具流 UTF-8 无效")
		}
		var chunk struct {
			Error   json.RawMessage  `json:"error"`
			Model   string           `json:"model"`
			Usage   *completionUsage `json:"usage"`
			Choices []struct {
				Index *int `json:"index"`
				Delta struct {
					Reasoning *string         `json:"reasoning_content"`
					Content   *string         `json:"content"`
					Refusal   *string         `json:"refusal"`
					Legacy    json.RawMessage `json:"function_call"`
					Calls     []struct {
						Index    *int   `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				Finish *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			return false, generationError(ErrorProtocol, "API 工具事件无效")
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			return false, generationError(ErrorUpstream, "API 工具请求失败")
		}
		if chunk.Model != "" {
			result.Model = chunk.Model
		}
		if chunk.Usage != nil {
			result.Tokens = completionTokens(chunk.Usage)
		}
		selected := false
		for _, choice := range chunk.Choices {
			if choice.Index == nil {
				return false, generationError(ErrorProtocol, "API 候选编号缺失")
			}
			if *choice.Index != 0 {
				continue
			}
			if selected || finish != "" {
				return false, generationError(ErrorProtocol, "API 重复或迟到工具事件")
			}
			selected = true
			if choice.Delta.Refusal != nil && *choice.Delta.Refusal != "" {
				return false, generationError(ErrorUpstream, "API 拒绝读取")
			}
			if len(choice.Delta.Legacy) > 0 && string(choice.Delta.Legacy) != "null" {
				return false, generationError(ErrorCapability, "API 仅支持旧式函数协议")
			}
			if choice.Delta.Content != nil {
				result.Text += *choice.Delta.Content
				if len(result.Text) > 64<<10 {
					return false, generationError(ErrorLimit, "API 中间说明过长")
				}
			}
			if choice.Delta.Reasoning != nil {
				result.Reasoning += *choice.Delta.Reasoning
				if len(result.Reasoning) > 256<<10 {
					return false, generationError(ErrorLimit, "API 续接信息超过限制")
				}
			}
			for _, part := range choice.Delta.Calls {
				if part.Index == nil || *part.Index < 0 || *part.Index >= 16 {
					return false, generationError(ErrorProtocol, "API 工具编号无效")
				}
				i := *part.Index
				c := calls[i]
				if c == nil {
					c = &completionToolCall{}
					calls[i] = c
				}
				if part.ID != "" {
					if c.ID != "" && c.ID != part.ID {
						return false, generationError(ErrorProtocol, "API 工具调用 ID 改变")
					}
					c.ID = part.ID
				}
				if part.Type != "" {
					if part.Type != "function" {
						return false, generationError(ErrorProtocol, "API 工具类型无效")
					}
					c.Type = part.Type
				}
				c.Function.Name += part.Function.Name
				c.Function.Arguments += part.Function.Arguments
				if len(c.Function.Name) > 100 || len(c.Function.Arguments) > 4096 {
					return false, generationError(ErrorLimit, "API 工具参数过长")
				}
			}
			if choice.Finish != nil {
				if *choice.Finish != "tool_calls" && *choice.Finish != "stop" {
					return false, generationError(ErrorProtocol, "API 工具请求未正常结束")
				}
				finish = *choice.Finish
			}
		}
		return false, nil
	}
	if err := consumeCompletionFrames(ctx, r, dispatch); err != nil {
		return result, err
	}
	if finish == "stop" && len(calls) != 0 || finish == "tool_calls" && len(calls) == 0 {
		return protocol()
	}
	seen := map[string]bool{}
	for i := 0; i < len(calls); i++ {
		c := calls[i]
		if c == nil || c.ID == "" || seen[c.ID] || c.Type != "function" || c.Function.Name == "" || !json.Valid([]byte(c.Function.Arguments)) {
			return protocol()
		}
		seen[c.ID] = true
		result.Calls = append(result.Calls, *c)
	}
	return result, nil
}
func toolCompletion(ctx context.Context, c APIConnection, req completionRequest, messages []toolCompletionMessage, tools []any) (toolCompletionResult, error) {
	payload := map[string]any{"model": req.Model, "messages": messages, "stream": true, "n": 1, "stream_options": map[string]bool{"include_usage": true}}
	if tools != nil {
		payload["tools"] = tools
	}
	if req.Effort != "" {
		payload["reasoning_effort"] = req.Effort
		if strings.HasPrefix(strings.ToLower(req.Model), "deepseek") {
			payload["thinking"] = map[string]string{"type": "enabled"}
		}
	}
	body, _ := json.Marshal(payload)
	if len(body) > 512<<10 {
		return toolCompletionResult{}, generationError(ErrorLimit, "API 工具上下文超过限制")
	}
	response, err := apiRequest(ctx, c, "POST", "/chat/completions", body)
	if err != nil {
		return toolCompletionResult{}, err
	}
	defer response.Body.Close()
	media, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if media != "text/event-stream" {
		return toolCompletionResult{}, generationError(ErrorProtocol, "API 未返回工具调用 SSE")
	}
	return readToolCompletion(ctx, response.Body)
}
func streamAPIDocumentChat(ctx context.Context, c APIConnection, req completionRequest, in AIInput, emit func(ProviderEvent) error) (GenerateResult, error) {
	agent := in.Chat.Agent
	provider := in.UsageProvider
	if provider == "" {
		provider = "api"
	}
	agent.resetAttempt(provider)
	agent.Mode = "retrieval"
	if req.Tools {
		agent.Mode = "agent"
		messages := []toolCompletionMessage{{Role: "system", Content: documentReadingInstructions}, {Role: "user", Content: documentReadingPrompt(in)}}
		seenCalls := map[string]bool{}
		complete := false
		for round := 0; round < 8; round++ {
			var response toolCompletionResult
			_, err := trackModelUsage(UsageCall{Provider: provider}, in.UsageSink, nil, func(send func(ProviderEvent) error) (GenerateResult, error) {
				var e error
				response, e = toolCompletion(ctx, c, req, messages, documentAPITools())
				metricErr := send(ProviderEvent{Metrics: []ModelTokens{{Model: response.Model, Tokens: response.Tokens}}})
				if e == nil {
					e = metricErr
				}
				return GenerateResult{}, e
			})
			if err != nil {
				return GenerateResult{}, err
			}
			if len(response.Calls) == 0 {
				complete = true
				break
			}
			messages = append(messages, toolCompletionMessage{Role: "assistant", Content: response.Text, Calls: response.Calls, Reasoning: response.Reasoning})
			for _, call := range response.Calls {
				if seenCalls[call.ID] {
					return GenerateResult{}, generationError(ErrorProtocol, "API 重复使用工具调用 ID")
				}
				seenCalls[call.ID] = true
				raw, e := agent.call(ctx, call.Function.Name, json.RawMessage(call.Function.Arguments))
				if e != nil {
					return GenerateResult{}, e
				}
				messages = append(messages, toolCompletionMessage{Role: "tool", Content: string(raw), CallID: call.ID})
			}
		}
		if !complete {
			return GenerateResult{}, generationError(ErrorLimit, "API 自主阅读达到轮次上限")
		}
	} else {
		if agent.activity != nil {
			if err := agent.activity("reading", "pre_retrieval"); err != nil {
				return GenerateResult{}, err
			}
		}
		if err := agent.preRetrieve(ctx, in.Chat.Question); err != nil {
			return GenerateResult{}, err
		}
	}
	if err := agent.check(ctx); err != nil {
		return GenerateResult{}, err
	}
	if agent.activity != nil {
		if err := agent.activity("writing", ""); err != nil {
			return GenerateResult{}, err
		}
	}
	return trackModelUsage(UsageCall{Provider: provider}, in.UsageSink, emit, func(send func(ProviderEvent) error) (GenerateResult, error) {
		return streamCompletion(ctx, c, req, agent.finalInput(in), send)
	})
}
func (a *documentAgent) preRetrieve(ctx context.Context, question string) error {
	raw, e := a.call(ctx, "search_document", json.RawMessage(jsonText(map[string]any{"query": question, "limit": 4})))
	if e != nil {
		return e
	}
	var found struct {
		Items []struct {
			ID string `json:"blockId"`
		} `json:"items"`
	}
	json.Unmarshal(raw, &found)
	for _, p := range found.Items {
		if _, e = a.call(ctx, "read_passage", json.RawMessage(jsonText(map[string]any{"blockId": p.ID, "before": 1, "after": 1}))); e != nil {
			return e
		}
	}
	return nil
}
