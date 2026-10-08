package proxy

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/386jp/mcp-profiles/internal/upstream"
)

type listProfilesResult struct {
	DefaultProfile string           `json:"defaultProfile,omitempty"`
	Profiles       []upstream.State `json:"profiles"`
}

func (p *Proxy) listProfilesTool() *mcp.Tool {
	return &mcp.Tool{
		Name:        p.cfg.ListProfilesTool,
		Description: "List the profiles that tools can target, with their descriptions and whether each is available.",
		InputSchema: map[string]any{"type": "object"},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}
}

func (p *Proxy) handleListProfiles(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	res := listProfilesResult{DefaultProfile: p.cfg.DefaultProfile}
	for _, name := range p.names {
		res.Profiles = append(res.Profiles, p.upstreams[name].State())
	}
	return structuredResult(res)
}

func (p *Proxy) reconnectTool() *mcp.Tool {
	destructive := false
	return &mcp.Tool{
		Name:        p.cfg.ReconnectTool,
		Description: "Reconnect to the MCP server behind a profile. Use this to recover a disabled profile.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				p.cfg.ProfileArg: map[string]any{
					"type":        "string",
					"enum":        p.names,
					"description": "Profile to reconnect.",
				},
			},
			"required": []string{p.cfg.ProfileArg},
		},
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &destructive, IdempotentHint: true},
	}
}

func (p *Proxy) handleReconnect(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, err := parseArgs(req.Params.Arguments)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	var profile string
	if err := json.Unmarshal(args[p.cfg.ProfileArg], &profile); err != nil {
		return errorResult(fmt.Sprintf("argument %q is required and must be a string", p.cfg.ProfileArg)), nil
	}
	u, ok := p.upstreams[profile]
	if !ok {
		return errorResult(fmt.Sprintf("unknown profile %q", profile)), nil
	}
	state := u.Reconnect(ctx)
	if state.Status == upstream.StatusActive {
		p.markUsed(profile)
	}
	return structuredResult(state)
}

// structuredResult returns v as structured content and as JSON text.
func structuredResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(b)}},
		StructuredContent: v,
	}, nil
}
