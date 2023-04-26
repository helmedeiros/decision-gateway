package proxy_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/helmedeiros/decision-gateway/internal/gateway"
	"github.com/helmedeiros/decision-gateway/internal/proxy"
)

func TestHolder_ServeHTTPDelegatesToCurrent(t *testing.T) {
	backendA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Backend", "A")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backendA.Close)
	uA, _ := url.Parse(backendA.URL)

	router, _ := gateway.NewRouter([]gateway.Route{{Prefix: "/", Backend: uA}})
	h, err := proxy.NewHolder(router, proxy.BuildConfig{})
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	resp, _ := http.Get(srv.URL + "/decide")
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.Header.Get("X-Backend") != "A" {
		t.Errorf("backend = %q, want A", resp.Header.Get("X-Backend"))
	}
}

func TestHolder_ReplaceRoutesNewBackend(t *testing.T) {
	backendA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Backend", "A")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backendA.Close)
	backendB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Backend", "B")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backendB.Close)

	uA, _ := url.Parse(backendA.URL)
	uB, _ := url.Parse(backendB.URL)

	router, _ := gateway.NewRouter([]gateway.Route{{Prefix: "/", Backend: uA}})
	h, _ := proxy.NewHolder(router, proxy.BuildConfig{})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// Pre-replace: backend A.
	resp, _ := http.Get(srv.URL + "/x")
	resp.Body.Close()
	if got := resp.Header.Get("X-Backend"); got != "A" {
		t.Fatalf("pre-replace backend = %q, want A", got)
	}

	// Replace with backend B.
	newRouter, err := gateway.NewRouter([]gateway.Route{{Prefix: "/", Backend: uB}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Replace(newRouter); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	// Post-replace: backend B.
	resp, _ = http.Get(srv.URL + "/x")
	resp.Body.Close()
	if got := resp.Header.Get("X-Backend"); got != "B" {
		t.Errorf("post-replace backend = %q, want B", got)
	}
}

func TestHolder_RoutesReturnsCurrentSnapshot(t *testing.T) {
	uA, _ := url.Parse("http://backend-a:8080")
	router, _ := gateway.NewRouter([]gateway.Route{{Prefix: "/decide", Backend: uA}})
	h, _ := proxy.NewHolder(router, proxy.BuildConfig{})

	got := h.Routes()
	if len(got) != 1 || got[0].Prefix != "/decide" {
		t.Errorf("Routes() = %+v", got)
	}
}
