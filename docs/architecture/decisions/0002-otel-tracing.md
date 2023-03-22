# 2. Gateway-side OTel tracing + W3C trace context propagation

## Status

Accepted — `--otel-enabled` on the gateway bootstraps an OTLP gRPC TracerProvider, sets the global TextMapPropagator to W3C TraceContext + Baggage, wraps the inbound handler with a `gateway.request` span, and wraps each per-route `httputil.ReverseProxy`'s outbound `RoundTripper` with an `InstrumentedTransport` that opens a `gateway.proxy.upstream` child span + injects the `traceparent` header on the upstream request. The gateway becomes a trace-context hop: any caller that emits W3C traceparent (traffic-gen in v0.0.3+, or operators' curl tests) appears as the trace root; markup-svc's `markup.decider.decide` span joins the same trace as a grandchild of the gateway span. The OTel-disabled path stays the zero-overhead default; no new mandatory dependency on the SDK at runtime.

## Context

`pricing-observability/ADR-0002` (the traces phase) and `markup-svc/ADR-0016` (the SDK bootstrap) together produced a working trace for one service. Jaeger UI shows one root span per `/decide` call named `markup.decider.decide`, with the engine's per-rule attributes. That's already a step up from "no traces at all," but it answers only the **inside-markup-svc** question — for the user's stated goal of finding bottlenecks **between markup-svc and the gateway**, the trace is missing the proxy-overhead side of the latency split. The operator looking at Jaeger sees the engine span took 67µs and learns nothing about whether the gateway added 50µs or 5ms on top.

Three downstream needs gate on this ADR:

1. **Per-request gateway / engine latency split.** The user explicitly named "performance of markup-svc and api gateway so we can improve bottlenecks." Without a gateway span as the parent + an upstream-call span as the child, the gateway-side cost (routing, request rewriting, header propagation, connection-pool wait) is invisible. The cost might be sub-millisecond at idle, but under the traffic-gen ramp profile (10→500 QPS), the connection-pool wait can dominate.
2. **Traffic-gen → gateway → markup-svc as one trace.** v0.0.3 of traffic-gen will emit a root span per outbound POST and inject traceparent. The gateway must accept that header and become the middle hop of the chain; otherwise the traffic-gen trace ends at "the request was sent" and the markup-svc trace starts fresh, and the join only exists in the operator's head.
3. **Log/trace correlation in v0.0.4 of pricing-observability** (Phase 4 of the cross-service tracing rollout). The gateway's access-log middleware will write `trace_id` + `span_id` into the JSON line so a Kibana operator filtering by `attrs.correlation_id` can click directly to the matching Jaeger trace. That work needs the trace_id to exist in the request context, which needs this ADR's span middleware.

Three design questions.

### 1. Custom middleware + RoundTripper vs `otelhttp` contrib

The OTel project ships `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` with `otelhttp.NewHandler` and `otelhttp.NewTransport`. They are the standard.

- Pro of contrib: one fewer thing to maintain; spec-compliant attribute names (`http.method`, `http.status_code` etc. straight from the semantic conventions); well-tested across Go stdlib HTTP server variants.
- Con of contrib: a third dependency tree to vet (`go.opentelemetry.io/contrib/...` is a separate Go module with its own version cadence — `v0.36.x` for OTel `v1.11.x`). The library is opinionated about span name (`HandlerFunc` by default; needs a `WithSpanNameFormatter` override to get `gateway.request`). Wraps the outbound `RoundTripper` with extra middleware that includes a metrics shim the gateway does not need at v0.0.2.

Custom middleware + RoundTripper:

- Pro: ~50 lines each; the span name + attribute set match the gateway's domain language (`gateway.duration_ms`, `gateway.proxy.upstream`, `upstream.host`); no extra module in `go.mod`. Same posture as the rest of the project (`middleware.CorrelationID`, `middleware.AccessLog`).
- Con: re-derives what contrib already does — attribute-name maintenance shifts to this repo when the semantic conventions evolve (which they do, `http.method` → `http.request.method` in 1.20+ semconv).

**Pick custom.** The 50 LOC stays inside `internal/observability/otel/` (already a package in the markup-svc + traffic-gen tree by then) and matches the project's small-focused-middleware shape. When semconv 1.20 lands the change is one-file. The contrib library lands when the gateway gains enough instrumentation that the maintenance cost flips.

### 2. Span shape: one root span vs root + upstream child

