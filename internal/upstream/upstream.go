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
	"slices"
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
	// StatusIdle means not connected in lazy mode. The next call connects.
	StatusIdle Status = "idle"
)

// State is a snapshot of a profile's state, as returned by the builtin tools.
type State struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Status      Status `json:"status"`
	Reason      string `json:"reason,omitempty"`
	Warning     string `json:"warning,omitempty"`
	// MismatchedTools lists the tools that cannot be called on this profile, with schemaMismatch "warn".
	MismatchedTools []toolset.Mismatch `json:"mismatchedTools,omitempty"`
}

// mismatchWarning is reported for a profile whose tools differ from the exposed definitions.
const mismatchWarning = "some tools differ from the definitions in your tool list and cannot be called on this profile"

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

// MismatchError is returned, instead of calling the upstream, for a tool whose definition
// on the profile differs from the exposed one.
type MismatchError struct {
	Profile string
	Tool    string
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("tool %q on profile %q differs from its definition in your tool list, so it was not called", e.Tool, e.Profile)
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
	// Lazy makes an idle upstream connect on the next call instead of failing it.
	Lazy bool
	// IdleTimeout disconnects a lazy upstream after this long without calls. Zero means never.
	IdleTimeout time.Duration
	// TolerateMismatch keeps the profile when its tools differ from the snapshot, and rejects
	// calls to the tools that differ, instead of disabling the profile.
	TolerateMismatch bool
	// ReportMismatch shows the tools that differ in State, with TolerateMismatch.
	ReportMismatch bool
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
	// mismatched holds the tools that differ from the snapshot, with TolerateMismatch.
	mismatched []toolset.Mismatch

	// Lazy mode bookkeeping.
	inflight       int  // running calls on the current session
	releasePending bool // disconnect once inflight drops to zero
	idleTimer      *time.Timer
}

// New returns an Upstream that is not connected yet.
func New(name string, profile config.Profile, opts Options) *Upstream {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	u := &Upstream{
		name:    name,
		profile: profile,
		opts:    opts,
		logger:  logger.With("profile", name),
		status:  StatusDisabled,
		reason:  "not connected",
	}
	if opts.Lazy {
		u.status, u.reason = StatusIdle, ""
	}
	return u
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
	s := State{Name: u.name, Description: u.profile.Description, Status: u.status, Reason: u.reason}
	if len(u.mismatched) > 0 && u.opts.ReportMismatch {
		s.Warning = mismatchWarning
		s.MismatchedTools = slices.Clone(u.mismatched)
	}
	return s
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

// CheckTools compares tools with the snapshot. With TolerateMismatch, a difference is recorded
// and nil is returned; otherwise it is returned as an error.
func (u *Upstream) CheckTools(tools []*mcp.Tool) error { return u.checkTools(tools) }

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

// CallTool forwards a tools/call to the upstream. In lazy mode, an idle upstream is connected first.
func (u *Upstream) CallTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	session, gen, err := u.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer u.done(gen)

	u.mu.Lock()
	mismatched := slices.ContainsFunc(u.mismatched, func(m toolset.Mismatch) bool { return m.Tool == params.Name })
	u.mu.Unlock()
	if mismatched {
		return nil, &MismatchError{Profile: u.name, Tool: params.Name}
	}

	if u.opts.CallTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, u.opts.CallTimeout)
		defer cancel()
	}
	return session.CallTool(ctx, params)
}

// acquire returns the session for a call and counts the call as in flight on that session.
func (u *Upstream) acquire(ctx context.Context) (*mcp.ClientSession, uint64, error) {
	for {
		u.mu.Lock()
		if u.status == StatusActive && u.session != nil {
			u.inflight++
			u.releasePending = false
			session, gen := u.session, u.gen
			u.mu.Unlock()
			return session, gen, nil
		}
		state := u.stateLocked()
		u.mu.Unlock()
		if state.Status != StatusIdle {
			return nil, 0, &UnavailableError{State: state}
		}
		if err := u.connectIdle(ctx); err != nil {
			return nil, 0, err
		}
	}
}

