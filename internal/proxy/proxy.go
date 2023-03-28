package proxy

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/helmedeiros/decision-gateway/internal/gateway"
	"github.com/helmedeiros/decision-gateway/internal/middleware"
)

// Handler is the http.Handler returned by New. It dispatches each
// inbound request to the matched route's backend via a per-route
// httputil.ReverseProxy, propagating the correlation ID on the
// outbound request and stamping the matched-route prefix on the
// writer wrapper so the access-log middleware reports it.
type Handler struct {
	router   *gateway.Router
	proxies  map[string]*httputil.ReverseProxy
	notFound http.Handler
}

// PoolConfig tunes the per-route HTTP transport's connection pool.
// Zero values fall back to Go's stdlib defaults — which include
// MaxIdleConnsPerHost=2, a cliff that forces the gateway to constantly
// open + close TCP connections to a single backend at typical
// platform QPS. See ADR-0005.
type PoolConfig struct {
	// MaxIdleConnsPerHost is the upper bound on idle keep-alive
	// connections retained per backend host. Default (zero) falls
	// back to http.DefaultMaxIdleConnsPerHost = 2 which is a
	// stdlib-historical setting for general-purpose clients, not
	// a service-to-service gateway. The gateway runs against a
	// small number of backends (1 in the canonical compose; a
	// handful in multi-route deployments) and benefits from a
	// large per-host pool. Operators tune via --upstream-max-idle-conns.
	MaxIdleConnsPerHost int

	// IdleConnTimeout is how long an idle keep-alive connection
	// stays in the pool before it is closed. Zero = never expire,
	// which is wrong for production (NAT tables time out at ~5min
	// and a stale conn from the pool then fails on the next call).
	// New() applies a 90s default matching http.DefaultTransport
	// when this is zero.
	IdleConnTimeout time.Duration
}

// New constructs a Handler from router. backendTimeout sets the
// per-request response-header timeout for outbound calls to backends;
// a value of 0 leaves the default (no timeout) and is reasonable
// only for tests. pool tunes the per-route Transport's connection
// pool — see PoolConfig. Per-prefix proxies are built once at
// construction and reused for every matching request; the connection
// pool persists for the gateway's lifetime so steady-state cost is
// one reused HTTP/1.1 connection per backend per pool slot.
//
// transportWrapper, when non-nil, wraps each per-route reverse proxy's
// outbound RoundTripper. The OTel-enabled binary passes
// internal/observability/otel.InstrumentedTransport here so the
// upstream call carries a traceparent header and emits a
// gateway.proxy.upstream child span (see ADR-0002). When nil, the
// proxies use the tuned Transport unchanged.
func New(router *gateway.Router, backendTimeout time.Duration, pool PoolConfig, transportWrapper func(http.RoundTripper) http.RoundTripper) (*Handler, error) {
	if router == nil {
		return nil, fmt.Errorf("router is required")
	}
	proxies := make(map[string]*httputil.ReverseProxy, len(router.Routes()))
	for _, route := range router.Routes() {
		rp := newReverseProxy(route.Backend, backendTimeout, pool, transportWrapper)
		proxies[route.Prefix] = rp
	}
	return &Handler{
		router:   router,
		proxies:  proxies,
		notFound: http.HandlerFunc(notFound),
	}, nil
}

// ServeHTTP matches the request path against the configured routes,
// stamps the matched route on the writer wrapper, propagates the
// correlation ID on the outbound request, and delegates to the
// per-route reverse proxy. A non-matching request gets the notFound
// 404 handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route, ok := h.router.Match(r.URL.Path)
	if !ok {
		h.notFound.ServeHTTP(w, r)
		return
	}
	if rec, ok := w.(middleware.RouteRecorder); ok {
		rec.SetMatchedRoute(route.Prefix)
	}
	if id := middleware.CorrelationIDFromContext(r.Context()); id != "" {
		// Stamp the correlation ID on the outbound request so the
		// backend sees the same value the gateway received or
		// minted. ReverseProxy's Director runs per request and can
		// mutate r.Header safely because the proxy operates on a
		// copy of the request.
		r.Header.Set(middleware.CorrelationIDHeader, id)
	}
	h.proxies[route.Prefix].ServeHTTP(w, r)
}

// newReverseProxy builds an httputil.ReverseProxy that targets
// backend. The Director rewrites the outbound request's
// URL.Scheme, URL.Host, and Host header from backend; URL.Path is
// preserved verbatim so the backend sees the same path the client
// requested. The Transport is configured with the per-host idle
// pool sized via pool.MaxIdleConnsPerHost so steady-state QPS does
// not force constant TCP open + close per request. See ADR-0005.
func newReverseProxy(backend *url.URL, timeout time.Duration, pool PoolConfig, transportWrapper func(http.RoundTripper) http.RoundTripper) *httputil.ReverseProxy {
	rp := httputil.NewSingleHostReverseProxy(backend)
	// httputil.NewSingleHostReverseProxy's default Director rewrites
	// scheme, host, and URL.Path; we keep the path-rewrite default
	// off because the inbound request already carries the full path
	// the backend expects (the route's prefix is part of the URL,
	// not a base path to strip). The default Director does that
	// already when backend.Path is empty -- which it is in
	// production deployments where the operator passes
	// http://markup-svc:8080 as the backend. Documented here so a
	// future refactor that adds a backend Path does not accidentally
	// prepend it.
	rp.Transport = newTunedTransport(timeout, pool, transportWrapper)
	return rp
}

// newTunedTransport builds the per-route Transport applying the
// pool tunings + optional response-header timeout + optional OTel
// wrap. Default values applied here when PoolConfig fields are zero:
//
//   - MaxIdleConnsPerHost: 128 (vs stdlib default of 2). Operators
//     tune via the cmd flag; the 128 default fits a single-backend
//     compose plus headroom for QPS bursts. Higher than 128 is fine;
//     the cost is one TCP socket per slot.
//   - IdleConnTimeout: 90s (matches http.DefaultTransport). Production
//     NAT tables time out at ~5min, so an idle conn older than that
//     becomes a stale read on the next reuse. 90s keeps us under
//     every realistic NAT timeout.
//
// Other fields take stdlib defaults (DisableKeepAlives=false,
// TLSHandshakeTimeout=10s, ExpectContinueTimeout=1s).
func newTunedTransport(timeout time.Duration, pool PoolConfig, transportWrapper func(http.RoundTripper) http.RoundTripper) http.RoundTripper {
	maxIdlePerHost := pool.MaxIdleConnsPerHost
	if maxIdlePerHost <= 0 {
		maxIdlePerHost = 128
	}
	idleTimeout := pool.IdleConnTimeout
	if idleTimeout <= 0 {
		idleTimeout = 90 * time.Second
	}
	t := &http.Transport{
		MaxIdleConns:          0, // unlimited total; the per-host cap is the real lever
		MaxIdleConnsPerHost:   maxIdlePerHost,
		MaxConnsPerHost:       0, // unlimited active; we are not a rate limiter
		IdleConnTimeout:       idleTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: timeout,
	}
	var base http.RoundTripper = t
	if transportWrapper != nil {
		base = transportWrapper(base)
	}
	return base
}

// notFound is the handler invoked when Router.Match returns false.
// The body is intentionally opaque so a misconfigured route table
// does not leak the gateway's internal layout to clients.
func notFound(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":"no route matched"}` + "\n"))
}
