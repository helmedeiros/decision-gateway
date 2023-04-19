# 7. Prometheus `/metrics` endpoint on the gateway

## Status

Accepted — `internal/observability/metrics` ships `New() (*Sink, http.Handler)` returning a private-registry-backed Sink + the matching `/metrics` handler. The Sink exposes `gateway_requests_total{method, route, status}` counter + `gateway_request_duration_seconds{method, route, status}` histogram with custom 14-bucket boundaries covering the measured per-request latency (median ~400 µs, p99 ~3 ms). `cmd` flag `--metrics-enabled` wires the Sink's middleware inside the OTel tracing frame and outside AccessLog so the labels read the same values AccessLog writes.

## Context

The platform already has two Prometheus exposers: markup-svc emits per-Decide counters/histograms (`markup_decide_*`), and the OTel Collector emits per-span call/duration histograms (`traces_spanmetrics_*`). The gateway's own work — routing, proxying, response copy — was visible only as a gateway-side span span_kind=SERVER series in the spanmetrics view (`traces_spanmetrics_*{service_name="decision-gateway",span_kind="SPAN_KIND_SERVER"}`).

That's enough for SPM Monitor and a handful of platform dashboards, but it has two limitations: (1) spanmetrics requires `--otel-enabled` + the Collector + sampling configured; turning observability off makes the metrics go dark too; (2) per-route slicing requires custom span dimensions or attribute extraction in the connector config. An always-on gateway-owned Prometheus exposition is the simpler operator artifact: scrape it directly, query it without going through Collector + spanmetrics translation.

## Decision

`internal/observability/metrics/metrics.go` defines a Sink + its `Middleware`. The middleware captures the response status via a wrapper that also implements `middleware.RouteRecorder`, so the proxy's existing writer-stamping path (set the matched route via `SetMatchedRoute`) populates the `route` label without any further wiring. Status code becomes a label as `strconv.Itoa(int)` — bounded by HTTP's status set so cardinality stays sane.

Bucket boundaries:

```
0.0001  0.00025  0.0005
0.001   0.0025   0.005   0.01
0.025   0.05     0.1     0.25  0.5  1  2.5
```

14 buckets, sub-millisecond resolution at the low end where the platform's median latency sits, 2.5 s ceiling to bound the timeout tail. Same shape as markup-svc's ADR-0024 buckets, scaled for the gateway's wider distribution.

`cmd/decision-gateway` flag `--metrics-enabled` (default off for backwards compatibility) constructs the Sink + handler, mounts the handler at `/metrics`, and inserts the middleware in the existing composition order:

```
CorrelationID(Middleware?(MetricsMiddleware?(AccessLog(mux))))
```

The order keeps Metrics inside the OTel span so its work counts in `gateway.request` and outside AccessLog so the same status/route/duration the access log writes is what the metrics observe — labels and access events stay in sync by construction.

Pricing-observability's Prometheus scrape config already targets `host.docker.internal:8090/metrics` via the gateway's existing `/metrics` route forwarder (per ADR-0019 in markup-svc). With the gateway's own `/metrics` endpoint live, the forwarder behavior needs to change: a future small ADR updates the gateway to serve `/metrics` directly (when `--metrics-enabled`) and the route table's `/metrics=>markup-svc` is removed. Until then, operators set `--upstream-h2c` style precedence: gateway's own endpoint wins the path because the mux registers `/metrics` exactly, ahead of the `/` catch-all that points at the proxy.

## Consequences

### Closed

- Operators can scrape per-route gateway request counts + latency histograms without `--otel-enabled` and without going through the OTel Collector. Always-on signal.
- Per-route slicing is a label, not a span-dimension. `sum(rate(gateway_requests_total[5m])) by (route)` answers "how is each route being used" with one PromQL.
- markup-svc's `/metrics` scrape still works: the gateway mux registers `/metrics` exactly before the `/` proxy fallback, so the gateway's own endpoint takes precedence.

### Not closed

- Connection pool stats. `http.Transport` does not expose easy pool counters; pulling `MaxIdleConns / IdleConns` would need either a Transport wrapper or Go runtime metrics. Lands when an operator's investigation actually wants the data.
- Response-size histogram. Adds value for payload-growth investigation; out of scope today.
- Removing the `/metrics=>markup-svc` route from the compose. Once both gateway and markup-svc serve their own `/metrics`, the scrape config in pricing-observability splits into two jobs and the gateway's route forwarder is no longer needed. Lands in a coordinated commit across pricing-observability + decision-gateway compose.
- Reusing the existing AccessLog's status recorder. Both middlewares wrap the response writer; combining them into one wrapper saves an allocation per request. ~50 ns. Below the noise floor.
