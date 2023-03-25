# 3. Access log carries trace_id + span_id for log/trace correlation

## Status

Accepted — the gateway access-log middleware reads the active OTel SpanContext from the request context and writes the trace + span IDs onto the per-request JSON event as `attrs.trace_id` + `attrs.span_id`. When `--otel-enabled` is off or the request flows through a path the OTel middleware does not wrap (today: none, the OTel middleware mounts on the same mux), `SpanContext.IsValid()` returns false and the fields stay omitted via `omitempty` so the v0.0.1 + v0.0.2 schema stays a strict subset of the v0.0.3 schema (consumers parsing the smaller shape do not break).

## Context

ADR-0002 (gateway-side OTel tracing) shipped the `gateway.request` span and propagated W3C trace context to upstream services. ADR-0002 in pricing-observability shipped the traces phase. Together they made traces visible in Jaeger; ADR-0017 in markup-svc + ADR-0004 in traffic-gen completed the chain so a trace renders across all four services (traffic-gen → decision-gateway → markup-svc + the observability collector).

What is still missing: the operator's workflow when an alert fires on the access log. Today: an alert on `attrs.status:>=500` shows a list of log lines with `correlation_id` values. The operator copies the correlation ID, switches to Jaeger UI, searches by `rule.markup.correlation_id` tag, finds the matching trace, opens the waterfall. Three context-switches per investigation. Multiply by the volume during an incident and the cost is real.

The fix is to write the OTel trace + span IDs onto the access event so the Kibana row carries a direct link to the trace. Once trace_id is in the JSON, an operator clicks `attrs.trace_id` in Kibana's Discover → "view in Jaeger" → trace opens in a new tab. Zero context-switches, two clicks.

One design question.

### Where to read the SpanContext: middleware-time vs handler-time

The access-log middleware runs in the request middleware chain (`CorrelationID(Middleware(AccessLog(mux)))` per ADR-0002). The trace span starts in `Middleware` (the OTel middleware from ADR-0002); by the time `AccessLog` runs, the span is the active one in the request context. Two reasonable read sites:

- **At the start of AccessLog's frame**: read the SpanContext once, store on the writer wrapper, write it into the entry at the end. Pros: matches the existing pattern for `correlation_id` (read once, stored, used later). Cons: redundant — the context is unchanged from middleware-call to end-of-frame; the read site is arbitrary.
- **At the end of AccessLog's frame, just before writing the entry**: read the SpanContext directly off `r.Context()`. Pros: one read site; no extra writer-wrapper state; lazy (the read happens only if we actually emit the entry, not on early returns). Cons: requires `r.Context()` to still hold the OTel-augmented context — which it does, because middleware composition copies the context onto a new request and `next.ServeHTTP(w, r)` propagates it.

**Pick end-of-frame.** Simpler, lazier, matches the lazy posture of the other entry-time reads (`now()` is also called late). One line addition in the entry-build block.

## Decision

`internal/middleware/accesslog.go` imports `go.opentelemetry.io/otel/trace`. After building the basic `accessAttrs` struct, the middleware reads `oteltrace.SpanContextFromContext(r.Context())` and, if `IsValid()` returns true, writes `sc.TraceID().String()` into `attrs.TraceID` and `sc.SpanID().String()` into `attrs.SpanID`. The `accessAttrs` struct gains two new JSON-tagged fields:

```go
TraceID string `json:"trace_id,omitempty"`
SpanID  string `json:"span_id,omitempty"`
```

`omitempty` keeps the legacy shape strict-subset compatible: consumers parsing v0.0.1 or v0.0.2 access events with a schema validator do not see new required fields.

The change is in the middleware package because that is where the access-log emit lives; the OTel package stays unchanged (it produces the span; reading the SpanContext is a stdlib OTel API, not an internal-package one).

## Consequences

### Closed by this ADR

- Kibana → Jaeger one-click hop. When pricing-observability ships the Kibana URL template for `attrs.trace_id` (a small follow-up there, blocked on this ADR landing) the operator's investigation workflow becomes Discover → Trace in 2 clicks.
- Log/trace correlation is symmetric: Jaeger already carries `rule.markup.correlation_id` on the markup-svc spans, so the operator going from Jaeger → Kibana also has a tag to filter on.
- The access-event schema gains two strictly-additive fields. The v0.0.2 → v0.0.3 jump does not break any consumer parsing the older shape.

### NOT closed by this ADR

- markup-svc structured logs do not exist today — the binary writes `markup-server: listening on :8080 ...` as plain text and nothing per request. A future markup-svc ADR converts the stdout to per-request JSON + adds the same trace_id/span_id fields. Not blocking on this ADR; markup-svc's per-request signal is the spans themselves.
- traffic-gen's boot + summary JSON events are per-Run, not per-request. No spans exist at those points (the spans are inside the InstrumentedTransport's RoundTrip). Adding trace_id at the per-Run level would be misleading; the per-request signal is the spans themselves.
- Kibana URL template + Jaeger search-by-trace-id link. The URL template lives in pricing-observability's Kibana dashboard config (a separate compose volume or saved-object import). Blocked on this ADR + a small Kibana saved-object commit there.

### Performance impact

The added work per request is one `SpanContextFromContext` call (a context-value lookup, ~10 ns), one `IsValid` check (atomic load, ~1 ns), and two `String()` calls (hex encoding of 16/8 bytes, ~50 ns each) when the span context is valid. Aggregate ~110 ns per traced request, 11 ns per un-traced request (only the lookup + IsValid since the body is gated). Below the access-log encode + write cost (which is the dominant factor at ~1 µs per entry).

### Validation strategy

- Unit test (added in the same commit): construct a request with a known SpanContext via `oteltrace.ContextWithSpanContext`, run it through AccessLog with a recorder, assert the JSON entry contains the expected trace_id + span_id values. Counterpart test asserts the fields are absent when no valid SpanContext is in the context.
- Manual smoke against the live stack: send a /decide through the gateway with `--otel-enabled` set, observe the gateway.access JSON line carries `trace_id` matching the trace visible in Jaeger UI.
