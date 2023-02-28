// Package httpapi ships the gateway's own HTTP surface (health
// probes today; future admin endpoints land here). The proxy
// adapter lives in internal/proxy; the cross-cutting middlewares
// live in internal/middleware. This package owns endpoints the
// gateway itself answers, not endpoints forwarded to a backend.
package httpapi

import (
	"encoding/json"
	"net/http"
)

// Ready is the closure cmd supplies to Readyz. It returns the
// current readiness state plus a reason string used on 503
// responses so operators see why the gateway is not yet ready.
// Mirrors markup-svc/internal/httpapi.Ready exactly so an operator
// reading both projects sees the same shape.
type Ready func() (reason string, ready bool)

// Healthz returns an http.Handler that responds 200 on GET with
// {"status":"ok"} and 405 with Allow: GET on any other method.
// The kubelet treats 200 as live and any other status as a request
// to restart the container.
func Healthz() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "method not allowed"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
}

// Readyz returns an http.Handler that calls ready() on every GET.
// 200 with {"status":"ready"} when ready returns true; 503 with
// {"status":"not_ready","reason":"<...>"} when ready returns false.
// 405 with Allow: GET on any other method.
//
// cmd flips its readiness state from "decider not bound" to "ready"
// the moment http.Server.ListenAndServe returns successfully from
// its bind. The closure is called on every probe so a future
// "drain" feature (flip to not-ready while serving in-flight) lands
// without changing the handler signature.
func Readyz(ready Ready) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "method not allowed"})
			return
		}
		reason, isReady := ready()
		w.Header().Set("Content-Type", "application/json")
		if isReady {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "not_ready",
			"reason": reason,
		})
	})
}
