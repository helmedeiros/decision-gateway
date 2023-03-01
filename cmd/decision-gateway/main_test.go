package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// freeListenAddr returns ":PORT" for an ephemeral port the OS hands
// us. Tests use this to avoid hardcoding ports that might collide
// with the developer's machine.
func freeListenAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	// Convert to host:port string with empty host so the http.Server
	// binds to all interfaces on the freed port.
	_, port, _ := net.SplitHostPort(addr)
	return ":" + port
}

func TestRunRejectsMissingRoute(t *testing.T) {
	err := run(context.Background(), []string{"--listen", ":18091"}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("run accepted no --route; want error")
	}
}

func TestRunRejectsMalformedRoute(t *testing.T) {
	err := run(context.Background(), []string{
		"--listen", ":18092",
		"--route", "missing-arrow-separator",
	}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("run accepted malformed --route; want error")
	}
}

// TestRunForwardsRequestsToBackend is the load-bearing integration
// test for the cmd binary. Spins up a backend httptest server, runs
// the gateway against it in a goroutine, fires a real HTTP request
// at the gateway, asserts the backend received the request with the
// correlation ID stamped and the gateway propagated the response.
func TestRunForwardsRequestsToBackend(t *testing.T) {
	var backendHits int64
	var backendSawCorrelationID string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&backendHits, 1)
		backendSawCorrelationID = r.Header.Get("X-Correlation-ID")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"served":"by-backend"}`))
	}))
	t.Cleanup(backend.Close)

	listen := freeListenAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout, stderr bytes.Buffer
	runErr := make(chan error, 1)
	go func() {
		runErr <- run(ctx, []string{
			"--listen", listen,
			"--route", "/decide=>" + backend.URL,
		}, &stdout, &stderr)
	}()

	// Wait for the gateway to be ready by polling /readyz.
	gatewayURL := "http://127.0.0.1" + listen
	waitForReady(t, gatewayURL)

	// Send a request with a known correlation ID. The gateway must
	// propagate it to the backend and echo it on the response.
	req, _ := http.NewRequest(http.MethodPost, gatewayURL+"/decide", strings.NewReader(`{"customer_tier":"enterprise"}`))
	req.Header.Set("X-Correlation-ID", "e2e-corr-1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /decide: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("gateway status = %d, want 200 (body=%s, stderr=%s)", resp.StatusCode, body, stderr.String())
	}
	if string(body) != `{"served":"by-backend"}` {
		t.Errorf("gateway body = %q, want backend body forwarded", body)
	}
	if got := resp.Header.Get("X-Correlation-ID"); got != "e2e-corr-1" {
		t.Errorf("response X-Correlation-ID = %q, want e2e-corr-1 (echoed by gateway)", got)
	}
	if backendSawCorrelationID != "e2e-corr-1" {
		t.Errorf("backend saw X-Correlation-ID = %q, want e2e-corr-1 (propagated through gateway)", backendSawCorrelationID)
	}
	if atomic.LoadInt64(&backendHits) != 1 {
		t.Errorf("backend hits = %d, want 1", backendHits)
	}

	// The boot event and the per-request access-log event should
	// both be on stdout.
	if !strings.Contains(stdout.String(), `"gateway.boot"`) {
		t.Errorf("stdout missing boot event:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), `"gateway.access"`) {
		t.Errorf("stdout missing access-log event:\n%s", stdout.String())
	}

	// Boot event should describe the route table.
	bootLine := strings.Split(stdout.String(), "\n")[0]
	var boot struct {
		Attrs struct {
			Listen string              `json:"listen"`
			Routes []map[string]string `json:"routes"`
		} `json:"attrs"`
	}
	if err := json.Unmarshal([]byte(bootLine), &boot); err != nil {
		t.Fatalf("decode boot line: %v", err)
	}
	if boot.Attrs.Listen != listen {
		t.Errorf("boot attrs.listen = %q, want %q", boot.Attrs.Listen, listen)
	}
	if len(boot.Attrs.Routes) != 1 || boot.Attrs.Routes[0]["prefix"] != "/decide" {
		t.Errorf("boot attrs.routes = %v, want one /decide route", boot.Attrs.Routes)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("run returned: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("run did not return within 3s of ctx cancel")
	}
}

func TestRunHealthzReturns200(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)
	listen := freeListenAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = run(ctx, []string{
			"--listen", listen,
			"--route", "/decide=>" + backend.URL,
		}, io.Discard, io.Discard)
	}()

	gatewayURL := "http://127.0.0.1" + listen
	waitForReady(t, gatewayURL)

	resp, err := http.Get(gatewayURL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", resp.StatusCode)
	}
}

// waitForReady polls /readyz until 200 or timeout. Polls every
// 20ms for up to 2s.
func waitForReady(t *testing.T, gatewayURL string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(gatewayURL + "/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("gateway never became ready")
}
