// Package upstream manages the connection to the MCP server behind one profile.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/386jp/mcp-profiles/internal/config"
	"github.com/386jp/mcp-profiles/internal/toolset"
)

// ModernProtocolVersion is the protocol version mcp-profiles is designed for.
// Upstreams negotiating an older version are tolerated on a best-effort basis.
const ModernProtocolVersion = "2026-07-28"

// Status is the state of a profile.
type Status string

const (
	StatusActive       Status = "active"
	StatusReconnecting Status = "reconnecting"
	StatusDisabled     Status = "disabled"
)

// State is a snapshot of a profile's state, as returned by the builtin tools.
type State struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Status      Status `json:"status"`
	Reason      string `json:"reason,omitempty"`
}

// UnavailableError is returned when calling a profile that is not active.
type UnavailableError struct {
	State State
}

func (e *UnavailableError) Error() string {
	if e.State.Reason != "" {
		return fmt.Sprintf("profile %q is %s: %s", e.State.Name, e.State.Status, e.State.Reason)
	}
	return fmt.Sprintf("profile %q is %s", e.State.Name, e.State.Status)
}

// Options configures an Upstream.
type Options struct {
	Logger *slog.Logger
	// SDKLogger receives the go-sdk client's own logs. Defaults to Logger.
	SDKLogger *slog.Logger
	// Version is reported to the upstream as the client version.
	Version string
	// ConnectTimeout bounds connecting and listing tools. Zero means no timeout.
	ConnectTimeout time.Duration
	// CallTimeout bounds a single tools/call. Zero means no timeout.
	CallTimeout time.Duration
	// OnProgress receives progress notifications from the upstream.
	OnProgress func(context.Context, *mcp.ProgressNotificationParams)
	// Transport overrides the transport built from the profile. Used in tests.
	Transport func() mcp.Transport
}

// Upstream is the connection to one profile's MCP server.
type Upstream struct {
	name    string
	profile config.Profile
	opts    Options
	logger  *slog.Logger

	// reconnectMu serializes Connect and Reconnect.
	reconnectMu sync.Mutex

	mu       sync.Mutex
	status   Status
	reason   string
	session  *mcp.ClientSession
	gen      uint64      // incremented on every new session, to ignore events from old ones
	expected []*mcp.Tool // the tool snapshot taken at startup
}

// New returns an Upstream that is not connected yet.
func New(name string, profile config.Profile, opts Options) *Upstream {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Upstream{
		name:    name,
		profile: profile,
		opts:    opts,
		logger:  logger.With("profile", name),
		status:  StatusDisabled,
		reason:  "not connected",
	}
}

// Name returns the profile name.
func (u *Upstream) Name() string { return u.name }

// State returns the current state.
func (u *Upstream) State() State {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.stateLocked()
}

func (u *Upstream) stateLocked() State {
	return State{Name: u.name, Description: u.profile.Description, Status: u.status, Reason: u.reason}
}

// Connect connects to the upstream and returns its tools. The profile becomes active on success.
// It is used at startup, before the expected tools are known.
func (u *Upstream) Connect(ctx context.Context) ([]*mcp.Tool, error) {
	u.reconnectMu.Lock()
	defer u.reconnectMu.Unlock()

	session, tools, err := u.dial(ctx)
	if err != nil {
		return nil, err
	}
	u.activate(session)
	return tools, nil
}

// SetExpected records the tool snapshot that later tool lists must match.
func (u *Upstream) SetExpected(tools []*mcp.Tool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.expected = tools
}

// Reconnect drops the current connection, connects again and checks the tools against the snapshot.
func (u *Upstream) Reconnect(ctx context.Context) State {
	u.reconnectMu.Lock()
	defer u.reconnectMu.Unlock()

	u.mu.Lock()
	old := u.session
	u.session = nil
	u.gen++
	u.status, u.reason = StatusReconnecting, ""
	u.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}

	session, tools, err := u.dial(ctx)
	if err != nil {
		u.disable(fmt.Sprintf("reconnect failed: %v", err))
		return u.State()
	}
	if err := u.checkTools(tools); err != nil {
		_ = session.Close()
		u.disable(err.Error())
		return u.State()
	}
	u.activate(session)
	u.logger.Info("reconnected")
	return u.State()
}

// CallTool forwards a tools/call to the upstream.
func (u *Upstream) CallTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	u.mu.Lock()
	session := u.session
	state := u.stateLocked()
	u.mu.Unlock()
	if state.Status != StatusActive || session == nil {
		return nil, &UnavailableError{State: state}
	}

	if u.opts.CallTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, u.opts.CallTimeout)
		defer cancel()
	}
	return session.CallTool(ctx, params)
}

// Close closes the connection. stdio processes are terminated.
func (u *Upstream) Close() error {
	u.mu.Lock()
	session := u.session
	u.session = nil
	u.gen++
	u.status, u.reason = StatusDisabled, "closed"
	u.mu.Unlock()
	if session == nil {
		return nil
	}
	return session.Close()
}

