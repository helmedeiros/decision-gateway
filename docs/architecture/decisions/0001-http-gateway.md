# 1. HTTP Gateway for the Pricing Decision Platform

## Status

Proposed — proposes a custom Go HTTP gateway sitting in front of markup-svc (and any future decision-shaped backend) with a flag-driven route table, correlation-ID propagation, a structured JSON access log per request, and `/healthz` / `/readyz` probes. The v0.0.1 menu is deliberately small; deeper concerns (mTLS, retries, circuit breaking, weighted routing, body transformation, gateway-side rate cap) stay deferred until a real consumer asks. The first adapter (Route + Router selection logic) lands in the same release window as a separate commit; the HTTP reverse-proxy wiring and cmd flags follow in W20.

## Context

The platform today has two services running side-by-side: markup-svc serves `/decide` (and `/admin/*`); traffic-gen drives load directly at markup-svc's port. That topology has three sharp edges as the platform grows.

- **Single front door.** A second decision-shaped backend (a different rule engine, a recommendation service, a markup-v2 deployment running A/B against v1) needs operators to know the port-mapping per backend. Clients hardcoded against `markup-svc:8080` don't survive a topology change. The platform wants one stable URL for any client.
- **Cross-cutting concerns.** Correlation ID, access logging, request body limits, future auth — each is a thing every backend needs but nothing about the markup decision should care about. Putting them in each backend duplicates code and creates drift (the markup-svc ADR-0003 HTTP layer would have to grow a "is this request authenticated" branch the engine doesn't care about).
- **Routing decisions at the platform tier.** Weighted A/B between two backend deployments, traffic shadowing to a canary, "send 5% of `/decide` to the new backend" — these belong above the backend, not inside it. markup-svc already has the `--route` flag from ADR-0011 for intra-service A/B between rule sets; gateway-level routing is the orthogonal question of which *deployment* gets the traffic.

The gateway is the platform box that solves these. Three design questions.

### 1. Custom Go gateway vs adopting Envoy / Traefik / nginx

Off-the-shelf:

- **Pro**: huge feature surface (mTLS, JWT validation, WASM filters, retries, circuit breakers, weighted routing, request mirroring, hot config reload), battle-tested in production at every scale, OSS community, no Go code to maintain in this repo.
- **Con**: configuration is YAML / TOML / Lua / WASM and the operator learning curve is steep; a "small tweak" loop requires understanding the underlying gateway's mental model; the wire shape between gateway and backend is the gateway's design, not ours; "is this gateway behaving correctly?" investigations require gateway-specific tools (envoy config dump, traefik dashboard, etc.).

Custom Go:

- **Pro**: matches the rest of the platform's Go + ADR + agent-gate posture exactly. Operators reading this repo see Go code shaped the same way as markup-svc and traffic-gen. The feature surface is small but every feature is auditable line by line. The wire shape is `httputil.ReverseProxy` which operators already understand from the Go stdlib.
- **Con**: the feature surface is bounded by what we ship; mTLS, WAF, sophisticated retries, weighted routing all have to be built before they're available. For a busy production with regulatory requirements, off-the-shelf wins on weeks-saved.

**Pick custom for v0.0.x.** The platform is in the "learn the shape" phase; reading and changing Go code is the educational point. Once a real consumer asks for a feature the custom gateway does not ship (mTLS for a regulated deployment, JWT verification for a multi-tenant rollout), the ADR can either (a) extend the custom gateway with that feature, or (b) deploy an off-the-shelf gateway in front of it. Both paths stay open; neither is foreclosed by shipping a small custom gateway today.

### 2. What does the gateway ship in v0.0.1?

A minimum viable front door:

- **HTTP reverse proxy** via `net/http/httputil.ReverseProxy`. One proxy instance per configured `Route`; the gateway picks the proxy by matching the request path against the route prefixes.
- **Configurable routes**: a list of `(path prefix → backend URL)` pairs. v0.0.1 ships the `--route=/decide=>http://markup-svc:8080` flag (repeatable); a YAML config file is deferred.
- **Longest-prefix match**: when two routes overlap (`/decide` and `/decide/v2`) the longer prefix wins. The matching logic is the same shape `http.ServeMux` uses for its longest-match handler dispatch, just generalized to non-tree-structured prefixes.
- **Correlation-ID middleware**: read the inbound `X-Correlation-ID` header or mint a UUID; thread the value through to the backend on the outbound request and stamp it on the response. Markup-svc and traffic-gen already use this header (see markup-svc `internal/httpapi.WithCorrelationID`); the gateway preserves the value end-to-end so tracing dashboards correlate across all three services.
- **Structured JSON access log per request**: one line per response with `target`, `status`, `duration_ms`, `route` (the matched prefix), `correlation_id`. The shape matches `traffic-gen/internal/jsonlog` so an aggregator parses gateway + traffic-gen logs with the same schema.
- **`/healthz` and `/readyz`**: gateway's own probes, distinct from any backend. The gateway is healthy if its process is running; ready once it has bound to the listen address. Backend health is the backend's concern; the gateway's readiness is independent.

The v0.0.1 surface explicitly does NOT include:

- mTLS, JWT verification, WAF rules, IP allowlists. Auth posture is identical to markup-svc's `/admin/reload`: gate at the network layer (NetworkPolicy, sidecar, listen address binding).
- Retries, circuit breakers, hedged requests. A backend returning 5xx returns 5xx to the client; markup-svc's no-match returns 404 and the gateway forwards that.
- Weighted routing, request shadowing, mirroring. Markup-svc has intra-service A/B via the `--route` flag (ADR-0011); gateway-level weighted routing waits for a real two-deployment scenario.
- Hot reload of the route table. v0.0.1 reads routes at boot and a topology change requires a restart. A future `POST /admin/routes` ADR (mirroring markup-svc ADR-0015's `POST /admin/guardrails`) lands when a real operator asks.
- Per-route timeout / per-route header rewrites. v0.0.1 has one process-wide `--backend-timeout`; per-route knobs land when a backend with materially different latency motivates them.

### 3. Configuration shape

Flag-driven, same posture as traffic-gen and markup-svc:

```sh
./decision-gateway \
  --listen=:8090 \
  --route=/decide=>http://markup-svc:8080 \
  --route=/admin/=>http://markup-svc:8080 \
  --backend-timeout=5s
```

The `--route=PREFIX=>BACKEND` flag is repeatable; each occurrence adds one route. The arrow separator (`=>`) is the same shape markup-svc's `--route=model:variant:type:path` uses (a small DSL inside one flag), and is parsed at boot with errors quoting the offending value.

A YAML config file is deferred. The flag form covers the v0.0.1 menu cleanly; a config file would introduce a parser + schema + version surface that the small menu does not warrant. Operators outside the menu run a wrapper main per the markup-svc / traffic-gen pattern.

## Decision

`internal/gateway` ships:

```go
// Route declares a path prefix and the backend URL the gateway
// forwards matching requests to. The Backend is parsed at config
// time so invalid URLs fail boot; the parsed *url.URL is what the
// reverse-proxy adapter (a later commit) hands to httputil.NewSingleHostReverseProxy.
type Route struct {
    Prefix  string
    Backend *url.URL
}

// Router selects a Route by longest-prefix match on the request
// path. The matching is O(N) over the configured routes; for the
// expected v0.0.1 menu of a handful of routes this is well under
// any meaningful latency budget. A precomputed trie can land in a
// follow-up if the route list grows to materially larger than ~20.
type Router struct { ... }

func NewRouter(routes []Route) (*Router, error)
func (r *Router) Match(path string) (Route, bool)
```

The reverse-proxy adapter, correlation-ID middleware, structured-log middleware, `/healthz` + `/readyz` handlers, and `cmd/decision-gateway` wiring land in W20 commits of this release window. Each gets its own ADR section if a non-trivial design question surfaces during implementation; the v0.0.1 stack is deliberately small enough that one ADR covers the shape.

## Consequences

### Closed by this ADR

- The platform has a single stable URL operators can put any client behind; backend topology changes do not propagate to clients.
- Cross-cutting concerns (correlation ID, access logging, future auth) live in one place instead of being duplicated per backend.
- The reverse-proxy abstraction is `httputil.ReverseProxy` — a stdlib type operators already understand. No new mental model to learn.
- The custom vs off-the-shelf decision is made for v0.0.x and explicitly leaves both extension paths open: extend the custom gateway with new features or deploy off-the-shelf in front of it.

### NOT closed by this ADR

- mTLS, JWT verification, WAF, IP allowlists. Out of scope for v0.0.1.
- Retries, circuit breakers, hedged requests. Backend 5xx propagates to the client unchanged.
- Weighted routing, traffic shadowing, request mirroring. Wait for a real two-deployment scenario.
- Hot reload of the route table. A topology change requires a restart at v0.0.1.
- Per-route timeout / header rewrite knobs. One process-wide timeout for now.
- YAML config file. Flag form covers the menu; a file lands when operators need shapes the flag cannot express compactly.

### Performance impact

The gateway adds two layers between the client and the backend:

- One `Router.Match` call per request: O(N) over the route list. For N=5 routes the cost is sub-microsecond. A precomputed prefix trie would be O(log N) or O(L) where L is path length, but for the expected route counts the trie's setup cost dominates the lookup savings.
- One `httputil.ReverseProxy.ServeHTTP` call: this allocates a new outbound request, copies headers, opens (or reuses, via the default Transport's connection pool) a TCP connection to the backend, writes the body, reads the response, copies headers back, writes the body. The cost per request is dominated by the TCP round-trip and the body copy; the gateway adds ~50-100 µs of CPU work on a localhost backend, invisible against any over-network backend latency.

The correlation-ID and access-log middlewares add a header read, a header write, and one structured-log encode per request. Cost is in the same microsecond range as the reverse-proxy work; below the noise floor of the HTTP-handler cost itself.

A scientific harness for the gateway is out of scope for v0.0.1. If the gateway becomes a measurable contributor to platform latency dashboards, a future ADR commits to per-route bars against a Linux/amd64 reference posture, the same shape markup-svc's `scientific/v0.X.Y/` directories use.

### Validation strategy

- `internal/gateway`: unit tests for the `Router.Match` selection logic. Cover:
  - Empty route list → `NewRouter` returns an error (rejecting at construction surfaces operator misconfiguration loudly rather than producing a silent always-404 gateway).
  - Single route → `Match` returns the route for any path matching the prefix.
  - Two non-overlapping routes → `Match` returns the right route per path.
  - Two overlapping routes (`/decide` and `/decide/v2`) → longest-prefix wins.
  - Path with no matching route → `Match` returns `false`.
  - Empty prefix in `NewRouter` → error (an empty prefix would match every path and shadow every other route).
  - Duplicate prefix in `NewRouter` → error (operator typo'd the same prefix twice).
- `internal/proxy` (W20 commit): per-route reverse-proxy adapter wraps `httputil.ReverseProxy` and forwards correlation ID. An httptest backend asserts the outbound request carries the `X-Correlation-ID` value the inbound request had.
- `internal/middleware` (W20 commit): correlation-ID middleware mints a UUID v4 when the header is absent and preserves it when present. The structured-log middleware writes one JSON line per request with the documented field set.
- `cmd/decision-gateway` (W20 commit): an e2e test boots the gateway against an httptest markup-svc-like backend, sends a request through, asserts the JSON access log line contains the expected `target`, `status`, `route`, and `correlation_id` fields.
