// Package metrics ships the gateway-side Prometheus instrumentation:
// a per-request counter + duration histogram labeled by method /
// route / status. See ADR-0007.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/helmedeiros/decision-gateway/internal/middleware"
)

type Sink struct {
	count *prometheus.CounterVec
	dur   *prometheus.HistogramVec
}

func New() (*Sink, http.Handler) {
	reg := prometheus.NewRegistry()
	count := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_requests_total",
			Help: "Total gateway HTTP requests labeled by method / route / status.",
		},
		[]string{"method", "route", "status"},
	)
	dur := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gateway_request_duration_seconds",
			Help:    "Gateway HTTP request duration in seconds labeled by method / route / status.",
			Buckets: gatewayBuckets,
		},
		[]string{"method", "route", "status"},
	)
	reg.MustRegister(count, dur)
	handler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	return &Sink{count: count, dur: dur}, handler
}

// gatewayBuckets covers measured per-request latency (median ~400 µs,
// p99 ~3 ms post-pool-tuning). Goes to 2.5 s to bound timeout tail.
var gatewayBuckets = []float64{
	0.0001, 0.00025, 0.0005,
	0.001, 0.0025, 0.005, 0.01,
	0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5,
}

// Middleware wraps next so each request increments the counter +
// observes the duration histogram. Labels are method / route /
// status; the route is read via the existing RouteRecorder writer
// stamp (set by the proxy on a successful route match) or stays
// empty for unmatched paths so cardinality is bounded by the
// configured route table.
func (s *Sink) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		labels := prometheus.Labels{
			"method": r.Method,
			"route":  sw.route,
			"status": strconv.Itoa(sw.status),
		}
		s.count.With(labels).Inc()
		s.dur.With(labels).Observe(time.Since(start).Seconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	route       string
	wroteHeader bool
}

func (sw *statusWriter) WriteHeader(code int) {
	if sw.wroteHeader {
		return
	}
	sw.wroteHeader = true
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if !sw.wroteHeader {
		sw.wroteHeader = true
	}
	return sw.ResponseWriter.Write(b)
}

// SetMatchedRoute implements middleware.RouteRecorder so the proxy's
// existing writer-stamping path lights up this label too.
func (sw *statusWriter) SetMatchedRoute(route string) { sw.route = route }

var _ middleware.RouteRecorder = (*statusWriter)(nil)
