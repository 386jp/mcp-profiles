package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/386jp/mcp-profiles/internal/config"
	"github.com/386jp/mcp-profiles/internal/listen"
	"github.com/386jp/mcp-profiles/internal/toolset"
	"github.com/386jp/mcp-profiles/internal/upstream"
)

// progressDelivered is signaled when the downstream client receives a progress notification.
// The upstream tool waits for it, because progress sent after the response may be dropped.
var progressDelivered = make(chan struct{}, 1)

type echoArgs struct {
	Text string `json:"text"`
}

// newUpstreamServer returns an MCP server whose echo tool prefixes the text with label.
func newUpstreamServer(label string, extraTool bool) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: label}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "echo", Description: "echo on " + label},
		func(ctx context.Context, req *mcp.CallToolRequest, args echoArgs) (*mcp.CallToolResult, any, error) {
			if token := req.Params.GetProgressToken(); token != nil {
				_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: token, Progress: 1, Total: 2})
				select {
				case <-progressDelivered:
				case <-time.After(5 * time.Second):
				}
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: label + ":" + args.Text}}}, nil, nil
		})
	if extraTool {
		mcp.AddTool(s, &mcp.Tool{Name: "extra"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})
	}
	return s
}

// upstreamHTTP serves an upstream over streamable HTTP. The served server can be swapped.
type upstreamHTTP struct {
	url    string
	server atomic.Pointer[mcp.Server]
}

func startUpstream(t *testing.T, s *mcp.Server, headers map[string]string) *upstreamHTTP {
	t.Helper()
	u := &upstreamHTTP{}
	u.server.Store(s)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return u.server.Load() },
		&mcp.StreamableHTTPOptions{Stateless: true})
	ts := httptest.NewServer(listen.RequireHeaders(headers, h))
	t.Cleanup(ts.Close)
	u.url = ts.URL
	return u
}

type downstream struct {
	session  *mcp.ClientSession
	mu       sync.Mutex
	progress []*mcp.ProgressNotificationParams
}

// startProxy starts a proxy and connects a client to it in memory.
func startProxy(t *testing.T, cfg *config.Config) (*Proxy, *downstream) {
	t.Helper()
	ctx := context.Background()
	p := New(cfg, Options{Version: "test"})
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(p.Close)

	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := p.Server().Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	d := &downstream{}
	client := mcp.NewClient(&mcp.Implementation{Name: "client"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.progress = append(d.progress, req.Params)
			select {
			case progressDelivered <- struct{}{}:
			default:
			}
		},
	})
	d.session, err = client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.session.Close() })
	return p, d
}

func httpProfile(u *upstreamHTTP, headers map[string]string) config.Profile {
	return config.Profile{Type: config.TypeHTTP, URL: u.url, Headers: headers}
}

func text(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("content = %v", res.Content)
	}
	return res.Content[0].(*mcp.TextContent).Text
}

func call(t *testing.T, d *downstream, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := d.session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	return res
}

