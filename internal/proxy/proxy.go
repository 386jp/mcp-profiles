// Package proxy builds the MCP server exposed to clients and routes tool calls to profiles.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/386jp/mcp-profiles/internal/config"
	"github.com/386jp/mcp-profiles/internal/toolset"
	"github.com/386jp/mcp-profiles/internal/upstream"
)

// reservedMetaPrefix marks _meta keys owned by the protocol, which the SDK sets per connection.
const reservedMetaPrefix = "io.modelcontextprotocol/"

// Options configures a Proxy.
type Options struct {
	Logger *slog.Logger
	// SDKLogger receives the go-sdk's own logs. Defaults to Logger.
	SDKLogger *slog.Logger
	Version   string
	// Transports overrides upstream transports by profile name. Used in tests.
	Transports map[string]func() mcp.Transport
}

// Proxy bundles the upstreams of all profiles behind one MCP server.
type Proxy struct {
	cfg       *config.Config
	opts      Options
	logger    *slog.Logger
	names     []string // sorted profile names
	upstreams map[string]*upstream.Upstream
	progress  progressRelay

	recentMu sync.Mutex
	recent   []string // profile names, most recently used first (lazy mode)
	server   *mcp.Server

	inflight inflight
}

// New creates the upstreams. Nothing is connected until Start.
func New(cfg *config.Config, opts Options) *Proxy {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if opts.SDKLogger == nil {
		opts.SDKLogger = logger
	}
	p := &Proxy{
		cfg:       cfg,
		opts:      opts,
		logger:    logger,
		upstreams: make(map[string]*upstream.Upstream, len(cfg.Profiles)),
	}
	p.progress.targets = map[string]progressTarget{}
	for name, profile := range cfg.Profiles {
		p.names = append(p.names, name)
		p.upstreams[name] = upstream.New(name, profile, upstream.Options{
			Logger:           logger,
			SDKLogger:        opts.SDKLogger,
			Version:          opts.Version,
			ConnectTimeout:   cfg.StartupTimeout(),
			CallTimeout:      cfg.CallTimeout(),
			OnProgress:       p.progress.forward,
			Transport:        opts.Transports[name],
			Lazy:             cfg.Lazy.Enabled,
			IdleTimeout:      cfg.Lazy.IdleTimeout,
			TolerateMismatch: cfg.ToleratesMismatch(),
			ReportMismatch:   cfg.SchemaMismatch == config.SchemaMismatchWarn,
		})
	}
	slices.Sort(p.names)
	return p
}

// Start connects to every upstream, verifies that their tools match and builds the server.
// In lazy mode, only the base profile is connected, to read the tools.
// On error, all upstreams are closed.
func (p *Proxy) Start(ctx context.Context) error {
	connect := p.connectAll
	if p.cfg.Lazy.Enabled {
		connect = p.connectBase
	}
	tools, err := connect(ctx)
	if err == nil {
		err = p.buildServer(tools)
	}
	if err != nil {
		p.Close()
		return err
	}
	return nil
}

// Server returns the MCP server to expose to clients. Valid after Start.
func (p *Proxy) Server() *mcp.Server { return p.server }

// Shutdown rejects new calls and waits for in-flight calls until ctx is done.
func (p *Proxy) Shutdown(ctx context.Context) error {
	select {
	case <-p.inflight.drain():
		return nil
	case <-ctx.Done():
		return fmt.Errorf("in-flight calls did not finish: %w", ctx.Err())
	}
}

// Close closes all upstreams.
func (p *Proxy) Close() {
	var wg sync.WaitGroup
	for _, u := range p.upstreams {
		wg.Go(func() {
			if err := u.Close(); err != nil {
				p.logger.Warn("failed to close upstream", "profile", u.Name(), "error", err)
			}
		})
	}
	wg.Wait()
}

