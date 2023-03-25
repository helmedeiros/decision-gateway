package middleware

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	oteltrace "go.opentelemetry.io/otel/trace"
)

// RouteRecorder is implemented by the http.ResponseWriter AccessLog
// hands to inner handlers. The router (or proxy adapter) calls
// SetMatchedRoute(prefix) so AccessLog can report attrs.route after
// the inner handler returns. The wrapper-side rendezvous is the only
// place AccessLog can observe inner-frame state because
// request-context mutations made via r.WithContext do not propagate
// back to the outer http.Request the access log holds.
//
// Handlers that want to read the matched route from inside the
// response chain use a type-assertion: `if rec, ok := w.(RouteRecorder); ok`.
type RouteRecorder interface {
	SetMatchedRoute(prefix string)
}

// AccessLog wraps next so each request produces one JSON line on
// out describing the served response. The wire shape is fixed at
// {time, level=info, msg=gateway.access, attrs={method, path,
// status, duration_ms, route, correlation_id}} so an aggregator
// parses gateway access logs with the same schema traffic-gen
// uses for its own JSON events (see ADR-0001 + traffic-gen/internal/jsonlog).
//
// out is the structured-log sink (typically os.Stdout); writes are
// serialized via an internal sync.Mutex held only for the encode +
// write window so contended traffic does not interleave bytes.
// now is overrideable for tests; nil means time.Now.
func AccessLog(out io.Writer, now func() time.Time, next http.Handler) http.Handler {
	if now == nil {
		now = time.Now
	}
	var mu sync.Mutex
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := now()
		wrapped := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(wrapped, r)
		attrs := accessAttrs{
			Method:        r.Method,
			Path:          r.URL.Path,
			Status:        wrapped.status,
			DurationMS:    float64(now().Sub(start)) / float64(time.Millisecond),
			Route:         wrapped.route,
			CorrelationID: CorrelationIDFromContext(r.Context()),
		}
		// Trace correlation: when the OTel middleware ran ahead of
		// AccessLog (composition order CorrelationID(Middleware(AccessLog)))
		// and a span is active in the request context, write its trace
		// + span IDs onto the access entry so Kibana operators
		// filtering by attrs.correlation_id can hop to Jaeger via the
		// trace_id link. When the SpanContext is invalid (--otel-enabled
		// off, or this endpoint is not wrapped by the OTel middleware),
		// SpanContextFromContext returns a zero SpanContext whose
		// IsValid() is false; the fields stay omitted via omitempty.
		if sc := oteltrace.SpanContextFromContext(r.Context()); sc.IsValid() {
			attrs.TraceID = sc.TraceID().String()
			attrs.SpanID = sc.SpanID().String()
		}
		entry := accessEntry{
			Time:  start.UTC().Format(time.RFC3339Nano),
			Level: "info",
			Msg:   "gateway.access",
			Attrs: attrs,
		}
		mu.Lock()
		_ = json.NewEncoder(out).Encode(entry)
		mu.Unlock()
	})
}

type accessEntry struct {
	Time  string      `json:"time"`
	Level string      `json:"level"`
	Msg   string      `json:"msg"`
	Attrs accessAttrs `json:"attrs"`
}

type accessAttrs struct {
	Method        string  `json:"method"`
	Path          string  `json:"path"`
	Status        int     `json:"status"`
	DurationMS    float64 `json:"duration_ms"`
	Route         string  `json:"route,omitempty"`
	CorrelationID string  `json:"correlation_id,omitempty"`
	TraceID       string  `json:"trace_id,omitempty"`
	SpanID        string  `json:"span_id,omitempty"`
}

// statusRecorder wraps http.ResponseWriter so AccessLog can read
// the status the inner handler wrote and the matched route the
// router stamped via SetMatchedRoute. The wrapper assumes
// WriteHeader is called once; a handler that calls it multiple
// times keeps the first status (matches the http.Server default
// behavior of ignoring subsequent WriteHeader calls).
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	route       string
}

// SetMatchedRoute implements RouteRecorder.
func (s *statusRecorder) SetMatchedRoute(prefix string) {
	s.route = prefix
}

func (s *statusRecorder) WriteHeader(status int) {
	if s.wroteHeader {
		return
	}
	s.wroteHeader = true
	s.status = status
	s.ResponseWriter.WriteHeader(status)
}

// Write captures the implicit 200 when a handler writes without
// calling WriteHeader explicitly. Without this, statusRecorder
// would report 200 (the default) for handlers that wrote a body
// before WriteHeader -- still correct, but capturing the implicit
// transition keeps the wroteHeader bookkeeping consistent.
func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true
		// status stays at the StatusOK default set in AccessLog.
	}
	return s.ResponseWriter.Write(b)
}
