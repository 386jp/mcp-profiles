// Package config loads and validates the mcp-profiles configuration file.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// EnvConfigPath is the environment variable that points to the configuration file.
const EnvConfigPath = "MCP_PROFILES_CONFIG"

// DefaultConfigPath is used when EnvConfigPath is not set.
const DefaultConfigPath = "mcp-profiles.json"

// Values of schemaMismatch.
const (
	// SchemaMismatchFail fails the startup, or disables the profile once running.
	SchemaMismatchFail = "fail"
	// SchemaMismatchWarn keeps the profile, rejects calls to the tools that differ and
	// reports them in the profile listing tool.
	SchemaMismatchWarn = "warn"
	// SchemaMismatchSilent is SchemaMismatchWarn without the report, for intended differences
	// such as a read-only profile.
	SchemaMismatchSilent = "silent"
)

// Transport types shared by listen and profile entries.
const (
	TypeStdio = "stdio"
	TypeHTTP  = "http"
)

const (
	defaultProfileArg = "profile"
	defaultAddr       = "127.0.0.1:8000"
	defaultPath       = "/mcp"
	defaultLogLevel   = "info"
	defaultLogFormat  = "text"
	defaultStartup    = 30 * time.Second
	defaultShutdown   = 10 * time.Second

	defaultMaxConnected = 1
)

// Config is the root of the configuration file.
type Config struct {
	Listen           Listen             `json:"listen"`
	Log              Log                `json:"log"`
	Timeouts         Timeouts           `json:"timeouts"`
	Lazy             Lazy               `json:"lazy"`
	ProfileArg       string             `json:"profileArg"`
	BaseProfile      string             `json:"baseProfile"`
	DefaultProfile   string             `json:"defaultProfile"`
	SchemaMismatch   string             `json:"schemaMismatch"`
	ListProfilesTool string             `json:"listProfilesTool"`
	ReconnectTool    string             `json:"reconnectTool"`
	Profiles         map[string]Profile `json:"profiles"`
}

// Listen describes how mcp-profiles accepts connections from clients.
type Listen struct {
	Type    string            `json:"type"`
	Addr    string            `json:"addr"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
}

// Log configures the logger. Logs are always written to stderr.
type Log struct {
	Level  string `json:"level"`
	Format string `json:"format"`
}

// Timeouts holds the timeouts. Zero means no timeout.
type Timeouts struct {
	Startup  *Duration `json:"startup"`
	Call     *Duration `json:"call"`
	Shutdown *Duration `json:"shutdown"`
}

// ToleratesMismatch reports whether profiles whose tools differ stay available.
func (c *Config) ToleratesMismatch() bool {
	return c.SchemaMismatch == SchemaMismatchWarn || c.SchemaMismatch == SchemaMismatchSilent
}

// Lazy enables lazy mode, where upstreams are connected on first use and only the
// most recently used ones stay connected. It accepts true, false or an object.
type Lazy struct {
	Enabled bool
	// MaxConnected is how many of the most recently used upstreams stay connected.
	MaxConnected int
	// KeepBase keeps the base profile connected after its tools are read at startup.
	KeepBase bool
	// IdleTimeout disconnects the connected upstream after this long without calls. Zero means never.
	IdleTimeout time.Duration
}

func (l *Lazy) UnmarshalJSON(b []byte) error {
	var enabled bool
	if err := json.Unmarshal(b, &enabled); err == nil {
		*l = Lazy{Enabled: enabled}
		if enabled {
			l.MaxConnected = defaultMaxConnected
		}
		return nil
	}
	var obj struct {
		MaxConnected *int      `json:"maxConnected"`
		KeepBase     bool      `json:"keepBase"`
		IdleTimeout  *Duration `json:"idleTimeout"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&obj); err != nil {
		return fmt.Errorf("lazy must be a boolean or an object with maxConnected, keepBase and idleTimeout: %w", err)
	}
	maxConnected := defaultMaxConnected
	if obj.MaxConnected != nil {
		maxConnected = *obj.MaxConnected
	}
	if maxConnected < 1 {
		return fmt.Errorf("lazy.maxConnected must be at least 1: %d", maxConnected)
	}
	*l = Lazy{Enabled: true, MaxConnected: maxConnected, KeepBase: obj.KeepBase, IdleTimeout: durationOr(obj.IdleTimeout, 0)}
	return nil
}

// Profile is one upstream MCP server, in the same shape as an .mcp.json entry.
type Profile struct {
	Description string            `json:"description"`
	Type        string            `json:"type"`
	URL         string            `json:"url"`
	Headers     map[string]string `json:"headers"`
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
}

// Duration is a time.Duration encoded as a Go duration string such as "30s".
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"30s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	if v < 0 {
		return fmt.Errorf("duration must not be negative: %s", s)
	}
	*d = Duration(v)
	return nil
}