// done ends an in-flight call, disconnecting if a release was requested meanwhile.
// Calls on a session that has since been replaced are not counted anymore.
func (u *Upstream) done(gen uint64) {
	u.mu.Lock()
	if u.gen != gen {
		u.mu.Unlock()
		return
	}
	u.inflight--
	var session *mcp.ClientSession
	if u.inflight == 0 {
		if u.releasePending {
			session = u.goIdleLocked()
		} else {
			u.armIdleTimerLocked()
		}
	}
	u.mu.Unlock()
	if session != nil {
		_ = session.Close()
		u.logger.Debug("disconnected after the last call finished")
	}
}

// connectIdle connects an idle upstream and checks its tools against the snapshot.
// Concurrent callers wait for the first one and then find the upstream active.
func (u *Upstream) connectIdle(ctx context.Context) error {
	u.reconnectMu.Lock()
	defer u.reconnectMu.Unlock()
	if u.State().Status != StatusIdle {
		return nil
	}

	session, tools, err := u.dial(ctx)
	if err != nil {
		// Stay idle so that the next call tries again.
		return fmt.Errorf("connect: %w", err)
	}
	if err := u.checkTools(tools); err != nil {
		_ = session.Close()
		u.disable(err.Error())
		u.logger.Error("disabled profile", "reason", err.Error())
		return &UnavailableError{State: u.State()}
	}
	u.activate(session)
	u.logger.Info("connected")
	return nil
}

// Release disconnects a lazy upstream and makes it idle, once its in-flight calls finish.
func (u *Upstream) Release() {
	u.mu.Lock()
	if u.status != StatusActive {
		u.mu.Unlock()
		return
	}
	if u.inflight > 0 {
		u.releasePending = true
		u.mu.Unlock()
		return
	}
	session := u.goIdleLocked()
	u.mu.Unlock()
	_ = session.Close()
	u.logger.Debug("disconnected")
}

// goIdleLocked detaches the session and makes the upstream idle. The caller closes the returned session.
func (u *Upstream) goIdleLocked() *mcp.ClientSession {
	session := u.session
	u.session = nil
	u.gen++ // the stdio watcher must not treat this exit as a failure
	u.status, u.reason = StatusIdle, ""
	u.releasePending = false
	u.stopIdleTimerLocked()
	return session
}

func (u *Upstream) armIdleTimerLocked() {
	if !u.opts.Lazy || u.opts.IdleTimeout <= 0 {
		return
	}
	if u.idleTimer == nil {
		u.idleTimer = time.AfterFunc(u.opts.IdleTimeout, u.onIdleTimeout)
		return
	}
	u.idleTimer.Reset(u.opts.IdleTimeout)
}

func (u *Upstream) stopIdleTimerLocked() {
	if u.idleTimer != nil {
		u.idleTimer.Stop()
	}
}

func (u *Upstream) onIdleTimeout() {
	u.mu.Lock()
	// A running call re-arms the timer when it finishes.
	if u.status != StatusActive || u.inflight > 0 {
		u.mu.Unlock()
		return
	}
	session := u.goIdleLocked()
	u.mu.Unlock()
	_ = session.Close()
	u.logger.Info("disconnected after the idle timeout")
}

// Close closes the connection. stdio processes are terminated.
func (u *Upstream) Close() error {
	u.mu.Lock()
	session := u.session
	u.session = nil
	u.gen++
	u.status, u.reason = StatusDisabled, "closed"
	u.stopIdleTimerLocked()
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
	u.inflight, u.releasePending = 0, false
	u.armIdleTimerLocked()
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
	if diffs != nil && !u.opts.TolerateMismatch {
		return fmt.Errorf("tool schema changed: %s", toolset.FormatDiff(diffs))
	}

	mismatched, err := toolset.Mismatched(expected, tools)
	if err != nil {
		return err
	}
	u.mu.Lock()
	u.mismatched = mismatched
	u.mu.Unlock()
	if len(mismatched) > 0 {
		level := slog.LevelDebug // the differences are intended
		if u.opts.ReportMismatch {
			level = slog.LevelWarn
		}
		u.logger.Log(context.Background(), level, "tools differ from the exposed definitions; calls to them are rejected", "tools", mismatched)
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
	u.stopIdleTimerLocked()
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
