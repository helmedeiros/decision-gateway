package middleware_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmedeiros/decision-gateway/internal/middleware"
)

type recorder struct {
	http.ResponseWriter
	route string
}

func (r *recorder) SetMatchedRoute(s string) { r.route = s }

func TestAccessLog_SetMatchedRoute_ChainsToUnderlying(t *testing.T) {
	outer := &recorder{ResponseWriter: httptest.NewRecorder()}
	handler := middleware.AccessLog(io.Discard, nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if rec, ok := w.(middleware.RouteRecorder); ok {
			rec.SetMatchedRoute("/decide")
		}
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/decide", nil)
	handler.ServeHTTP(outer, req)

	if outer.route != "/decide" {
		t.Errorf("outer recorder route = %q, want /decide", outer.route)
	}
}
