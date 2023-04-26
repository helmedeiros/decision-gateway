package proxy

import (
	"net/http"
	"sync"
	"time"

	"github.com/helmedeiros/decision-gateway/internal/gateway"
)

// BuildConfig captures the per-route build-time options the proxy
// needs to construct a Handler. The Holder reuses these every time
// Replace runs so all rebuilt routes share the same transport
// configuration, OTel wrapping, etc. See ADR-0008.
type BuildConfig struct {
	BackendTimeout   time.Duration
	Pool             PoolConfig
	Protocol         UpstreamProtocol
	TransportWrapper func(http.RoundTripper) http.RoundTripper
}

// Holder wraps a *Handler behind a sync.RWMutex (same pattern as
// markup-svc/ADR-0015). Reads (ServeHTTP) take the RLock just long
// enough to copy the handler pointer + release. Writes (Replace)
// build a fresh Handler from a new Router under the BuildConfig +
// atomic-swap-style WLock the pointer. The /admin/routes endpoint
// calls Replace.
type Holder struct {
	cfg     BuildConfig
	mu      sync.RWMutex
	current *Handler
}

func NewHolder(initialRouter *gateway.Router, cfg BuildConfig) (*Holder, error) {
	h, err := New(initialRouter, cfg.BackendTimeout, cfg.Pool, cfg.Protocol, cfg.TransportWrapper)
	if err != nil {
		return nil, err
	}
	return &Holder{cfg: cfg, current: h}, nil
}

// ServeHTTP delegates to the currently active Handler.
func (h *Holder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	cur := h.current
	h.mu.RUnlock()
	cur.ServeHTTP(w, r)
}

// Replace builds a new Handler from router using the Holder's
// captured BuildConfig and atomically swaps it in. Returns the
// build error without swapping if construction fails.
func (h *Holder) Replace(router *gateway.Router) error {
	next, err := New(router, h.cfg.BackendTimeout, h.cfg.Pool, h.cfg.Protocol, h.cfg.TransportWrapper)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.current = next
	h.mu.Unlock()
	return nil
}

// Routes returns a snapshot of the active route table for the
// /admin/routes GET handler.
func (h *Holder) Routes() []gateway.Route {
	h.mu.RLock()
	cur := h.current
	h.mu.RUnlock()
	return cur.router.Routes()
}
