package middleware_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"

	"github.com/helmedeiros/decision-gateway/internal/middleware"
)

// uuidV4Pattern matches lowercase 8-4-4-4-12 hex with the version
// nibble pinned to 4 and the variant nibble pinned to 8/9/a/b.
// Catches a regression that minted a non-v4 ID or used uppercase.
var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestCorrelationIDMintsWhenHeaderAbsent(t *testing.T) {
	var seen string
	h := middleware.CorrelationID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = middleware.CorrelationIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/decide", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen == "" {
		t.Fatal("middleware did not stash a correlation ID in context")
	}
	if !uuidV4Pattern.MatchString(seen) {
		t.Errorf("minted ID %q does not match UUID v4 pattern", seen)
	}
	if got := rec.Header().Get(middleware.CorrelationIDHeader); got != seen {
		t.Errorf("response header X-Correlation-ID = %q, want %q", got, seen)
	}
}

func TestCorrelationIDPreservesInboundHeader(t *testing.T) {
	const inbound = "abc-1234-test"
	var seen string
	h := middleware.CorrelationID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = middleware.CorrelationIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/decide", nil)
	req.Header.Set(middleware.CorrelationIDHeader, inbound)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen != inbound {
		t.Errorf("context value = %q, want %q (inbound preserved)", seen, inbound)
	}
	if got := rec.Header().Get(middleware.CorrelationIDHeader); got != inbound {
		t.Errorf("response header = %q, want %q (echoed)", got, inbound)
	}
}

func TestCorrelationIDFromContextReturnsEmptyWhenAbsent(t *testing.T) {
	if got := middleware.CorrelationIDFromContext(context.Background()); got != "" {
		t.Errorf("CorrelationIDFromContext on bare context = %q, want empty", got)
	}
}

// TestCorrelationIDMintsUniquePerRequest pins that two consecutive
// requests through the middleware get distinct minted IDs. A
// regression that used a process-level seed without re-randomizing
// (or returned a static UUID) would fail here.
func TestCorrelationIDMintsUniquePerRequest(t *testing.T) {
	seen := map[string]bool{}
	h := middleware.CorrelationID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[middleware.CorrelationIDFromContext(r.Context())] = true
		w.WriteHeader(http.StatusOK)
	}))
	for i := 0; i < 16; i++ {
		req := httptest.NewRequest(http.MethodGet, "/decide", nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if len(seen) != 16 {
		t.Errorf("16 requests minted %d distinct IDs; want 16", len(seen))
	}
}

// TestCorrelationIDFallsBackToSentinelOnRandReadFailure pins the
// last-resort behavior: when crypto/rand.Read fails (a pathological
// OS RNG state), the middleware mints a deterministic sentinel ID
// rather than an empty string so downstream filters do not silently
// drop the request. A regression that returned "" would let an
// observability dashboard miss correlated-but-failed traffic.
func TestCorrelationIDFallsBackToSentinelOnRandReadFailure(t *testing.T) {
	prev := middleware.SetRandReadForTesting(func([]byte) (int, error) {
		return 0, errors.New("simulated rng failure")
	})
	defer middleware.SetRandReadForTesting(prev)

	var seen string
	h := middleware.CorrelationID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = middleware.CorrelationIDFromContext(r.Context())
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/decide", nil))

	if seen == "" {
		t.Error("middleware returned empty string on RNG failure; want sentinel UUID")
	}
	if seen != "00000000-0000-4000-8000-000000000000" {
		t.Errorf("sentinel UUID = %q, want fixed all-zero v4 form", seen)
	}
}

// TestCorrelationIDConcurrentRequestsDoNotInterfere pins that the
// context value stored per request stays isolated under concurrent
// inbound traffic. A regression that used a package-level variable
// instead of the per-request context would fail here.
func TestCorrelationIDConcurrentRequestsDoNotInterfere(t *testing.T) {
	var (
		mu     sync.Mutex
		seen   = map[string]string{} // inbound -> ctx
		wg     sync.WaitGroup
		errors int
	)
	h := middleware.CorrelationID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inbound := r.Header.Get(middleware.CorrelationIDHeader)
		ctx := middleware.CorrelationIDFromContext(r.Context())
		mu.Lock()
		seen[inbound] = ctx
		if inbound != ctx {
			errors++
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))

	const requests = 64
	wg.Add(requests)
	for i := 0; i < requests; i++ {
		go func(i int) {
			defer wg.Done()
			id := "concurrent-" + string(rune('a'+i%26)) + "-" + string(rune('0'+i/26))
			req := httptest.NewRequest(http.MethodGet, "/decide", nil)
			req.Header.Set(middleware.CorrelationIDHeader, id)
			h.ServeHTTP(httptest.NewRecorder(), req)
		}(i)
	}
	wg.Wait()
	if errors > 0 {
		t.Errorf("%d/%d concurrent requests saw a context value different from their inbound header", errors, requests)
	}
}
