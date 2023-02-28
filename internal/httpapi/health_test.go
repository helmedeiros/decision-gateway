package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmedeiros/decision-gateway/internal/httpapi"
)

func TestHealthzReturns200OnGet(t *testing.T) {
	h := httpapi.Healthz()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "ok" {
		t.Errorf("body.status = %q, want ok", body["status"])
	}
}

func TestHealthzRejectsNonGetWith405(t *testing.T) {
	h := httpapi.Healthz()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/healthz", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 405", method, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != http.MethodGet {
			t.Errorf("%s Allow header = %q, want GET", method, got)
		}
	}
}

func TestReadyzReturns200WhenReady(t *testing.T) {
	h := httpapi.Readyz(func() (string, bool) { return "", true })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "ready" {
		t.Errorf("body.status = %q, want ready", body["status"])
	}
}

func TestReadyzReturns503WhenNotReady(t *testing.T) {
	h := httpapi.Readyz(func() (string, bool) { return "binding to listener", false })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "not_ready" {
		t.Errorf("body.status = %q, want not_ready", body["status"])
	}
	if body["reason"] != "binding to listener" {
		t.Errorf("body.reason = %q, want 'binding to listener'", body["reason"])
	}
}

func TestReadyzRejectsNonGetWith405(t *testing.T) {
	h := httpapi.Readyz(func() (string, bool) { return "", true })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/readyz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodGet {
		t.Errorf("Allow header = %q, want GET", got)
	}
}

// TestReadyzCallsReadyOnEveryProbe pins the contract that the
// Ready closure is re-evaluated per request, not cached at handler
// construction. A future drain feature flips the state from
// outside the handler; a cached closure result would render the
// drain invisible.
func TestReadyzCallsReadyOnEveryProbe(t *testing.T) {
	calls := 0
	h := httpapi.Readyz(func() (string, bool) {
		calls++
		return "", calls > 2 // not ready for the first two probes
	})
	for i := 0; i < 3; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/readyz", nil))
	}
	if calls != 3 {
		t.Errorf("Ready closure called %d times, want 3 (every probe)", calls)
	}
}