func TestRouting(t *testing.T) {
	dev := startUpstream(t, newUpstreamServer("dev", false), nil)
	prod := startUpstream(t, newUpstreamServer("prod", false), map[string]string{"Authorization": "Bearer secret"})
	cfg := &config.Config{
		ProfileArg:       "profile",
		DefaultProfile:   "dev",
		ListProfilesTool: "list_profiles",
		ReconnectTool:    "reconnect_profile",
		Profiles: map[string]config.Profile{
			"dev":  httpProfile(dev, nil),
			"prod": httpProfile(prod, map[string]string{"Authorization": "Bearer secret"}),
		},
	}
	_, d := startProxy(t, cfg)

	t.Run("tools/list", func(t *testing.T) {
		res, err := d.session.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		if strings.Join(names, ",") != "echo,list_profiles,reconnect_profile" {
			t.Errorf("tools = %v", names)
		}
		echo := res.Tools[0]
		if echo.Description != "echo on dev" {
			t.Errorf("description should come from the default profile: %q", echo.Description)
		}
		props := echo.InputSchema.(map[string]any)["properties"].(map[string]any)
		if _, ok := props["profile"]; !ok {
			t.Errorf("profile argument not injected: %v", props)
		}
	})

	t.Run("routes by profile", func(t *testing.T) {
		if got := text(t, call(t, d, "echo", map[string]any{"text": "hi", "profile": "prod"})); got != "prod:hi" {
			t.Errorf("got %q", got)
		}
		res := call(t, d, "echo", map[string]any{"text": "hi"})
		if got := text(t, res); got != "dev:hi" {
			t.Errorf("default profile: got %q", got)
		}
		if name := res.Meta["io.modelcontextprotocol/serverInfo"]; name != nil {
			if info, _ := name.(map[string]any); info["name"] == "dev" {
				t.Errorf("upstream serverInfo leaked: %v", res.Meta)
			}
		}
	})

	t.Run("unknown profile", func(t *testing.T) {
		res := call(t, d, "echo", map[string]any{"text": "hi", "profile": "stg"})
		if !res.IsError || !strings.Contains(text(t, res), `unknown profile "stg"`) {
			t.Errorf("got %v %q", res.IsError, text(t, res))
		}
	})

	t.Run("relays progress", func(t *testing.T) {
		params := &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "hi", "profile": "prod"}}
		params.SetProgressToken("client-token")
		if _, err := d.session.CallTool(context.Background(), params); err != nil {
			t.Fatal(err)
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if len(d.progress) != 1 || d.progress[0].ProgressToken != "client-token" || d.progress[0].Total != 2 {
			t.Errorf("progress = %+v", d.progress)
		}
	})

	t.Run("list_profiles", func(t *testing.T) {
		res := call(t, d, "list_profiles", nil)
		var got listProfilesResult
		if err := json.Unmarshal([]byte(text(t, res)), &got); err != nil {
			t.Fatal(err)
		}
		if got.DefaultProfile != "dev" || len(got.Profiles) != 2 || got.Profiles[1].Name != "prod" || got.Profiles[1].Status != "active" {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("reconnect detects a schema change", func(t *testing.T) {
		prod.server.Store(newUpstreamServer("prod", true))
		res := call(t, d, "reconnect_profile", map[string]any{"profile": "prod"})
		if !strings.Contains(text(t, res), `"status":"disabled"`) || !strings.Contains(text(t, res), `tool \"extra\" is unexpected`) {
			t.Errorf("reconnect = %s", text(t, res))
		}
		res = call(t, d, "echo", map[string]any{"text": "hi", "profile": "prod"})
		if !res.IsError || !strings.Contains(text(t, res), "disabled") {
			t.Errorf("call on disabled profile = %v %q", res.IsError, text(t, res))
		}

		prod.server.Store(newUpstreamServer("prod", false))
		res = call(t, d, "reconnect_profile", map[string]any{"profile": "prod"})
		if !strings.Contains(text(t, res), `"status":"active"`) {
			t.Errorf("reconnect = %s", text(t, res))
		}
		if got := text(t, call(t, d, "echo", map[string]any{"text": "hi", "profile": "prod"})); got != "prod:hi" {
			t.Errorf("got %q", got)
		}
	})
}

func TestProfileRequiredWithoutDefault(t *testing.T) {
	dev := startUpstream(t, newUpstreamServer("dev", false), nil)
	cfg := &config.Config{
		ProfileArg: "env",
		Profiles:   map[string]config.Profile{"dev": httpProfile(dev, nil)},
	}
	_, d := startProxy(t, cfg)
	res := call(t, d, "echo", map[string]any{"text": "hi"})
	if !res.IsError || !strings.Contains(text(t, res), `argument "env" is required`) {
		t.Errorf("got %v %q", res.IsError, text(t, res))
	}
	if got := text(t, call(t, d, "echo", map[string]any{"text": "hi", "env": "dev"})); got != "dev:hi" {
		t.Errorf("got %q", got)
	}
}

func TestStartFails(t *testing.T) {
	dev := startUpstream(t, newUpstreamServer("dev", false), nil)
	tests := []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{
			name: "schema mismatch",
			cfg: &config.Config{ProfileArg: "profile", Profiles: map[string]config.Profile{
				"dev":  httpProfile(dev, nil),
				"prod": httpProfile(startUpstream(t, newUpstreamServer("prod", true), nil), nil),
			}},
			want: `profile "prod": tools differ from base profile "dev": tool "extra" is unexpected`,
		},
		{
			name: "argument conflict",
			cfg:  &config.Config{ProfileArg: "text", Profiles: map[string]config.Profile{"dev": httpProfile(dev, nil)}},
			want: `already has an argument named "text"`,
		},
		{
			name: "builtin tool conflict",
			cfg:  &config.Config{ProfileArg: "profile", ListProfilesTool: "echo", Profiles: map[string]config.Profile{"dev": httpProfile(dev, nil)}},
			want: `builtin tool name "echo" conflicts`,
		},
		{
			name: "missing header",
			cfg: &config.Config{ProfileArg: "profile", Profiles: map[string]config.Profile{
				"dev": httpProfile(startUpstream(t, newUpstreamServer("dev", false), map[string]string{"X-Key": "k"}), nil),
			}},
			want: `profile "dev"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := New(tt.cfg, Options{}).Start(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Start err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestShutdownRejectsNewCalls(t *testing.T) {
	dev := startUpstream(t, newUpstreamServer("dev", false), nil)
	p, d := startProxy(t, &config.Config{ProfileArg: "profile", DefaultProfile: "dev", Profiles: map[string]config.Profile{"dev": httpProfile(dev, nil)}})
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	res := call(t, d, "echo", map[string]any{"text": "hi"})
	if !res.IsError || !strings.Contains(text(t, res), "shutting down") {
		t.Errorf("got %v %q", res.IsError, text(t, res))
	}
}

func statuses(t *testing.T, d *downstream) map[string]string {
	t.Helper()
	var got listProfilesResult
	if err := json.Unmarshal([]byte(text(t, call(t, d, "list_profiles", nil))), &got); err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, s := range got.Profiles {
		m[s.Name] = string(s.Status)
	}
	return m
}

// waitStatuses polls until the profile statuses match, since releases happen in the background.
func waitStatuses(t *testing.T, d *downstream, want map[string]string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := statuses(t, d)
		if reflect.DeepEqual(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("statuses = %v, want %v", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func lazyConfig(lazy config.Lazy, dev, prod *upstreamHTTP) *config.Config {
	return &config.Config{
		ProfileArg:       "profile",
		DefaultProfile:   "dev",
		ListProfilesTool: "list_profiles",
		Lazy:             lazy,
		Profiles: map[string]config.Profile{
			"dev":  httpProfile(dev, nil),
			"prod": httpProfile(prod, nil),
		},
	}
}

func TestLazyKeepsOnlyTheLastUsedProfile(t *testing.T) {
	dev := startUpstream(t, newUpstreamServer("dev", false), nil)
	prod := startUpstream(t, newUpstreamServer("prod", false), nil)
	_, d := startProxy(t, lazyConfig(config.Lazy{Enabled: true, MaxConnected: 1}, dev, prod))

	waitStatuses(t, d, map[string]string{"dev": "idle", "prod": "idle"})

	if got := text(t, call(t, d, "echo", map[string]any{"text": "hi", "profile": "prod"})); got != "prod:hi" {
		t.Errorf("got %q", got)
	}
	waitStatuses(t, d, map[string]string{"dev": "idle", "prod": "active"})

	if got := text(t, call(t, d, "echo", map[string]any{"text": "hi"})); got != "dev:hi" {
		t.Errorf("got %q", got)
	}
	waitStatuses(t, d, map[string]string{"dev": "active", "prod": "idle"})
}

func TestLazyKeepBase(t *testing.T) {
	dev := startUpstream(t, newUpstreamServer("dev", false), nil)
	prod := startUpstream(t, newUpstreamServer("prod", false), nil)
	_, d := startProxy(t, lazyConfig(config.Lazy{Enabled: true, MaxConnected: 1, KeepBase: true}, dev, prod))
	waitStatuses(t, d, map[string]string{"dev": "active", "prod": "idle"})
}

func TestLazyChecksSchemaOnFirstUse(t *testing.T) {
	dev := startUpstream(t, newUpstreamServer("dev", false), nil)
	prod := startUpstream(t, newUpstreamServer("prod", true), nil)
	// Startup succeeds because prod is not connected yet.
	_, d := startProxy(t, lazyConfig(config.Lazy{Enabled: true, MaxConnected: 1}, dev, prod))

	res := call(t, d, "echo", map[string]any{"text": "hi", "profile": "prod"})
	if !res.IsError || !strings.Contains(text(t, res), `tool "extra" is unexpected`) {
		t.Errorf("got %v %q", res.IsError, text(t, res))
	}
	waitStatuses(t, d, map[string]string{"dev": "idle", "prod": "disabled"})

	if got := text(t, call(t, d, "echo", map[string]any{"text": "hi", "profile": "dev"})); got != "dev:hi" {
		t.Errorf("got %q", got)
	}
}

func TestLazyMaxConnected(t *testing.T) {
	cfg := &config.Config{
		ProfileArg:       "profile",
		ListProfilesTool: "list_profiles",
		Lazy:             config.Lazy{Enabled: true, MaxConnected: 2},
		Profiles:         map[string]config.Profile{},
	}
	for _, name := range []string{"dev", "stg", "prod"} {
		cfg.Profiles[name] = httpProfile(startUpstream(t, newUpstreamServer(name, false), nil), nil)
	}
	_, d := startProxy(t, cfg)

	for _, name := range []string{"prod", "stg"} {
		call(t, d, "echo", map[string]any{"text": "hi", "profile": name})
	}
	waitStatuses(t, d, map[string]string{"dev": "idle", "stg": "active", "prod": "active"})

	// prod is the least recently used, so it is the one disconnected.
	call(t, d, "echo", map[string]any{"text": "hi", "profile": "dev"})
	waitStatuses(t, d, map[string]string{"dev": "active", "stg": "active", "prod": "idle"})

	// Using stg again keeps it, and dev stays as the second most recent.
	call(t, d, "echo", map[string]any{"text": "hi", "profile": "stg"})
	waitStatuses(t, d, map[string]string{"dev": "active", "stg": "active", "prod": "idle"})
}

type greetArgs struct {
	Name string `json:"name"`
}

type greetArgsV2 struct {
	Name   string `json:"name"`
	Formal bool   `json:"formal"`
}

// newGreetServer returns an echo server with a greet tool whose definition depends on variant:
// "same", "changed" (different inputSchema) or "missing".
func newGreetServer(label, variant string) *mcp.Server {
	s := newUpstreamServer(label, false)
	switch variant {
	case "same":
		mcp.AddTool(s, &mcp.Tool{Name: "greet"}, func(_ context.Context, _ *mcp.CallToolRequest, args greetArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: label + ":hello " + args.Name}}}, nil, nil
		})
	case "changed":
		mcp.AddTool(s, &mcp.Tool{Name: "greet"}, func(_ context.Context, _ *mcp.CallToolRequest, args greetArgsV2) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: label + ":hello " + args.Name}}}, nil, nil
		})
	}
	return s
}

func listProfiles(t *testing.T, d *downstream) map[string]upstream.State {
	t.Helper()
	var got listProfilesResult
	if err := json.Unmarshal([]byte(text(t, call(t, d, "list_profiles", nil))), &got); err != nil {
		t.Fatal(err)
	}
	m := map[string]upstream.State{}
	for _, s := range got.Profiles {
		m[s.Name] = s
	}
	return m
}

func warnings(t *testing.T, d *downstream) map[string]string {
	t.Helper()
	var got listProfilesResult
	if err := json.Unmarshal([]byte(text(t, call(t, d, "list_profiles", nil))), &got); err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, s := range got.Profiles {
		m[s.Name] = s.Warning
	}
	return m
}

func TestBaseProfile(t *testing.T) {
	dev := startUpstream(t, newUpstreamServer("dev", false), nil)
	prod := startUpstream(t, newUpstreamServer("prod", false), nil)
	cfg := &config.Config{
		ProfileArg:     "profile",
		BaseProfile:    "prod",
		DefaultProfile: "dev",
		Profiles:       map[string]config.Profile{"dev": httpProfile(dev, nil), "prod": httpProfile(prod, nil)},
	}
	_, d := startProxy(t, cfg)

	res, err := d.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Tools[0].Description; got != "echo on prod" {
		t.Errorf("description should come from the base profile: %q", got)
	}
	// Routing still defaults to defaultProfile.
	if got := text(t, call(t, d, "echo", map[string]any{"text": "hi"})); got != "dev:hi" {
		t.Errorf("got %q", got)
	}
}

func warnConfig(lazy config.Lazy, servers map[string]*mcp.Server, t *testing.T) *config.Config {
	return mismatchConfig(config.SchemaMismatchWarn, lazy, servers, t)
}

func mismatchConfig(mode string, lazy config.Lazy, servers map[string]*mcp.Server, t *testing.T) *config.Config {
	cfg := &config.Config{
		ProfileArg:       "profile",
		BaseProfile:      "dev",
		DefaultProfile:   "dev",
		ListProfilesTool: "list_profiles",
		SchemaMismatch:   mode,
		Lazy:             lazy,
		Profiles:         map[string]config.Profile{},
	}
	for name, s := range servers {
		cfg.Profiles[name] = httpProfile(startUpstream(t, s, nil), nil)
	}
	return cfg
}

func TestSchemaMismatchWarn(t *testing.T) {
	_, d := startProxy(t, warnConfig(config.Lazy{}, map[string]*mcp.Server{
		"dev":  newGreetServer("dev", "same"),
		"stg":  newGreetServer("stg", "changed"),
		"prod": newGreetServer("prod", "missing"),
	}, t))

	// Startup succeeds and the differing profiles stay active with a warning.
	waitStatuses(t, d, map[string]string{"dev": "active", "stg": "active", "prod": "active"})
	w := warnings(t, d)
	if w["dev"] != "" || !strings.Contains(w["stg"], "differ from the definitions in your tool list") || w["prod"] == "" {
		t.Errorf("warnings = %v", w)
	}
	states := listProfiles(t, d)
	for profile, want := range map[string][]toolset.Mismatch{
		"dev":  nil,
		"stg":  {{Tool: "greet", Reason: toolset.ReasonInputSchema}},
		"prod": {{Tool: "greet", Reason: toolset.ReasonMissing}},
	} {
		if got := states[profile].MismatchedTools; !reflect.DeepEqual(got, want) {
			t.Errorf("mismatchedTools of %s = %v, want %v", profile, got, want)
		}
	}

	// Tools that match still work on every profile.
	if got := text(t, call(t, d, "echo", map[string]any{"text": "hi", "profile": "stg"})); got != "stg:hi" {
		t.Errorf("echo on stg = %q", got)
	}
	if got := text(t, call(t, d, "greet", map[string]any{"name": "a", "profile": "dev"})); got != "dev:hello a" {
		t.Errorf("greet on dev = %q", got)
	}

	// Tools that differ or are missing are rejected without calling the upstream.
	for _, profile := range []string{"stg", "prod"} {
		res := call(t, d, "greet", map[string]any{"name": "a", "profile": profile})
		want := `tool "greet" on profile "` + profile + `" differs from its definition in your tool list, so it was not called`
		if !res.IsError || !strings.Contains(text(t, res), want) {
			t.Errorf("greet on %s = %v %q", profile, res.IsError, text(t, res))
		}
	}
}

func TestSchemaMismatchWarnLazy(t *testing.T) {
	_, d := startProxy(t, warnConfig(config.Lazy{Enabled: true, MaxConnected: 1}, map[string]*mcp.Server{
		"dev": newGreetServer("dev", "same"),
		"stg": newGreetServer("stg", "changed"),
	}, t))

	res := call(t, d, "greet", map[string]any{"name": "a", "profile": "stg"})
	if !res.IsError || !strings.Contains(text(t, res), "differs from its definition in your tool list") {
		t.Errorf("greet on stg = %v %q", res.IsError, text(t, res))
	}
	waitStatuses(t, d, map[string]string{"dev": "idle", "stg": "active"})
	if w := warnings(t, d); w["stg"] == "" {
		t.Errorf("warnings = %v", w)
	}
	if got := text(t, call(t, d, "echo", map[string]any{"text": "hi", "profile": "stg"})); got != "stg:hi" {
		t.Errorf("echo on stg = %q", got)
	}
}

func TestSchemaMismatchWarnIgnoresExtraTools(t *testing.T) {
	_, d := startProxy(t, warnConfig(config.Lazy{}, map[string]*mcp.Server{
		"dev":  newUpstreamServer("dev", false),
		"prod": newUpstreamServer("prod", true),
	}, t))
	if w := warnings(t, d); w["prod"] != "" {
		t.Errorf("a profile with only extra tools should not warn: %v", w)
	}
}

func TestSchemaMismatchSilent(t *testing.T) {
	_, d := startProxy(t, mismatchConfig(config.SchemaMismatchSilent, config.Lazy{}, map[string]*mcp.Server{
		"dev": newGreetServer("dev", "same"),
		"stg": newGreetServer("stg", "missing"),
	}, t))

	// Nothing is reported in the listing.
	if s := listProfiles(t, d)["stg"]; s.Status != "active" || s.Warning != "" || s.MismatchedTools != nil {
		t.Errorf("stg = %+v", s)
	}
	// Calls to the missing tool are still rejected; the others work.
	res := call(t, d, "greet", map[string]any{"name": "a", "profile": "stg"})
	if !res.IsError || !strings.Contains(text(t, res), "differs from its definition in your tool list") {
		t.Errorf("greet on stg = %v %q", res.IsError, text(t, res))
	}
	if got := text(t, call(t, d, "echo", map[string]any{"text": "hi", "profile": "stg"})); got != "stg:hi" {
		t.Errorf("echo on stg = %q", got)
	}
}
