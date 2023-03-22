package otel_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gatewayotel "github.com/helmedeiros/decision-gateway/internal/observability/otel"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// Test_InstrumentedTransport_InjectsTraceparentAndEmitsChildSpan
// asserts the round-tripper:
//
//  1. Sets traceparent on the outbound request matching the active
//     span's trace ID + span ID.
//  2. Emits a span named gateway.proxy.upstream rooted at the
//     active parent span.
//  3. Sets upstream.status_code from the response.
func Test_InstrumentedTransport_InjectsTraceparentAndEmitsChildSpan(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})

	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	tracer := tp.Tracer("test")

	captured := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	rt := &gatewayotel.InstrumentedTransport{Tracer: tracer, Inner: http.DefaultTransport}

	ctx, parent := tracer.Start(context.Background(), "parent")
	parentTraceID := parent.SpanContext().TraceID().String()
	parentSpanID := parent.SpanContext().SpanID().String()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	parent.End()

	if !strings.HasPrefix(captured, "00-"+parentTraceID+"-") {
		t.Errorf("traceparent on upstream req = %q, expected to include parent trace id %s", captured, parentTraceID)
	}
	_ = parentSpanID // parent span id is not the injected one (the upstream span is); we only assert trace id

	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("expected 2 spans (parent + upstream), got %d", len(spans))
	}
	var upstreamSpan tracetest.SpanStub
	for _, s := range spans {
		if s.Name == "gateway.proxy.upstream" {
			upstreamSpan = s
			break
		}
	}
	if upstreamSpan.Name == "" {
		t.Fatalf("did not find gateway.proxy.upstream span; got %+v", spans)
	}
	if upstreamSpan.SpanKind != oteltrace.SpanKindClient {
		t.Errorf("upstream span kind = %v, want Client", upstreamSpan.SpanKind)
	}
	if upstreamSpan.Parent.SpanID().String() != parent.SpanContext().SpanID().String() {
		t.Errorf("upstream span parent = %s, want %s", upstreamSpan.Parent.SpanID().String(), parent.SpanContext().SpanID().String())
	}
	got := map[attribute.Key]attribute.Value{}
	for _, a := range upstreamSpan.Attributes {
		got[a.Key] = a.Value
	}
	if got["upstream.status_code"] != attribute.IntValue(200) {
		t.Errorf("upstream.status_code = %v, want 200", got["upstream.status_code"])
	}
}
