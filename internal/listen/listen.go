// Package listen accepts client connections over stdio or streamable HTTP.
package listen

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/386jp/mcp-profiles/internal/config"
)

// Serve serves server until ctx is done or the client goes away (stdio).
// sdkLogger receives the go-sdk HTTP handler's own logs.
func Serve(ctx context.Context, cfg config.Listen, server *mcp.Server, logger, sdkLogger *slog.Logger) error {
	if cfg.Type == config.TypeStdio {
		err := server.Run(ctx, &mcp.StdioTransport{})
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	return serveHTTP(ctx, cfg, server, logger, sdkLogger)
}

func serveHTTP(ctx context.Context, cfg config.Listen, server *mcp.Server, logger, sdkLogger *slog.Logger) error {
	if len(cfg.Headers) == 0 && !isLoopback(cfg.Addr) {
		logger.Warn("listening on a non-loopback address without listen.headers; anyone who can reach it can use every profile",
			"addr", cfg.Addr)
	}

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, Logger: sdkLogger})
	mux := http.NewServeMux()
	mux.Handle(cfg.Path, RequireHeaders(cfg.Headers, handler))

	srv := &http.Server{Addr: cfg.Addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	logger.Info("listening", "addr", ln.Addr().String(), "path", cfg.Path)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		// In-flight calls have already been drained by the proxy.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return nil
	}
}

// RequireHeaders rejects requests unless every configured header matches exactly.
func RequireHeaders(headers map[string]string, next http.Handler) http.Handler {
	if len(headers) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok := true
		for k, want := range headers {
			got := r.Header.Get(k)
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				ok = false
			}
		}
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
