// Package otel bootstraps the OTel SDK and provides the gateway's
// HTTP middleware + outbound RoundTripper for emitting + propagating
// trace context across the platform. See ADR-0002.
package otel

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Shutdown is the cleanup function returned by Bootstrap.
type Shutdown func(ctx context.Context) error

// Bootstrap initialises an OTel TracerProvider with an OTLP gRPC
// exporter and sets it as the global provider. It also sets the
// global TextMapPropagator to TraceContext + Baggage so the
// middleware extracts incoming traceparent headers and the
// outbound RoundTripper injects them on proxied requests. This is
// what makes the gateway a trace-context hop: parent span from
// traffic-gen (or any caller) on the inbound side, child span on
// markup-svc on the outbound side, all stitched into one trace in
// Jaeger.
//
// Reads the standard OTel SDK env vars:
//
//   - OTEL_EXPORTER_OTLP_ENDPOINT (default localhost:4317)
//   - OTEL_SERVICE_NAME (sets resource service.name)
//   - OTEL_RESOURCE_ATTRIBUTES (additional resource attributes)
//
// instrumentationName is the name used for the returned tracer
// (typically the binary's import path).
func Bootstrap(ctx context.Context, instrumentationName string) (trace.Tracer, Shutdown, error) {
	exp, err := otlptrace.New(ctx, otlptracegrpc.NewClient())
	if err != nil {
		return nil, nil, fmt.Errorf("otlptrace gRPC exporter: %w", err)
	}
	res, err := resource.New(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("resource detection: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	// Propagator set globally so:
	//   1. The Middleware extracts incoming W3C trace context from
	//      request headers (so the gateway.request span becomes a
	//      child of whatever sent the request, e.g. traffic-gen).
	//   2. The InstrumentedTransport injects the current span's
	//      context into outbound headers (so the upstream service,
	//      e.g. markup-svc, sees the gateway as its parent).
	// Baggage is included so user-added baggage keys propagate; the
	// gateway does not produce baggage today but operators wiring
	// custom middleware can.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp.Tracer(instrumentationName), tp.Shutdown, nil
}