// StartupTimeout returns the per-upstream timeout for connecting and listing tools.
func (c *Config) StartupTimeout() time.Duration {
	return durationOr(c.Timeouts.Startup, defaultStartup)
}

// CallTimeout returns the timeout for a single tools/call. Zero means no timeout.
func (c *Config) CallTimeout() time.Duration { return durationOr(c.Timeouts.Call, 0) }

// ShutdownTimeout returns how long to wait for in-flight calls on shutdown.
func (c *Config) ShutdownTimeout() time.Duration {
	return durationOr(c.Timeouts.Shutdown, defaultShutdown)
}

func durationOr(d *Duration, def time.Duration) time.Duration {
	if d == nil {
		return def
	}
	return time.Duration(*d)
}

// Path returns the configuration file path from the environment.
func Path() string {
	if p := os.Getenv(EnvConfigPath); p != "" {
		return p
	}
	return DefaultConfigPath
}

// Load reads, expands, defaults and validates the configuration file at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(b, os.LookupEnv)
}

// Parse decodes a configuration from b. lookupEnv resolves ${VAR} references.
func Parse(b []byte, lookupEnv func(string) (string, bool)) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.expand(lookupEnv); err != nil {
		return nil, err
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Listen.Type == "" {
		c.Listen.Type = TypeStdio
	}
	if c.Listen.Type == TypeHTTP {
		if c.Listen.Addr == "" {
			c.Listen.Addr = defaultAddr
		}
		if c.Listen.Path == "" {
			c.Listen.Path = defaultPath
		}
	}
	if c.Log.Level == "" {
		c.Log.Level = defaultLogLevel
	}
	if c.Log.Format == "" {
		c.Log.Format = defaultLogFormat
	}
	if c.ProfileArg == "" {
		c.ProfileArg = defaultProfileArg
	}
	if c.SchemaMismatch == "" {
		c.SchemaMismatch = SchemaMismatchFail
	}
}

func (c *Config) validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	switch c.Listen.Type {
	case TypeStdio:
		if c.Listen.Addr != "" || c.Listen.Path != "" || len(c.Listen.Headers) > 0 {
			add("listen: addr, path and headers are only allowed for type %q", TypeHTTP)
		}
	case TypeHTTP:
		if !strings.HasPrefix(c.Listen.Path, "/") {
			add("listen.path must start with \"/\": %q", c.Listen.Path)
		}
	default:
		add("listen.type must be %q or %q: %q", TypeStdio, TypeHTTP, c.Listen.Type)
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("log.level must be one of debug, info, warn, error: %q", c.Log.Level)
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		add("log.format must be text or json: %q", c.Log.Format)
	}

	if len(c.Profiles) == 0 {
		add("profiles must not be empty")
	}
	if c.DefaultProfile != "" {
		if _, ok := c.Profiles[c.DefaultProfile]; !ok {
			add("defaultProfile %q is not defined in profiles", c.DefaultProfile)
		}
	}
	if c.BaseProfile != "" {
		if _, ok := c.Profiles[c.BaseProfile]; !ok {
			add("baseProfile %q is not defined in profiles", c.BaseProfile)
		}
	}
	switch c.SchemaMismatch {
	case SchemaMismatchFail, SchemaMismatchWarn, SchemaMismatchSilent:
	default:
		add("schemaMismatch must be %q, %q or %q: %q", SchemaMismatchFail, SchemaMismatchWarn, SchemaMismatchSilent, c.SchemaMismatch)
	}
	if c.ListProfilesTool != "" && c.ListProfilesTool == c.ReconnectTool {
		add("listProfilesTool and reconnectTool must differ: %q", c.ListProfilesTool)
	}
	for name, p := range c.Profiles {
		if name == "" {
			add("profiles: profile name must not be empty")
		}
		if p.Description != "" && c.ListProfilesTool == "" {
			add("profiles.%s.description is set but listProfilesTool is not, so it would never reach the client", name)
		}
		switch p.Type {
		case TypeStdio:
			if p.Command == "" {
				add("profiles.%s.command is required for type %q", name, TypeStdio)
			}
			if p.URL != "" || len(p.Headers) > 0 {
				add("profiles.%s: url and headers are only allowed for type %q", name, TypeHTTP)
			}
		case TypeHTTP:
			if p.URL == "" {
				add("profiles.%s.url is required for type %q", name, TypeHTTP)
			}
			if p.Command != "" || len(p.Args) > 0 || len(p.Env) > 0 {
				add("profiles.%s: command, args and env are only allowed for type %q", name, TypeStdio)
			}
		default:
			add("profiles.%s.type must be %q or %q: %q", name, TypeStdio, TypeHTTP, p.Type)
		}
	}
	return errors.Join(errs...)
}
