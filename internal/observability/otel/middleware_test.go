package otel_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	gatewayotel "github.com/helmedeiros/decision-gateway/internal/observability/otel"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// Test_Middleware_EmitsSpan_WithCorrectAttributes drives the
// middleware with a synthetic handler and asserts the exporter
// observes one span with the expected name + attributes for the
// happy 200 path.
func Test_Middleware_EmitsSpan_WithCorrectAttributes(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	tracer := tp.Tracer("test")

	h := gatewayotel.Middleware(tracer, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/decide", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected exactly 1 span, got %d", len(spans))
	}
	span := spans[0]
	if span.Name != "gateway.request" {
		t.Errorf("span name = %q, want gateway.request", span.Name)
	}
	if span.SpanKind != oteltrace.SpanKindServer {
		t.Errorf("span kind = %v, want Server", span.SpanKind)
	}
	want := map[attribute.Key]attribute.Value{
		"http.method":      attribute.StringValue("POST"),
		"http.target":      attribute.StringValue("/decide"),
		"http.status_code": attribute.IntValue(200),
	}
	got := map[attribute.Key]attribute.Value{}
	for _, a := range span.Attributes {
		got[a.Key] = a.Value
	}
	for k, v := range want {
		gv, ok := got[k]
		if !ok || gv != v {
			t.Errorf("attr %s = %v (present=%v), want %v", k, gv, ok, v)
		}
	}
	if _, ok := got["gateway.duration_ms"]; !ok {
		t.Errorf("expected gateway.duration_ms attribute, got none")
	}
}

// Test_Middleware_ExtractsParentTraceContext drives the middleware
// with an incoming W3C traceparent header and asserts the emitted
// span is a child of the provided parent (same trace ID, parent
// span ID set).
func Test_Middleware_ExtractsParentTraceContext(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})

	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	tracer := tp.Tracer("test")

	const incoming = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	req := httptest.NewRequest(http.MethodPost, "/decide", nil)
	req.Header.Set("traceparent", incoming)

	h := gatewayotel.Middleware(tracer, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), req)

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	span := spans[0]
	if span.SpanContext.TraceID().String() != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("trace id = %s, want parent trace id from header", span.SpanContext.TraceID().String())
	}
	if span.Parent.SpanID().String() != "b7ad6b7169203331" {
		t.Errorf("parent span id = %s, want b7ad6b7169203331", span.Parent.SpanID().String())
	}
}

// Test_Middleware_500RecordsErrorStatus asserts the span's Status
// is set to Error when the handler writes a 5xx response code.
func Test_Middleware_500RecordsErrorStatus(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	tracer := tp.Tracer("test")

	h := gatewayotel.Middleware(tracer, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/anything", nil))

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Status.Code.String() != "Error" {
		t.Errorf("span status code = %s, want Error", spans[0].Status.Code.String())
	}
}