// baseProfile is the profile whose tool definitions are exposed and compared against.
func (p *Proxy) baseProfile() string {
	if p.cfg.BaseProfile != "" {
		return p.cfg.BaseProfile
	}
	if p.cfg.DefaultProfile != "" {
		return p.cfg.DefaultProfile
	}
	return p.names[0]
}

// connectAll connects in parallel and returns the base profile's tools once all of them match.
func (p *Proxy) connectAll(ctx context.Context) ([]*mcp.Tool, error) {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		tools = map[string][]*mcp.Tool{}
		errs  []error
	)
	for _, name := range p.names {
		wg.Go(func() {
			t, err := p.upstreams[name].Connect(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("profile %q: %w", name, err))
				return
			}
			tools[name] = t
		})
	}
	wg.Wait()
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	base := p.baseProfile()
	if !p.cfg.ToleratesMismatch() {
		for _, name := range p.names {
			if name == base {
				continue
			}
			diffs, err := toolset.Diff(tools[base], tools[name])
			if err != nil {
				return nil, fmt.Errorf("profile %q: %w", name, err)
			}
			if diffs != nil {
				errs = append(errs, fmt.Errorf("profile %q: tools differ from base profile %q: %s", name, base, toolset.FormatDiff(diffs)))
			}
		}
		if len(errs) > 0 {
			return nil, errors.Join(errs...)
		}
	}
	for name, u := range p.upstreams {
		u.SetExpected(tools[base])
		// With schemaMismatch "warn" or "silent", this records the tools that differ instead of failing.
		if err := u.CheckTools(tools[name]); err != nil {
			return nil, fmt.Errorf("profile %q: %w", name, err)
		}
	}
	return tools[base], nil
}

// connectBase reads the tools from the base profile only. The other profiles are checked
// against them when they are first used.
func (p *Proxy) connectBase(ctx context.Context) ([]*mcp.Tool, error) {
	base := p.baseProfile()
	tools, err := p.upstreams[base].Connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("profile %q: %w", base, err)
	}
	for _, u := range p.upstreams {
		u.SetExpected(tools)
	}
	if p.cfg.Lazy.KeepBase {
		p.markUsed(base)
	} else {
		p.upstreams[base].Release()
	}
	return tools, nil
}

// markUsed makes name the most recently used profile. In lazy mode, connected profiles
// beyond lazy.maxConnected, oldest first, are disconnected once their running calls finish.
func (p *Proxy) markUsed(name string) {
	if !p.cfg.Lazy.Enabled {
		return
	}
	p.recentMu.Lock()
	defer p.recentMu.Unlock()
	p.recent = slices.DeleteFunc(p.recent, func(n string) bool { return n == name })
	p.recent = slices.Insert(p.recent, 0, name)

	// Count only connected profiles, so that ones already idle do not take a slot.
	connected := 1 // name itself, which is connected or about to be
	for _, other := range p.recent[1:] {
		u := p.upstreams[other]
		if u.State().Status != upstream.StatusActive {
			continue
		}
		if connected < p.cfg.Lazy.MaxConnected {
			connected++
			continue
		}
		// Closing a stdio upstream waits for the process to exit, so do not block the call.
		go u.Release()
	}
}

func (p *Proxy) buildServer(tools []*mcp.Tool) error {
	exposed, err := toolset.WithProfileArg(tools, toolset.ProfileArg{
		Name:         p.cfg.ProfileArg,
		Profiles:     p.names,
		Default:      p.cfg.DefaultProfile,
		ListToolName: p.cfg.ListProfilesTool,
	})
	if err != nil {
		return err
	}
	names := toolset.Names(tools)
	for _, builtin := range []string{p.cfg.ListProfilesTool, p.cfg.ReconnectTool} {
		if builtin != "" && slices.Contains(names, builtin) {
			return fmt.Errorf("builtin tool name %q conflicts with an upstream tool", builtin)
		}
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "mcp-profiles", Version: p.opts.Version}, &mcp.ServerOptions{
		Logger:                    p.opts.SDKLogger,
		SupportedProtocolVersions: []string{upstream.ModernProtocolVersion},
	})
	for _, t := range exposed {
		server.AddTool(t, p.forwardTool(t.Name))
	}
	if p.cfg.ListProfilesTool != "" {
		server.AddTool(p.listProfilesTool(), p.track(p.handleListProfiles))
	}
	if p.cfg.ReconnectTool != "" {
		server.AddTool(p.reconnectTool(), p.track(p.handleReconnect))
	}
	p.server = server
	return nil
}