// dial opens a new session and lists all tools.
func (u *Upstream) dial(ctx context.Context) (*mcp.ClientSession, []*mcp.Tool, error) {
	if u.opts.ConnectTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, u.opts.ConnectTimeout)
		defer cancel()
	}

	sdkLogger := u.opts.SDKLogger
	if sdkLogger == nil {
		sdkLogger = u.logger
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "mcp-profiles", Version: u.opts.Version}, &mcp.ClientOptions{
		Logger:                 sdkLogger.With("profile", u.name),
		ToolListChangedHandler: u.onToolListChanged,
		ProgressNotificationHandler: func(ctx context.Context, req *mcp.ProgressNotificationClientRequest) {
			if u.opts.OnProgress != nil {
				u.opts.OnProgress(ctx, req.Params)
			}
		},
		// Pass input-required results through to the client instead of handling them here.
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
	})
	session, err := client.Connect(ctx, u.transport(), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("connect: %w", err)
	}

	version := session.InitializeResult().ProtocolVersion
	if version < ModernProtocolVersion {
		u.logger.Warn("upstream negotiated an older protocol version; this is unsupported and works on a best-effort basis",
			"protocolVersion", version)
	}

	tools, err := listTools(ctx, session)
	if err != nil {
		_ = session.Close()
		return nil, nil, fmt.Errorf("list tools (protocol version %s): %w", version, err)
	}
	return session, tools, nil
}

func listTools(ctx context.Context, session *mcp.ClientSession) ([]*mcp.Tool, error) {
	var tools []*mcp.Tool
	for t, err := range session.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		tools = append(tools, t)
	}
	return tools, nil
}

func (u *Upstream) transport() mcp.Transport {
	if u.opts.Transport != nil {
		return u.opts.Transport()
	}
	switch u.profile.Type {
	case config.TypeStdio:
		cmd := exec.Command(u.profile.Command, u.profile.Args...)
		cmd.Env = os.Environ()
		for k, v := range u.profile.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		cmd.Stderr = os.Stderr
		return &mcp.CommandTransport{Command: cmd}
	default:
		return &mcp.StreamableClientTransport{
			Endpoint: u.profile.URL,
			HTTPClient: &http.Client{Transport: &headerTransport{
				headers: u.profile.Headers,
				base:    http.DefaultTransport,
			}},
		}
	}
}

func (u *Upstream) activate(session *mcp.ClientSession) {
	u.mu.Lock()
	u.session = session
	u.gen++
	gen := u.gen
	u.status, u.reason = StatusActive, ""
	u.mu.Unlock()

	if u.profile.Type == config.TypeStdio {
		go u.watch(session, gen)
	}
}

// watch disables the profile when the stdio process exits on its own.
func (u *Upstream) watch(session *mcp.ClientSession, gen uint64) {
	err := session.Wait()
	reason := "upstream process exited"
	if err != nil && !errors.Is(err, mcp.ErrConnectionClosed) {
		reason = fmt.Sprintf("%s: %v", reason, err)
	}
	if u.disableIfCurrent(gen, reason) {
		u.logger.Error("upstream disconnected", "reason", reason)
	}
}

func (u *Upstream) onToolListChanged(ctx context.Context, req *mcp.ToolListChangedRequest) {
	u.mu.Lock()
	session, gen := u.session, u.gen
	u.mu.Unlock()
	if session == nil || req.Session != session {
		return
	}
	// Do not block the SDK's notification handling while listing.
	go func() {
		ctx := context.Background()
		if u.opts.ConnectTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, u.opts.ConnectTimeout)
			defer cancel()
		}
		tools, err := listTools(ctx, session)
		if err != nil {
			u.logger.Warn("failed to list tools after a change notification", "error", err)
			return
		}
		if err := u.checkTools(tools); err != nil {
			if u.disableIfCurrent(gen, err.Error()) {
				u.logger.Error("disabled profile", "reason", err.Error())
				_ = session.Close()
			}
		}
	}()
}

func (u *Upstream) checkTools(tools []*mcp.Tool) error {
	u.mu.Lock()
	expected := u.expected
	u.mu.Unlock()
	diffs, err := toolset.Diff(expected, tools)
	if err != nil {
		return err
	}
	if diffs != nil {
		return fmt.Errorf("tool schema changed: %s", toolset.FormatDiff(diffs))
	}
	return nil
}

func (u *Upstream) disable(reason string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status, u.reason = StatusDisabled, reason
}

// disableIfCurrent disables the profile only if gen is still the current session.
func (u *Upstream) disableIfCurrent(gen uint64, reason string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.gen != gen || u.status != StatusActive {
		return false
	}
	u.session = nil
	u.status, u.reason = StatusDisabled, reason
	return true
}

// headerTransport adds static headers to every request.
type headerTransport struct {
	headers map[string]string
	base    http.RoundTripper
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(t.headers) > 0 {
		req = req.Clone(req.Context())
		for k, v := range t.headers {
			req.Header.Set(k, v)
		}
	}
	return t.base.RoundTrip(req)
}
