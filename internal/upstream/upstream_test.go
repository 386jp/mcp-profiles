package upstream

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/386jp/mcp-profiles/internal/config"
)

const envStdioServer = "MCP_PROFILES_TEST_STDIO_SERVER"

// TestMain lets the test binary act as a stdio upstream when re-executed.
func TestMain(m *testing.M) {
	if os.Getenv(envStdioServer) == "1" {
		runStdioServer()
		return
	}
	os.Exit(m.Run())
}

func runStdioServer() {
	s := mcp.NewServer(&mcp.Implementation{Name: "stdio-upstream"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "echo"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "sleep"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		time.Sleep(300 * time.Millisecond)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "slept"}}}, nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "exit"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		os.Exit(0)
		return nil, nil, nil
	})
	_ = s.Run(context.Background(), &mcp.StdioTransport{})
}

func stdioProfile() config.Profile {
	return config.Profile{Type: config.TypeStdio, Command: os.Args[0], Env: map[string]string{envStdioServer: "1"}}
}

func TestStdioProcessExitDisablesProfile(t *testing.T) {
	ctx := context.Background()
	u := New("local", stdioProfile(), Options{ConnectTimeout: 10 * time.Second})
	t.Cleanup(func() { _ = u.Close() })

	tools, err := u.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	u.SetExpected(tools)
	if s := u.State(); s.Status != StatusActive {
		t.Fatalf("state = %+v", s)
	}

	// The process exits while handling the call, so the call itself fails.
	if _, err := u.CallTool(ctx, &mcp.CallToolParams{Name: "exit"}); err == nil {
		t.Fatal("expected the call to fail")
	}
	deadline := time.Now().Add(5 * time.Second)
	for u.State().Status != StatusDisabled {
		if time.Now().After(deadline) {
			t.Fatalf("profile not disabled: %+v", u.State())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s := u.State(); !strings.Contains(s.Reason, "upstream process exited") {
		t.Errorf("reason = %q", s.Reason)
	}

	_, err = u.CallTool(ctx, &mcp.CallToolParams{Name: "echo"})
	if _, ok := err.(*UnavailableError); !ok {
		t.Errorf("call on disabled profile: err = %v", err)
	}

	if s := u.Reconnect(ctx); s.Status != StatusActive {
		t.Fatalf("reconnect = %+v", s)
	}
	res, err := u.CallTool(ctx, &mcp.CallToolParams{Name: "echo"})
	if err != nil || res.Content[0].(*mcp.TextContent).Text != "ok" {
		t.Errorf("echo after reconnect = %v, %v", res, err)
	}
}

func TestCloseDoesNotReportExit(t *testing.T) {
	u := New("local", stdioProfile(), Options{ConnectTimeout: 10 * time.Second})
	if _, err := u.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := u.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if s := u.State(); s.Reason != "closed" {
		t.Errorf("state after Close = %+v", s)
	}
}

func waitFor(t *testing.T, u *Upstream, want Status) State {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s := u.State()
		if s.Status == want {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %+v, want %s", s, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// newLazy returns a lazy stdio upstream whose tool snapshot is set, released to idle.
func newLazy(t *testing.T, idleTimeout time.Duration) *Upstream {
	t.Helper()
	u := New("local", stdioProfile(), Options{ConnectTimeout: 10 * time.Second, Lazy: true, IdleTimeout: idleTimeout})
	t.Cleanup(func() { _ = u.Close() })
	if s := u.State(); s.Status != StatusIdle {
		t.Fatalf("initial state = %+v", s)
	}
	tools, err := u.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	u.SetExpected(tools)
	u.Release()
	waitFor(t, u, StatusIdle)
	return u
}

func callText(t *testing.T, u *Upstream, name string) string {
	t.Helper()
	res, err := u.CallTool(context.Background(), &mcp.CallToolParams{Name: name})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	return res.Content[0].(*mcp.TextContent).Text
}

func TestLazyConnectsOnCallAndReleases(t *testing.T) {
	u := newLazy(t, 0)

	// The process stopped by Release must not disable the profile.
	time.Sleep(200 * time.Millisecond)
	if s := u.State(); s.Status != StatusIdle {
		t.Fatalf("after release = %+v", s)
	}

	if got := callText(t, u, "echo"); got != "ok" {
		t.Errorf("echo = %q", got)
	}
	waitFor(t, u, StatusActive)

	u.Release()
	waitFor(t, u, StatusIdle)
	if got := callText(t, u, "echo"); got != "ok" {
		t.Errorf("echo after release = %q", got)
	}
}

func TestLazyReleaseWaitsForRunningCall(t *testing.T) {
	u := newLazy(t, 0)
	callText(t, u, "echo") // connect

	done := make(chan string)
	go func() { done <- callText(t, u, "sleep") }()
	time.Sleep(100 * time.Millisecond)
	u.Release()
	if s := u.State(); s.Status != StatusActive {
		t.Fatalf("released while a call was running: %+v", s)
	}
	if got := <-done; got != "slept" {
		t.Errorf("sleep = %q", got)
	}
	waitFor(t, u, StatusIdle)
}

func TestLazyIdleTimeout(t *testing.T) {
	u := newLazy(t, 200*time.Millisecond)
	callText(t, u, "echo")
	if s := u.State(); s.Status != StatusActive {
		t.Fatalf("after call = %+v", s)
	}
	waitFor(t, u, StatusIdle)
}

func TestLazyProcessExitDisables(t *testing.T) {
	u := newLazy(t, 0)
	_, _ = u.CallTool(context.Background(), &mcp.CallToolParams{Name: "exit"})
	if s := waitFor(t, u, StatusDisabled); !strings.Contains(s.Reason, "upstream process exited") {
		t.Errorf("reason = %q", s.Reason)
	}
	if _, err := u.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo"}); err == nil {
		t.Error("a disabled lazy profile must not reconnect on its own")
	}
}
