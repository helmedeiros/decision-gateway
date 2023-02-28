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

// New constructs a Handler from router. backendTimeout sets the
// per-request HTTP client timeout for outbound calls to backends;
// a value of 0 leaves the default (no timeout) and is reasonable
// only for tests. Per-prefix proxies are built once at construction
// and reused for every matching request; httputil.ReverseProxy
// internally pools connections so the steady-state cost is one
// reused HTTP/1.1 connection per backend.
func New(router *gateway.Router, backendTimeout time.Duration) (*Handler, error) {
	if router == nil {
		return nil, fmt.Errorf("router is required")
	}
	proxies := make(map[string]*httputil.ReverseProxy, len(router.Routes()))
	for _, route := range router.Routes() {
		rp := newReverseProxy(route.Backend, backendTimeout)
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
// requested.
func newReverseProxy(backend *url.URL, timeout time.Duration) *httputil.ReverseProxy {
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
	if timeout > 0 {
		rp.Transport = &http.Transport{
			ResponseHeaderTimeout: timeout,
		}
	}
	return rp
}

// notFound is the handler invoked when Router.Match returns false.
// The body is intentionally opaque so a misconfigured route table
// does not leak the gateway's internal layout to clients.
func notFound(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":"no route matched"}` + "\n"))
}
