package proxy

import (
	"context"
	"fmt"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// progressRelay maps progress tokens sent upstream back to the client that asked for progress.
// Tokens are rewritten so that tokens from different clients never collide upstream.
type progressRelay struct {
	mu      sync.Mutex
	next    uint64
	targets map[string]progressTarget
}

type progressTarget struct {
	// ctx is the client's request context; notifications sent with it reach that request's stream.
	ctx     context.Context
	session *mcp.ServerSession
	token   any
}

func (r *progressRelay) register(ctx context.Context, session *mcp.ServerSession, token any) (string, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	proxyToken := fmt.Sprintf("mcp-profiles-%d", r.next)
	r.targets[proxyToken] = progressTarget{ctx: ctx, session: session, token: token}
	return proxyToken, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.targets, proxyToken)
	}
}

func (r *progressRelay) forward(_ context.Context, params *mcp.ProgressNotificationParams) {
	token, _ := params.ProgressToken.(string)
	r.mu.Lock()
	t, ok := r.targets[token]
	r.mu.Unlock()
	if !ok {
		return
	}
	_ = t.session.NotifyProgress(t.ctx, &mcp.ProgressNotificationParams{
		ProgressToken: t.token,
		Message:       params.Message,
		Progress:      params.Progress,
		Total:         params.Total,
	})
}
