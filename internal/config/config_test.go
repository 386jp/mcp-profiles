package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestParseDefaults(t *testing.T) {
	c, err := Parse([]byte(`{"profiles": {"dev": {"type": "http", "url": "http://localhost/mcp"}}}`), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen.Type != TypeStdio || c.ProfileArg != "profile" || c.Log.Level != "info" || c.Log.Format != "text" {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if c.StartupTimeout() != 30*time.Second || c.CallTimeout() != 0 || c.ShutdownTimeout() != 10*time.Second {
		t.Errorf("unexpected timeouts: %v %v %v", c.StartupTimeout(), c.CallTimeout(), c.ShutdownTimeout())
	}
}

func TestParseHTTPListenDefaults(t *testing.T) {
	c, err := Parse([]byte(`{
		"listen": {"type": "http"},
		"timeouts": {"call": "2m"},
		"profiles": {"dev": {"type": "stdio", "command": "x"}}
	}`), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen.Addr != "127.0.0.1:8000" || c.Listen.Path != "/mcp" {
		t.Errorf("unexpected listen: %+v", c.Listen)
	}
	if c.CallTimeout() != 2*time.Minute {
		t.Errorf("call timeout = %v", c.CallTimeout())
	}
}

func TestParseExpandsEnv(t *testing.T) {
	c, err := Parse([]byte(`{
		"listen": {"type": "http", "headers": {"Authorization": "Bearer ${TOKEN}"}},
		"profiles": {
			"dev": {"type": "http", "url": "${HOST:-http://localhost}/mcp", "headers": {"X-Key": "${KEY}"}},
			"prod": {"type": "stdio", "command": "${CMD}", "args": ["--dir", "${DIR}"], "env": {"A": "${KEY}"}}
		}
	}`), env(map[string]string{"TOKEN": "t", "KEY": "k", "CMD": "run", "DIR": "/d"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Listen.Headers["Authorization"]; got != "Bearer t" {
		t.Errorf("listen header = %q", got)
	}
	dev, prod := c.Profiles["dev"], c.Profiles["prod"]
	if dev.URL != "http://localhost/mcp" || dev.Headers["X-Key"] != "k" {
		t.Errorf("dev = %+v", dev)
	}
	if prod.Command != "run" || prod.Args[1] != "/d" || prod.Env["A"] != "k" {
		t.Errorf("prod = %+v", prod)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name string
		json string
		want string
	}{
		{"unknown field", `{"profiles": {"dev": {"type": "http", "url": "u", "oauth": {}}}}`, "unknown field"},
		{"no profiles", `{}`, "profiles must not be empty"},
		{"sse type", `{"profiles": {"dev": {"type": "sse", "url": "u"}}}`, `profiles.dev.type must be`},
		{"stdio without command", `{"profiles": {"dev": {"type": "stdio"}}}`, "command is required"},
		{"http without url", `{"profiles": {"dev": {"type": "http"}}}`, "url is required"},
		{"http with command", `{"profiles": {"dev": {"type": "http", "url": "u", "command": "c"}}}`, "only allowed for type \"stdio\""},
		{"unknown default", `{"defaultProfile": "x", "profiles": {"dev": {"type": "http", "url": "u"}}}`, `defaultProfile "x"`},
		{"description without list tool", `{"profiles": {"dev": {"type": "http", "url": "u", "description": "d"}}}`, "listProfilesTool is not"},
		{"same builtin names", `{"listProfilesTool": "t", "reconnectTool": "t", "profiles": {"dev": {"type": "http", "url": "u"}}}`, "must differ"},
		{"stdio listen with addr", `{"listen": {"type": "stdio", "addr": ":1"}, "profiles": {"dev": {"type": "http", "url": "u"}}}`, "only allowed for type \"http\""},
		{"bad path", `{"listen": {"type": "http", "path": "mcp"}, "profiles": {"dev": {"type": "http", "url": "u"}}}`, "must start with"},
		{"bad level", `{"log": {"level": "trace"}, "profiles": {"dev": {"type": "http", "url": "u"}}}`, "log.level"},
		{"negative duration", `{"timeouts": {"call": "-1s"}, "profiles": {"dev": {"type": "http", "url": "u"}}}`, "negative"},
		{"unset env", `{"profiles": {"dev": {"type": "http", "url": "${NOPE}"}}}`, "NOPE is not set"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.json), env(nil))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestParseLazy(t *testing.T) {
	const profiles = `"profiles": {"dev": {"type": "http", "url": "u"}}`
	tests := []struct {
		name string
		lazy string
		want Lazy
	}{
		{"omitted", ``, Lazy{}},
		{"false", `"lazy": false,`, Lazy{}},
		{"true", `"lazy": true,`, Lazy{Enabled: true, MaxConnected: 1}},
		{"empty object", `"lazy": {},`, Lazy{Enabled: true, MaxConnected: 1}},
		{"options", `"lazy": {"maxConnected": 2, "keepBase": true, "idleTimeout": "10m"},`, Lazy{Enabled: true, MaxConnected: 2, KeepBase: true, IdleTimeout: 10 * time.Minute}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Parse([]byte(`{`+tt.lazy+profiles+`}`), env(nil))
			if err != nil {
				t.Fatal(err)
			}
			if c.Lazy != tt.want {
				t.Errorf("lazy = %+v, want %+v", c.Lazy, tt.want)
			}
		})
	}

	for _, bad := range []string{`"lazy": {"keepAlive": 2},`, `"lazy": {"maxConnected": 0},`, `"lazy": {"idleTimeout": "-1s"},`, `"lazy": "yes",`} {
		if _, err := Parse([]byte(`{`+bad+profiles+`}`), env(nil)); err == nil || !strings.Contains(err.Error(), "lazy") {
			t.Errorf("Parse(%s) err = %v, want a lazy error", bad, err)
		}
	}
}

func TestParseSchemaOptions(t *testing.T) {
	const profiles = `"profiles": {"dev": {"type": "http", "url": "u"}, "prod": {"type": "http", "url": "u"}}`
	c, err := Parse([]byte(`{`+profiles+`}`), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.SchemaMismatch != SchemaMismatchFail || c.BaseProfile != "" {
		t.Errorf("defaults: schemaMismatch = %q, baseProfile = %q", c.SchemaMismatch, c.BaseProfile)
	}

	c, err = Parse([]byte(`{"baseProfile": "prod", "schemaMismatch": "warn", `+profiles+`}`), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.SchemaMismatch != SchemaMismatchWarn || c.BaseProfile != "prod" {
		t.Errorf("schemaMismatch = %q, baseProfile = %q", c.SchemaMismatch, c.BaseProfile)
	}

	for bad, want := range map[string]string{
		`"baseProfile": "stg",`:       `baseProfile "stg"`,
		`"schemaMismatch": "ignore",`: "schemaMismatch must be",
		`"schemaMismatch": "warn ",`:  "schemaMismatch must be",
	} {
		if _, err := Parse([]byte(`{`+bad+profiles+`}`), env(nil)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%s) err = %v, want containing %q", bad, err, want)
		}
	}
}

func TestParseHiddenAndDisabled(t *testing.T) {
	c, err := Parse([]byte(`{"profiles": {
		"a": {"type": "http", "url": "u", "disabled": true},
		"b": {"type": "http", "url": "u", "hidden": true},
		"c": {"type": "http", "url": "u"}
	}}`), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	// The implicit base skips disabled profiles but may be hidden.
	if got := c.Base(); got != "b" {
		t.Errorf("Base() = %q, want b", got)
	}

	tests := []struct {
		name string
		json string
		want string
	}{
		{"hidden default", `{"defaultProfile": "a", "profiles": {"a": {"type": "http", "url": "u", "hidden": true}, "b": {"type": "http", "url": "u"}}}`, `defaultProfile "a" must be neither hidden nor disabled`},
		{"disabled default", `{"defaultProfile": "a", "profiles": {"a": {"type": "http", "url": "u", "disabled": true}, "b": {"type": "http", "url": "u"}}}`, `defaultProfile "a" must be neither hidden nor disabled`},
		{"disabled base", `{"baseProfile": "a", "profiles": {"a": {"type": "http", "url": "u", "disabled": true}, "b": {"type": "http", "url": "u"}}}`, `baseProfile "a" must not be disabled`},
		{"nothing callable", `{"profiles": {"a": {"type": "http", "url": "u", "hidden": true}, "b": {"type": "http", "url": "u", "disabled": true}}}`, "at least one profile must be neither hidden nor disabled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.json), env(nil))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}
