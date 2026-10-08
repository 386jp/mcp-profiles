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