A single `gateway.request` span that wraps the entire inbound handler covers the operator's "request took N ms" question. But the user's actual question is "where in those N ms is the cost." That split needs the gateway-side window (routing + proxy setup + connection pool acquisition + response copy) AND the upstream-side window (actual markup-svc work) to be distinguishable.

Two spans:

- `gateway.request` (root): full inbound window, server kind, `http.method` + `http.route` + `http.status_code` + `gateway.duration_ms` attributes.
- `gateway.proxy.upstream` (child of root): only the `RoundTrip` window, client kind, `http.method` + `http.url` + `upstream.host` + `upstream.status_code` attributes.

The delta between `gateway.request.duration_ms` and `gateway.proxy.upstream.duration` IS the gateway-side overhead the operator wants to see. In Jaeger's trace view, this is two stacked bars; the eye reads the gap directly.

**Pick two spans.** The cost is one extra span open + close per request (~50ns); the operator-experience win is the entire point of this ADR.

### 3. Where the upstream child span opens: middleware level vs RoundTripper level

The proxy's `ServeHTTP` is the call site that hands the request to `httputil.ReverseProxy.ServeHTTP`. A wrapping span around that line would cover the right window. But `ReverseProxy.ServeHTTP` includes header rewriting + body buffering + the actual `Transport.RoundTrip` + response copying. The window the operator wants for "how slow is markup-svc" is just the `RoundTrip` — the rest is gateway-side cost belonging to the `gateway.request` span's overhead.

Wrapping at the `RoundTripper` level (`InstrumentedTransport.RoundTrip`):

- Pro: the child span is exactly the time from "request goes out the wire" to "response headers come back." Pure upstream cost.
- Pro: the propagator's `Inject` call lives inside the same RoundTripper, so traceparent is added at the point where the headers are final + about to ship.
- Con: needs a transport-injection seam in `proxy.New`, which the public function did not have (added in this commit; signature gains a `transportWrapper func(http.RoundTripper) http.RoundTripper` parameter, `nil` keeps the previous behavior).

**Pick RoundTripper.** The window is the right one and the seam was a one-line signature change.

## Decision

`internal/observability/otel` is the new package. Three files:

- `bootstrap.go`: `Bootstrap(ctx, instrumentationName) (trace.Tracer, Shutdown, error)`. Constructs OTLP gRPC exporter + batched TracerProvider + detected resource + W3C TraceContext+Baggage propagator. Same shape as `markup-svc/internal/observability/otel.Bootstrap` (ADR-0016 in that repo), with the propagator addition.
- `middleware.go`: `Middleware(tracer, http.Handler) http.Handler`. Extracts incoming trace context from request headers, opens a `gateway.request` span (server kind), wraps the response writer in a `statusWriter` that captures the status code + implements the existing `RouteRecorder` interface, sets `http.method` / `http.target` / `http.status_code` / `http.route` / `gateway.duration_ms` on span close. Sets span status to Error on 5xx, leaves OK on 4xx (matches the access-log middleware's severity policy).
- `transport.go`: `InstrumentedTransport struct { Tracer, Inner }` implementing `http.RoundTripper`. Opens `gateway.proxy.upstream` (client kind) span, injects W3C traceparent via the global propagator, calls `Inner.RoundTrip`, sets `upstream.status_code` + Error status on 5xx upstream.

`internal/proxy/proxy.New` gains a third parameter `transportWrapper func(http.RoundTripper) http.RoundTripper`. When `nil`, each per-route reverse proxy uses the unwrapped transport (zero overhead, preserves the pre-ADR behavior). When set, each reverse proxy's transport is the wrapper output. `cmd/decision-gateway/main.go` passes `nil` when `--otel-enabled` is not set, or a wrapper that constructs `InstrumentedTransport{Tracer, Inner: rt}` when it is.

`cmd/decision-gateway/main.go` gains the `--otel-enabled` bool flag. Composition order changes from `CorrelationID(AccessLog(mux))` to `CorrelationID(Middleware(AccessLog(mux)))` when OTel is enabled, with the `Middleware` frame omitted entirely when it is not. The order keeps the existing semantic invariant (correlation ID is in context for the access log) and adds one: the span surrounds AccessLog so the `gateway.duration_ms` span attribute matches `attrs.duration_ms` on the access event by construction.

Docker compose (`docker-compose.yaml`) wires the gateway service with the same OTel env vars markup-svc uses: `OTEL_SERVICE_NAME=decision-gateway`, `OTEL_EXPORTER_OTLP_ENDPOINT=http://host.docker.internal:4317`, `OTEL_EXPORTER_OTLP_PROTOCOL=grpc`, `OTEL_TRACES_SAMPLER=parentbased_always_on`, `extra_hosts: host.docker.internal:host-gateway`. The command line adds `--otel-enabled`. This is a follow-up commit in the same release window.

## Consequences

### Closed by this ADR

- The Jaeger UI service list grows from `[markup-svc]` to `[markup-svc, decision-gateway]`. Every `/decide` request renders as one trace with two spans (gateway → markup-svc) or three when the gateway middleware emits the inbound + upstream split (which it does).
- Per-request gateway / engine cost split is directly visible: `gateway.request.duration_ms` minus `gateway.proxy.upstream.duration` = gateway-side overhead. Operators can answer "is the gateway adding 100µs or 5ms" by reading two adjacent bars.
- Trace context propagates one hop end-to-end. The gateway is now ready to be the middle hop when traffic-gen v0.0.3 lands the outbound root span.
- The `cookbook/traces-flowing.md` recipe in pricing-observability ADR-0002 works end-to-end through the gateway too: `curl /decide` produces a 3-span trace whose root is the gateway span instead of disappearing into the gateway as a black box.

### NOT closed by this ADR

- traffic-gen does not emit spans yet. The gateway's `Middleware` extracts traceparent when present and falls back to a root span when absent; once traffic-gen ships its root span the gateway becomes a true middle hop without code changes here.
- Log/trace correlation. The access-log middleware does not yet write `trace_id` + `span_id` into the JSON `attrs`. That work lands in a follow-up ADR in this repo OR in pricing-observability — the cleaner place is here (the middleware that writes the log line is what needs the trace context) but the bigger pricing-observability work is the index pattern + Kibana hop link. Decided in the next planning slice.
- Per-route prefix as a high-cardinality attribute. The current `http.route` attribute is the matched route prefix (`/decide`, `/admin`). For routes mounted as wildcards (none today; a future change), the prefix would still be low-cardinality. If a future ADR introduces path templates (e.g., `/decide/:experiment_id`), the attribute moves to the templated form to stay query-safe in Jaeger's tag search.
- Sampling configuration. The compose sets `OTEL_TRACES_SAMPLER=parentbased_always_on` which honors incoming sampling decisions and samples everything at the root. Production deployments override via the env var; the in-binary code stays unopinionated.

### Performance impact

- `--otel-enabled` not set: the `Middleware` frame is skipped (the cmd wires it conditionally); the proxy's transport is the unwrapped `http.Transport`. Zero ns delta vs the pre-ADR binary.
- `--otel-enabled` set, OTLP collector reachable: per-request adds two span open + close pairs (~100 ns total) + one propagator extract on the request headers (~50 ns) + one inject on the outbound headers (~50 ns). Aggregate ~200 ns per request. The batched span processor's submit is async; the hot path does not block on the gRPC export.
- `--otel-enabled` set, OTLP collector unreachable: the SDK's batched processor's queue grows to `MaxQueueSize` (default 2048 spans), then drops with a warning. The gateway's `/decide` latency stays unaffected; the operator sees lost spans in Jaeger.

The connection-pool sizing on the wrapped `Transport` is unchanged (the wrapper delegates to the inner transport for the actual RoundTrip + connection acquisition). Operators tuning the gateway for high QPS still tune `http.Transport.MaxIdleConnsPerHost` etc.; the wrapper is transparent to that.

### Validation strategy

- Unit tests in `internal/observability/otel/`:
  - `middleware_test.go`: emits one span with correct attributes for the happy 200 path; extracts incoming W3C traceparent so the emitted span is a child of the provided parent (real trace ID + parent span ID assertions); marks 5xx responses as Error span status.
  - `transport_test.go`: outbound request gets a `traceparent` header derived from the active span's trace ID; emits a `gateway.proxy.upstream` child span; captures `upstream.status_code` from the response.
- Integration smoke against the live platform stack: send `curl -H "X-Correlation-ID: trace-N" /decide`; Jaeger UI search for service `decision-gateway` shows the trace with three spans (`gateway.request` → `gateway.proxy.upstream` → `markup.decider.decide`); the trace IDs match across all three spans.
- The smoke is documented in the v0.0.2 release notes as a verification step operators replicate.
