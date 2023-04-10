package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/helmedeiros/decision-gateway/internal/gateway"
	"github.com/helmedeiros/decision-gateway/internal/middleware"
	"github.com/helmedeiros/decision-gateway/internal/proxy"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}

func newRouter(t *testing.T, routes ...gateway.Route) *gateway.Router {
	t.Helper()
	r, err := gateway.NewRouter(routes)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return r
}

func TestNewRejectsNilRouter(t *testing.T) {
	if _, err := proxy.New(nil, 0, proxy.PoolConfig{}, proxy.UpstreamHTTP1, nil); err == nil {
		t.Fatal("New accepted nil router; want error")
	}
}

func TestServeHTTPForwardsToMatchedBackend(t *testing.T) {
	var hits int64
	var sawPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		sawPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(backend.Close)

	router := newRouter(t, gateway.Route{Prefix: "/decide", Backend: mustURL(t, backend.URL)})
	h, err := proxy.New(router, 0, proxy.PoolConfig{}, proxy.UpstreamHTTP1, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/decide/v2", nil))

	if atomic.LoadInt64(&hits) != 1 {
		t.Errorf("backend hits = %d, want 1", hits)
	}
	if sawPath != "/decide/v2" {
		t.Errorf("backend saw path %q, want /decide/v2 (path preserved through proxy)", sawPath)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != `{"ok":true}` {
		t.Errorf("body = %q, want forwarded backend body", rec.Body.String())
	}
}

func TestServeHTTPPropagatesCorrelationIDToBackend(t *testing.T) {
	var sawCorrelationID string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawCorrelationID = r.Header.Get(middleware.CorrelationIDHeader)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	router := newRouter(t, gateway.Route{Prefix: "/decide", Backend: mustURL(t, backend.URL)})
	h, _ := proxy.New(router, 0, proxy.PoolConfig{}, proxy.UpstreamHTTP1, nil)

	// Wire CorrelationID outside the proxy so the context carries
	// the value before the proxy reads it. This mirrors the
	// production cmd composition.
	chain := middleware.CorrelationID(h)

	req := httptest.NewRequest(http.MethodGet, "/decide", nil)
	req.Header.Set(middleware.CorrelationIDHeader, "test-corr-789")
	chain.ServeHTTP(httptest.NewRecorder(), req)

	if sawCorrelationID != "test-corr-789" {
		t.Errorf("backend saw X-Correlation-ID = %q, want test-corr-789 (propagated through proxy)", sawCorrelationID)
	}
}

func TestServeHTTPStampsMatchedRouteOnWriter(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	router := newRouter(t, gateway.Route{Prefix: "/decide", Backend: mustURL(t, backend.URL)})
	h, _ := proxy.New(router, 0, proxy.PoolConfig{}, proxy.UpstreamHTTP1, nil)

	// The recorder type asserts a RouteRecorder for the access log.
	rec := &routeCapturingRecorder{ResponseWriter: httptest.NewRecorder()}
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/decide/anything", nil))

	if rec.captured != "/decide" {
		t.Errorf("matched route stamped = %q, want /decide", rec.captured)
	}
}

func TestServeHTTPReturns404ForUnmatchedPath(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("backend should not be hit on unmatched path")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	router := newRouter(t, gateway.Route{Prefix: "/decide", Backend: mustURL(t, backend.URL)})
	h, _ := proxy.New(router, 0, proxy.PoolConfig{}, proxy.UpstreamHTTP1, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("unmatched path status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no route matched") {
		t.Errorf("body = %q, want it to mention 'no route matched'", rec.Body.String())
	}
}

func TestServeHTTPDispatchesToLongestPrefix(t *testing.T) {
	var which string
	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		which = "short"
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(short.Close)
	long := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		which = "long"
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(long.Close)

	router := newRouter(t,
		gateway.Route{Prefix: "/decide", Backend: mustURL(t, short.URL)},
		gateway.Route{Prefix: "/decide/v2", Backend: mustURL(t, long.URL)},
	)
	h, _ := proxy.New(router, 0, proxy.PoolConfig{}, proxy.UpstreamHTTP1, nil)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/decide/v2/predict", nil))
	if which != "long" {
		t.Errorf("/decide/v2/predict dispatched to %q, want long (longest-prefix wins)", which)
	}

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/decide/legacy", nil))
	if which != "short" {
		t.Errorf("/decide/legacy dispatched to %q, want short", which)
	}
}

// routeCapturingRecorder wraps httptest.ResponseRecorder with the
// RouteRecorder interface so tests can assert the proxy stamps the
// matched prefix on the writer.
type routeCapturingRecorder struct {
	http.ResponseWriter
	captured string
}

func (r *routeCapturingRecorder) SetMatchedRoute(prefix string) {
	r.captured = prefix
}
