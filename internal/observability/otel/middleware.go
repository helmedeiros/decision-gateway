package otel

import (
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Middleware returns an HTTP middleware that opens one span per
// inbound request named "gateway.request". The span is rooted at
// the trace context extracted from the request headers (so a
// caller emitting W3C traceparent becomes the parent span) or
// rooted at a new trace if no parent context is present.
//
// Attributes set on the span:
//
//   - http.method        — the request method
//   - http.target        — the request URL path
//   - http.route         — set later via SetRoute when the proxy
//     matches a configured route prefix (empty for unmatched
//     requests so 404 spans still carry the inbound path)
//   - http.status_code   — captured from the response writer at
//     handler completion via a status-recording wrapper
//   - gateway.duration_ms — wall-clock duration in milliseconds
//
// Span status: when the response status >= 500 the span is marked
// as Error; 4xx is left as OK because a malformed-input 400 is not
// a gateway failure. This matches the access-log middleware's
// existing severity policy.
//
// Composition: place Middleware INSIDE CorrelationID so the
// correlation ID is in the context when the span starts and the
// span gains a gateway.correlation_id attribute; place it OUTSIDE
// AccessLog so AccessLog can later read trace_id + span_id from
// the context and log them with the access event (Phase 4 of the
// cross-service tracing rollout).
func Middleware(tracer trace.Tracer, h http.Handler) http.Handler {
	if tracer == nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := tracer.Start(ctx, "gateway.request",
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.target", r.URL.Path),
			),
		)
		defer span.End()

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		h.ServeHTTP(sw, r.WithContext(ctx))
		dur := time.Since(start)

		span.SetAttributes(
			attribute.Int("http.status_code", sw.status),
			attribute.Float64("gateway.duration_ms", float64(dur.Microseconds())/1000.0),
		)
		if sw.matchedRoute != "" {
			span.SetAttributes(attribute.String("http.route", sw.matchedRoute))
		}
		if sw.status >= 500 {
			span.SetStatus(codeError(), http.StatusText(sw.status))
		}
	})
}

// statusWriter wraps http.ResponseWriter to capture the status code
// and the matched route. It implements the gateway's RouteRecorder
// interface (the proxy stamps the matched route on the writer so
// the access-log middleware can attribute the request to a configured
// prefix). Implementing it here lets the same writer carry both the
// status code (for span attributes) and the matched route (for the
// existing route-recorder protocol).
type statusWriter struct {
	http.ResponseWriter
	status       int
	matchedRoute string
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) SetMatchedRoute(route string) {
	sw.matchedRoute = route
}
