package reader

import (
	"context"
	"crypto/subtle"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net"
	"net/http"
	"strings"
	"time"
)

func serveDocumentMCP(agent *documentAgent) (string, string, func(), error) {
	attempt := agent.ID
	server := mcp.NewServer(&mcp.Implementation{Name: "reader_document", Version: "1.0.0"}, &mcp.ServerOptions{Instructions: documentReadingInstructions})
	for _, definition := range documentToolDefinitions {
		d := definition
		server.AddTool(&mcp.Tool{Name: d.Name, Description: d.Description, InputSchema: d.Schema, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			ctx, cancel := context.WithCancel(ctx)
			stop := context.AfterFunc(agent.ctx, cancel)
			defer cancel()
			defer stop()
			raw, err := agent.callAttempt(ctx, attempt, d.Name, req.Params.Arguments)
			if err != nil {
				agent.mu.Lock()
				var abort context.CancelFunc
				if agent.ID == attempt {
					abort = agent.abort
				}
				agent.mu.Unlock()
				if abort != nil {
					abort()
				}
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}, nil
		})
	}
	token := id() + id()
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: 16 << 10})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", nil, err
	}
	httpServer := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		if agent.ctx.Err() != nil || subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")), []byte(token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		if r.Header.Get("Origin") != "" {
			http.Error(w, "invalid origin", 403)
			return
		}
		handler.ServeHTTP(w, r)
	})}
	go httpServer.Serve(listener)
	return "http://" + listener.Addr().String() + "/mcp", token, func() { _ = httpServer.Close() }, nil
}
