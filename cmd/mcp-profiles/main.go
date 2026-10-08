// Command mcp-profiles bundles MCP servers that share the same tools and
// routes each tool call to one of them by a profile argument.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/386jp/mcp-profiles/internal/config"
	"github.com/386jp/mcp-profiles/internal/listen"
	"github.com/386jp/mcp-profiles/internal/proxy"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	flag.Usage = func() {
		_, _ = fmt.Fprintf(flag.CommandLine.Output(), `Usage: mcp-profiles [--version]

mcp-profiles is configured entirely by its configuration file.
The file is read from $%s, or ./%s if it is not set.
`, config.EnvConfigPath, config.DefaultConfigPath)
	}
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if flag.NArg() > 0 {
		flag.Usage()
		os.Exit(2)
	}

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mcp-profiles:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(config.Path())
	if err != nil {
		return err
	}
	logger, sdkLogger := newLoggers(cfg.Log)

	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	p := proxy.New(cfg, proxy.Options{Logger: logger, SDKLogger: sdkLogger, Version: version})
	if err := p.Start(signalCtx); err != nil {
		return err
	}
	defer p.Close()

	// Serving continues after a signal until in-flight calls drain, so it gets its own context.
	serveCtx, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()
	served := make(chan error, 1)
	go func() { served <- listen.Serve(serveCtx, cfg.Listen, p.Server(), logger, sdkLogger) }()

	select {
	case err := <-served:
		return err
	case <-signalCtx.Done():
	}

	logger.Info("shutting down")
	drainCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout())
	defer cancel()
	if err := p.Shutdown(drainCtx); err != nil {
		logger.Warn("shutdown timed out", "error", err)
	}
	cancelServe()
	return <-served
}

// newLoggers returns the application logger and a quieter one for the go-sdk,
// which logs every stateless HTTP request at info. The go-sdk logs below warn only at debug level.
func newLoggers(cfg config.Log) (logger, sdkLogger *slog.Logger) {
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.Level))
	sdkLevel := max(level, slog.LevelWarn)
	if level == slog.LevelDebug {
		sdkLevel = level
	}
	return slog.New(newHandler(cfg.Format, level)), slog.New(newHandler(cfg.Format, sdkLevel))
}

func newHandler(format string, level slog.Level) slog.Handler {
	opts := &slog.HandlerOptions{Level: level}
	if format == "json" {
		return slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.NewTextHandler(os.Stderr, opts)
}
