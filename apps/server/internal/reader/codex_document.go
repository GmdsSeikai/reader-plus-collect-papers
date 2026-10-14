package reader

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Raw effective configuration is discarded, never logged or persisted.
func denyCodexClientRequest(life *rpcLifecycle) func(codexRPCMessage) error {
	return func(msg codexRPCMessage) error {
		if len(msg.ID) > 0 && msg.Method != "" {
			return life.send(map[string]any{"id": msg.ID, "error": map[string]any{"code": -32601, "message": "Reader does not expose this operation"}})
		}
		return nil
	}
}
func (s *codexSession) control(ctx context.Context, method string, params any) (json.RawMessage, error) {
	life := newRPCLifecycle(ctx, s.cmd, s.stdin, s.stdout)
	defer life.finish()
	result, err := s.callRPC(ctx, life, method, params, denyCodexClientRequest(life))
	if errors.Is(err, errCodexRPCResponse) {
		return nil, generationError(ErrorCapability, "当前 Codex 版本不支持文档工具隔离")
	}
	return result, err
}
func (s *codexSession) initializeControl(ctx context.Context) error {
	life := newRPCLifecycle(ctx, s.cmd, s.stdin, s.stdout)
	defer life.finish()
	err := s.initializeRPC(ctx, life, denyCodexClientRequest(life))
	if errors.Is(err, errCodexRPCResponse) {
		return generationError(ErrorCapability, "当前 Codex 版本不支持文档工具隔离")
	}
	return err
}

type codexMCPConfig struct {
	Enabled *bool  `json:"enabled"`
	URL     string `json:"url"`
}

