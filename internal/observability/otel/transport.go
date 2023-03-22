package otel

import (
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// InstrumentedTransport wraps an http.RoundTripper to:
//
//  1. Open a child span named "gateway.proxy.upstream" before the
//     RoundTrip call, parented at the current span in ctx (the
//     "gateway.request" span set up by Middleware).
//  2. Inject W3C trace context headers into the outbound request via
//     the global TextMapPropagator so the upstream service (markup-svc)
//     can join the same trace.
//  3. Record the upstream response status code + close the span.
//
// The span carries the per-hop attributes operators need to find
// where the latency is: upstream.host (so the same trace surfaces
// across multiple backends if a route is reconfigured), http.method,
// http.url, upstream.status_code, gateway.upstream.duration_ms.
type InstrumentedTransport struct {
	Tracer trace.Tracer
	Inner  http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t *InstrumentedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	inner := t.Inner
	if inner == nil {
		inner = http.DefaultTransport
	}
	if t.Tracer == nil {
		return inner.RoundTrip(req)
	}

	ctx, span := t.Tracer.Start(req.Context(), "gateway.proxy.upstream",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("http.method", req.Method),
			attribute.String("http.url", req.URL.String()),
			attribute.String("upstream.host", req.URL.Host),
		),
	)
	defer span.End()

	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

	resp, err := inner.RoundTrip(req.WithContext(ctx))
	if err != nil {
		span.SetStatus(codeError(), err.Error())
		return resp, err
	}
	span.SetAttributes(attribute.Int("upstream.status_code", resp.StatusCode))
	if resp.StatusCode >= 500 {
		span.SetStatus(codeError(), resp.Status)
	}
	return resp, nil
}