// track counts in-flight calls and rejects new ones during shutdown.
func (p *Proxy) track(h mcp.ToolHandler) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if !p.inflight.enter() {
			return errorResult("mcp-profiles is shutting down"), nil
		}
		defer p.inflight.leave()
		return h(ctx, req)
	}
}

func (p *Proxy) forwardTool(name string) mcp.ToolHandler {
	return p.track(func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := parseArgs(req.Params.Arguments)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		profile, err := p.takeProfile(args)
		if err != nil {
			return errorResult(err.Error()), nil
		}

		params := &mcp.CallToolParams{
			Name:           name,
			Arguments:      args,
			InputResponses: req.Params.InputResponses,
			RequestState:   req.Params.RequestState,
		}
		for k, v := range req.Params.Meta {
			if strings.HasPrefix(k, reservedMetaPrefix) || k == "progressToken" {
				continue
			}
			if params.Meta == nil {
				params.Meta = mcp.Meta{}
			}
			params.Meta[k] = v
		}
		if token := req.Params.GetProgressToken(); token != nil {
			proxyToken, release := p.progress.register(ctx, req.Session, token)
			defer release()
			params.SetProgressToken(proxyToken)
		}

		p.markUsed(profile)
		res, err := p.upstreams[profile].CallTool(ctx, params)
		if err != nil {
			return errorResult(fmt.Sprintf("profile %q: %v", profile, err)), nil
		}
		// Protocol-owned keys such as the upstream's serverInfo must not reach the client.
		for k := range res.Meta {
			if strings.HasPrefix(k, reservedMetaPrefix) {
				delete(res.Meta, k)
			}
		}
		return res, nil
	})
}

// takeProfile removes the profile argument from args and resolves it.
func (p *Proxy) takeProfile(args map[string]json.RawMessage) (string, error) {
	raw, ok := args[p.cfg.ProfileArg]
	delete(args, p.cfg.ProfileArg)
	if !ok || string(raw) == "null" {
		if p.cfg.DefaultProfile == "" {
			return "", fmt.Errorf("argument %q is required; choose one of %s", p.cfg.ProfileArg, strings.Join(p.names, ", "))
		}
		return p.cfg.DefaultProfile, nil
	}
	var profile string
	if err := json.Unmarshal(raw, &profile); err != nil {
		return "", fmt.Errorf("argument %q must be a string", p.cfg.ProfileArg)
	}
	if _, ok := p.upstreams[profile]; !ok {
		return "", fmt.Errorf("unknown profile %q; choose one of %s", profile, strings.Join(p.names, ", "))
	}
	return profile, nil
}

func parseArgs(raw json.RawMessage) (map[string]json.RawMessage, error) {
	args := map[string]json.RawMessage{}
	if len(raw) == 0 || string(raw) == "null" {
		return args, nil
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, errors.New("arguments must be a JSON object")
	}
	return args, nil
}

func errorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
		IsError: true,
	}
}

// inflight counts running calls and stops accepting new ones once drained.
type inflight struct {
	mu       sync.Mutex
	n        int
	draining bool
	idle     chan struct{}
}

func (f *inflight) enter() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.draining {
		return false
	}
	f.n++
	return true
}

func (f *inflight) leave() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n--
	if f.draining && f.n == 0 {
		close(f.idle)
	}
}

// drain stops accepting calls and returns a channel closed when none are running.
func (f *inflight) drain() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.draining {
		f.draining = true
		f.idle = make(chan struct{})
		if f.n == 0 {
			close(f.idle)
		}
	}
	return f.idle
}