func (s *codexSession) documentConfig(ctx context.Context) (map[string]codexMCPConfig, error) {
	raw, e := s.control(ctx, "config/read", map[string]any{"includeLayers": false, "cwd": s.work})
	if e != nil {
		return nil, e
	}
	s.contextWindow = codexConfiguredWindow(raw)
	var result struct {
		Config struct {
			Servers  map[string]codexMCPConfig `json:"mcp_servers"`
			Features map[string]bool           `json:"features"`
		} `json:"config"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Config.Servers == nil {
		return nil, generationError(ErrorProtocol, "Codex MCP 配置格式不兼容")
	}
	for _, name := range []string{"shell_tool", "unified_exec", "plugins", "hooks", "apps", "multi_agent", "code_mode", "view_image", "image_generation"} {
		if result.Config.Features[name] {
			return nil, generationError(ErrorCapability, "Codex 无关能力未关闭")
		}
	}
	return result.Config.Servers, nil
}
func (s *codexSession) verifyDocumentConfig(ctx context.Context) (map[string]codexMCPConfig, error) {
	servers, e := s.documentConfig(ctx)
	if e != nil {
		return nil, e
	}
	found := false
	for name, c := range servers {
		if name == s.documentMCPName && s.documentMCP != "" {
			if c.URL != s.documentMCP || c.Enabled == nil || !*c.Enabled {
				return nil, generationError(ErrorCapability, "Reader MCP 配置不符")
			}
			found = true
			continue
		}
		if c.Enabled == nil || *c.Enabled {
			return nil, generationError(ErrorCapability, "Codex 配置已变化，存在无关 MCP，请重试")
		}
	}
	if s.documentMCP != "" && !found {
		return nil, generationError(ErrorCapability, "Codex 未加载文档工具")
	}
	return servers, nil
}
func isolatedDocumentCodex(ctx context.Context, work, model, url, token string) (*codexSession, error) {
	probe, e := newCodexSession(work, model)
	if e != nil {
		return nil, e
	}
	if e = probe.initializeControl(ctx); e != nil {
		probe.close()
		return nil, e
	}
	servers, e := probe.documentConfig(ctx)
	probe.close()
	if e != nil {
		return nil, e
	}
	names := []string{}
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	// Keep the native harness from truncating Reader's bounded range results.
	overrides := []string{"tool_output_token_limit=24000"}
	readerName := "reader_document"
	if _, collision := servers[readerName]; collision {
		readerName += "_" + id()[:8]
	}
	for _, name := range names {
		if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(name) {
			return nil, generationError(ErrorCapability, "当前 Codex 配置含不支持的 MCP 名称，无法安全覆盖")
		}
		overrides = append(overrides, "mcp_servers."+name+".enabled=false")
	}
	if url != "" {
		prefix := "mcp_servers." + readerName + "."
		overrides = append(overrides, prefix+"enabled=true", prefix+"url="+strconv.Quote(url), prefix+"bearer_token_env_var=\"READER_DOCUMENT_MCP_TOKEN\"", prefix+"enabled_tools=[\"get_outline\",\"search_document\",\"read_passage\"]", prefix+"required=true", prefix+"startup_timeout_sec=15", prefix+"tool_timeout_sec=15")
	}
	session, e := startCodexSession(work, model, overrides, token)
	if e != nil {
		return nil, e
	}
	session.documentMCP = url
	session.documentMCPName = readerName
	session.baseInstructions = readerSystemPrompt
	if e = session.initializeControl(ctx); e == nil {
		_, e = session.verifyDocumentConfig(ctx)
	}
	if e != nil {
		session.close()
		return nil, e
	}
	return session, nil
}
func validDocumentMCPStatus(raw json.RawMessage, expected string) bool {
	var result struct {
		Data []struct {
			Name  string                     `json:"name"`
			Tools map[string]json.RawMessage `json:"tools"`
		} `json:"data"`
		Next json.RawMessage `json:"nextCursor"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return false
	}
	if len(result.Next) > 0 && string(result.Next) != "null" {
		return false
	}
	var tools map[string]json.RawMessage
	for _, server := range result.Data {
		if server.Name == expected {
			tools = server.Tools
		} else if len(server.Tools) != 0 {
			return false
		}
	}
	if len(tools) != 3 {
		return false
	}
	for _, d := range documentToolDefinitions {
		found := false
		for name := range tools {
			if name == d.Name || strings.HasSuffix(name, "__"+d.Name) {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func streamCodexDocumentChat(ctx context.Context, root, model string, in AIInput, emit func(ProviderEvent) error) (GenerateResult, error) {
	a := in.Chat.Agent
	a.resetAttempt("codex")
	a.Mode = "agent"
	if err := a.prepareReading(ctx, in); err != nil {
		return GenerateResult{}, err
	}
	readingContext, stopReading := documentReadingContext(ctx)
	defer stopReading()
	url, token, closeMCP, e := serveDocumentMCP(a)
	if e != nil {
		return GenerateResult{}, e
	}
	defer closeMCP()
	work, e := os.MkdirTemp(filepath.Join(root, "ai-work"), "document-chat-")
	if e != nil {
		return GenerateResult{}, e
	}
	defer os.RemoveAll(work)
	reader, e := isolatedDocumentCodex(readingContext, work, model, url, token)
	if e != nil {
		return GenerateResult{}, e
	}
	a.updateContextWindow(reader.contextWindow, 0, "codex_config")
	reader.baseInstructions = documentReadingInstructions
	a.mu.Lock()
	a.abort = stopReading
	a.mu.Unlock()
	readingInput := in
	readingInput.ContextInfo = func(window, used int) { a.updateContextWindow(window, used, "codex_runtime") }
	readingInput.Prompt = documentReadingPrompt(in)
	_, e = trackModelUsage(UsageCall{Provider: "codex"}, in.UsageSink, nil, func(send func(ProviderEvent) error) (GenerateResult, error) {
		var metricErr error
		text, err := reader.generate(readingContext, readingInput, nil, func(metrics []ModelTokens) {
			if metricErr == nil {
				metricErr = send(ProviderEvent{Metrics: metrics})
				if metricErr != nil {
					stopReading()
				}
			}
		})
		if metricErr != nil {
			err = metricErr
		}
		return GenerateResult{Text: text, FinishReason: "stop"}, err
	})
	reader.close()
	closeMCP()
	a.mu.Lock()
	a.abort = nil
	a.mu.Unlock()
	if ctx.Err() == nil && errors.Is(readingContext.Err(), context.DeadlineExceeded) {
		a.stopReading("预留最终回答时间")
	}
	if e = a.finishReading(ctx, e); e != nil {
		return GenerateResult{}, e
	}
	if a.activity != nil {
		if e = a.activity("writing", "", "正在根据已读原文生成回答"); e != nil {
			return GenerateResult{}, e
		}
	}
	final, e := isolatedDocumentCodex(ctx, work, model, "", "")
	if e != nil {
		return GenerateResult{}, e
	}
	defer final.close()
	result, e := trackModelUsage(UsageCall{Provider: "codex"}, in.UsageSink, emit, func(send func(ProviderEvent) error) (GenerateResult, error) {
		child, cancel := context.WithCancel(ctx)
		defer cancel()
		var emitErr error
		text, err := final.generate(child, a.finalInput(in), func(part string) {
			if emitErr == nil {
				emitErr = send(ProviderEvent{Text: part})
				if emitErr != nil {
					cancel()
				}
			}
		}, func(metrics []ModelTokens) {
			if emitErr == nil {
				emitErr = send(ProviderEvent{Metrics: metrics})
				if emitErr != nil {
					cancel()
				}
			}
		})
		if emitErr != nil {
			err = emitErr
		}
		return GenerateResult{text, "stop"}, err
	})
	if e != nil {
		return GenerateResult{}, e
	}
	return a.finishDocumentAnswer(ctx, result, emit)
}
