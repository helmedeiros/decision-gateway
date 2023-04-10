package proxy_test

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/helmedeiros/decision-gateway/internal/gateway"
	"github.com/helmedeiros/decision-gateway/internal/proxy"
)

func TestProxy_UpstreamH2C_NegotiatesHTTP2(t *testing.T) {
	var upstreamProto string
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamProto = r.Proto
		w.WriteHeader(http.StatusOK)
	}))
	backend.Config.Handler = h2c.NewHandler(backend.Config.Handler, &http2.Server{})
	backend.Start()
	t.Cleanup(backend.Close)

	backendURL, _ := url.Parse(backend.URL)
	router, err := gateway.NewRouter([]gateway.Route{{Prefix: "/", Backend: backendURL}})
	if err != nil {
		t.Fatal(err)
	}
	h, err := proxy.New(router, 0, proxy.PoolConfig{}, proxy.UpstreamH2C, nil)
	if err != nil {
		t.Fatal(err)
	}

	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)

	resp, err := http.Get(gw.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if upstreamProto != "HTTP/2.0" {
		t.Errorf("upstream saw proto %q, want HTTP/2.0 (h2c not negotiated)", upstreamProto)
	}
}

func TestProxy_UpstreamHTTP1_StaysOn11(t *testing.T) {
	var upstreamProto string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamProto = r.Proto
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	backendURL, _ := url.Parse(backend.URL)
	router, _ := gateway.NewRouter([]gateway.Route{{Prefix: "/", Backend: backendURL}})
	h, _ := proxy.New(router, 0, proxy.PoolConfig{}, proxy.UpstreamHTTP1, nil)

	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)

	resp, _ := http.Get(gw.URL + "/x")
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if upstreamProto != "HTTP/1.1" {
		t.Errorf("upstream saw proto %q, want HTTP/1.1", upstreamProto)
	}
}

// Avoid unused-import lint on context/tls/net by referencing them via a
// no-op helper that the build keeps alive in this file too.
var _ = context.Background
var _ = (*tls.Config)(nil)
var _ = net.DefaultResolver
