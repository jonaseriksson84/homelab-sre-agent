// Package mcpserver exposes the tool registry over MCP streamable HTTP, so
// the operator can chat about homelab status from any Claude client on the
// tailnet. It is a thin second frontend over internal/tools: zero tools are
// re-implemented here. Per ADR-0002 there is deliberately no app-level auth —
// the tailnet is the auth boundary, and every tool is read-only.
package mcpserver

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jonaseriksson84/homelab-sre-agent/internal/tools"
)

// Handler returns the streamable HTTP handler serving the registry's tools.
func Handler(reg *tools.Registry, version string, log *slog.Logger) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "sre-agent", Version: version}, nil)
	for _, t := range reg.Tools() {
		tool := t // capture
		server.AddTool(&mcp.Tool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.InputSchema,
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			log.Info("mcp tool call", "tool", req.Params.Name)
			out, err := reg.Execute(ctx, req.Params.Name, req.Params.Arguments)
			if err != nil {
				// A failing backend is a tool error the model can react to,
				// never a protocol error.
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, nil
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: out}},
			}, nil
		})
	}
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
}
