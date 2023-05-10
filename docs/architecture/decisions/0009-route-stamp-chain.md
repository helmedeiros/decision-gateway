# 9. Chain `SetMatchedRoute` through middleware wrappers

## Status

Accepted — `middleware.statusRecorder.SetMatchedRoute` (AccessLog) and `metrics.statusWriter.SetMatchedRoute` (gateway-side metrics) now forward the call to the underlying `http.ResponseWriter` if it also implements `RouteRecorder`. Fixes the `gateway_requests_total{route=""}` bug: when the metrics middleware wraps AccessLog (per the cmd composition `CorrelationID(Tracing?(Metrics(AccessLog(mux))))`), the proxy stamps the route on the topmost writer (AccessLog's wrapper). Without chaining, the metrics statusWriter underneath never sees the call. With chaining, both layers observe the same route value.

## Context

The platform's alerting story (pricing-observability/ADR-0014) wanted a `route="/admin"` filter on `gateway_requests_total` to fire only on rejected hot-reloads. Looking at live metrics, every entry had `route=""` regardless of which prefix actually matched. Trace + access-log paths were correct; only the metrics counter was wrong.

Root cause: the gateway middleware composition order. cmd wires:

```
CorrelationID( Tracing?( Metrics( AccessLog( mux ) ) ) )
```

Mux dispatches to the proxy. Proxy reads the writer it was handed, type-asserts to `RouteRecorder`, and calls `SetMatchedRoute(prefix)`. The writer it sees IS the AccessLog statusRecorder (since AccessLog is the immediate parent of mux). AccessLog's wrapper records the route — correctly populating `gateway.access` JSON events. But Metrics's statusWriter, one layer further out, is a separate wrapper with its own `route` field that nobody calls `SetMatchedRoute` on. By the time the response unwinds and Metrics reads `sw.route`, the value is still its zero string.

Two design options.

### 1. Chain the call through each RouteRecorder layer

`statusRecorder.SetMatchedRoute` calls its own setter AND forwards to the underlying writer if it also implements RouteRecorder. Same for `statusWriter`. The proxy stamps once on the topmost writer; the chain propagates the stamp inward to every wrapper that wants to observe it.

Pros: minimal change (two small methods); zero overhead per request when no chain is present (a type assertion + nil branch).
Cons: order-dependent (a non-RouteRecorder writer in between breaks the chain). The platform's current writers all implement it; future wrappers must too if they want to see the route.

### 2. Single shared writer

Combine AccessLog and Metrics into one wrapper that both observe through. A single struct with the route field that both middleware bodies read.

Pros: one source of truth.
Cons: combines two middleware lives; harder to unit-test; would break the existing AccessLog isolation that supports running without Metrics.

**Pick chaining.** Smallest possible change, preserves middleware isolation, two-line addition per wrapper.

## Decision

```go
// internal/middleware/accesslog.go
func (s *statusRecorder) SetMatchedRoute(prefix string) {
    s.route = prefix
    if rec, ok := s.ResponseWriter.(RouteRecorder); ok {
        rec.SetMatchedRoute(prefix)
    }
}
```

Same shape in `internal/observability/metrics/metrics.go`'s `statusWriter`. A regression test in `internal/middleware/route_chain_test.go` proves a stub outer recorder observes the route after the inner AccessLog wrapper handles a request that stamps via `SetMatchedRoute`.

## Consequences

### Closed

- `gateway_requests_total{route="..."}` now populates correctly. Per-route Grafana panels (decision-gateway-overview dashboard in pricing-observability/ADR-0012) show real per-prefix slices.
- pricing-observability ADR-0014's `AdminHotReloadRejected` alert can tighten from `method="POST",status="400"` to the more precise `route="/admin",status=~"4.."`. Lands as a separate commit in that repo.
- Future middleware that wants to see the matched route just implements `RouteRecorder` on its own writer wrapper. The chain finds it.

### Not closed

- Order independence. If an operator inserts a wrapper that doesn't implement RouteRecorder between Proxy and AccessLog, the chain breaks at that wrapper. The cmd's composition order is fixed; this is not a real risk today.
- A more general writer-decorator pattern (multiple writers contributing observations to a shared context). Out of scope; the current chain handles the platform's actual cases.
- Per-method label cardinality. The route label is now correct but still combines with method + status; the combined space is bounded by the configured route table size, so this stays well below any production Prometheus ceiling.
