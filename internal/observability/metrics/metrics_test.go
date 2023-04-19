package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmedeiros/decision-gateway/internal/middleware"
	"github.com/helmedeiros/decision-gateway/internal/observability/metrics"
)

func TestSink_RecordsCounterAndHistogram(t *testing.T) {
	sink, handler := metrics.New()

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped := sink.Middleware(inner)
	for i := 0; i < 3; i++ {
		wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/decide", nil))
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	if !strings.Contains(body, `gateway_requests_total{method="POST",route="",status="200"} 3`) {
		t.Errorf("counter missing/wrong; body:\n%s", body)
	}
	if !strings.Contains(body, `gateway_request_duration_seconds_count{method="POST",route="",status="200"} 3`) {
		t.Errorf("histogram count missing/wrong; body:\n%s", body)
	}
}

// proxyStub mimics what the real proxy.Handler does: stamps the
// matched route on the writer wrapper via the RouteRecorder interface.
type proxyStub struct{ route string }

func (p proxyStub) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	if rec, ok := w.(middleware.RouteRecorder); ok {
		rec.SetMatchedRoute(p.route)
	}
	w.WriteHeader(http.StatusNotFound)
}

func TestSink_PicksUpMatchedRouteLabel(t *testing.T) {
	sink, handler := metrics.New()
	wrapped := sink.Middleware(proxyStub{route: "/decide"})
	wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/decide", nil))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `gateway_requests_total{method="POST",route="/decide",status="404"} 1`) {
		t.Errorf("route label not picked up; body:\n%s", rec.Body.String())
	}
}
