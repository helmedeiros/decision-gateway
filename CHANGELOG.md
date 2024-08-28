# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `docker-compose.yaml` gains a `model-registry` service alongside markup-svc, decision-gateway, and traffic-gen. Image `ghcr.io/helmedeiros/model-registry:main` (pinned to main because v0.0.4 was tagged before the GHCR image job landed; next tag publishes `:vX.Y.Z` for pinning). Mounts a named `registry-data` volume at `/data` so the fsstore + fsstate + fsaudit SQLite files survive compose restarts; mounts `compose-fixtures/instances.json` so `mrctl promote --env production` routes to the in-stack `markup-svc`. Publishes host port 8091 to match the `pricing-observability` Prometheus scrape config (ADR-0019). OTel SDK env vars wire the registry as a trace HOP: mrctl opens the root span, the registry extracts traceparent and emits per-handler child spans (`registry.champion.commit_state`, `registry.audit.record`, `registry.deploy.push_to_instance`, `registry.deploy.readyz`), traceparent is injected on the outbound `/admin/reload` so markup-svc nests as a downstream service in the same trace.
- `compose-fixtures/instances.json` mapping `production → http://markup-svc:8080` so `mrctl promote --env production` succeeds out of the box against the compose stack.

## [0.0.9] - 2023-05-10

`gateway_requests_total` now populates the `route` label correctly. Closes ADR-0009.

### Fixed

- `statusRecorder.SetMatchedRoute` (AccessLog) and `statusWriter.SetMatchedRoute` (metrics) chain the call to the underlying writer if it also implements `RouteRecorder`. With Metrics wrapping AccessLog, the proxy stamps the route on AccessLog's wrapper; chaining propagates the stamp to the metrics writer underneath. Previously the metrics counter held `route=""` for every request.
- Regression test in `internal/middleware/route_chain_test.go` proves an outer recorder observes the route after AccessLog handles the stamp.

## [0.0.8] - 2023-04-26

Hot reload of the route table via `POST /admin/routes`. Operators reconfigure routes without restart — atomic swap, idempotent (replace-whole-table), validates before swapping. Closes ADR-0008.

### Added

- `internal/proxy.Holder` — wraps `*Handler` behind `sync.RWMutex` (same shape as markup-svc/ADR-0015's swap.Decider). `BuildConfig` captures the per-route options so every rebuilt `*Handler` keeps the same transport configuration.
- `internal/httpapi.RoutesAdmin(holder, errLog)` — `GET /admin/routes` returns the current table; `POST /admin/routes {"routes":[{"prefix":"...","backend":"..."}]}` validates + swaps. Validation failures return 400 and leave the old table serving.
- `cmd` flag `--routes-admin` (default off for backwards compatibility).
- ADR-0008.

### Performance impact

`--routes-admin` not set: zero ns delta vs v0.0.7 (the legacy `*proxy.Handler` is used directly). `--routes-admin` set: ~30 ns RLock pair per request inside `Holder.ServeHTTP`. Replace is ~50 µs operator-triggered, not on the hot path.

## [0.0.7] - 2023-04-19

Gateway-side Prometheus `/metrics` endpoint. Always-on signal complementing the OTel spanmetrics view (which requires `--otel-enabled` + the Collector). Closes ADR-0007.

### Added

- `internal/observability/metrics`: new package. `Sink` + `New() (*Sink, http.Handler)` constructs a private registry holding `gateway_requests_total` (counter, labels `method`/`route`/`status`) + `gateway_request_duration_seconds` (histogram, 14 sub-millisecond buckets, same labels). `Sink.Middleware` wraps the response writer, captures status + matched route via the existing `middleware.RouteRecorder` writer-stamp pattern.
- `cmd` flag `--metrics-enabled` (default off). When set, the gateway mounts `/metrics` and inserts the middleware inside the OTel tracing frame, outside AccessLog so labels and access events stay in sync.
- `github.com/prometheus/client_golang@v1.14.0` dep.
- ADR-0007.

## [0.0.6] - 2023-04-10

h2c upstream transport. `--upstream-h2c` (default false) switches the per-route reverse-proxy `Transport` from the tuned `http.Transport` to `http2.Transport{AllowHTTP: true}`. When on, one TCP connection per backend carries N concurrent requests via HTTP/2 multiplexing. Closes ADR-0006.

### Added

- `proxy.UpstreamProtocol` enum (`UpstreamHTTP1` / `UpstreamH2C`). `proxy.New` takes it as a parameter.
- `newH2CTransport` (h2c via prior knowledge) selected when the operator passes `--upstream-h2c`.
- `cmd` flag `--upstream-h2c`.
- `golang.org/x/net@v0.8.0` direct import.
- ADR-0006.

### Changed

- `proxy.New` signature: `func New(router, backendTimeout, pool, protocol, transportWrapper)`.

### Performance

Expected gateway → markup-svc hop median drops from ~310 µs (HTTP/1.1 with pool tuning) to ~50–100 µs (HTTP/2 multiplexed) at sustained QPS once the platform runs the v0.0.6 + markup-svc v0.1.11 pair. Validation lands with the compose bump.

## [0.0.5] - 2023-03-28

Upstream connection pool tuning. The per-route `httputil.ReverseProxy`'s outbound `Transport` is now constructed via `newTunedTransport` which sets `MaxIdleConnsPerHost: 128` (vs stdlib default of 2) and copies `http.DefaultTransport`'s sensible defaults for `TLSHandshakeTimeout` / `ExpectContinueTimeout` / `IdleConnTimeout`. Targets the dominant cost identified by the ADR-0017 trace work: the gateway → markup-svc network hop measured ~591 µs median on native arm64, mostly TCP open + close overhead because the previous transport silently used the stdlib's 2-conn-per-host cap. Closes ADR-0005.

### Added

- `proxy.PoolConfig{ MaxIdleConnsPerHost int; IdleConnTimeout time.Duration }`. Zero values fall back to defaults (128 / 90s) applied inside the new unexported `newTunedTransport` helper.
- `cmd/decision-gateway` flags `--upstream-max-idle-conns` (default 128) + `--upstream-idle-timeout` (default 90s). Operators with unusual fleet shapes tune; default is chosen for the canonical compose + typical multi-route deployments.
- ADR-0005 (Accepted): upstream connection pool tuning. Two design questions answered: flag-with-default vs hard-coded (pick flag — operator-tunable for unusual fleet shapes, sensible default for the canonical case), how many stdlib-default Transport fields to copy (copy `TLSHandshakeTimeout` + `ExpectContinueTimeout`; leave dial-level keep-alive alone until a real need appears).

### Changed

- `proxy.New` signature gains a `pool PoolConfig` third parameter (before `transportWrapper`). Test callers updated. The OTel-enabled binary still passes the InstrumentedTransport wrapper after the pool tuning — the wrap order is `InstrumentedTransport → TunedTransport → wire`.
- `newReverseProxy` delegates Transport construction to `newTunedTransport` instead of building a bare `&http.Transport{ResponseHeaderTimeout: timeout}`. Closes a latent bug where `--backend-timeout` set produced a Transport WORSE than the `http.DefaultTransport` (no TLS handshake timeout, no idle-conn timeout, no expect-continue timeout).

### Performance impact

- **Connection acquisition** drops from ~500-1000 µs (full TCP handshake + slow-start for the >2-conn excess) to ~10-100 ns (atomic pool pop) once the pool is warm. The first ~128 requests after a restart still open fresh conns.
- **Memory**: ~10 KB per idle conn × 128 × 1 backend = ~1.3 MB. Negligible.
- **Expected trace shift**: `gateway.proxy.upstream` median should drop from ~600 µs to ~100 µs at the same QPS; `traffic.request` total drops by roughly the same delta.

## [0.0.4] - 2023-03-27

Multi-arch image release. Mirror of markup-svc/ADR-0018 + traffic-gen/ADR-0005. Closes ADR-0004.

### Added

- `cmd/decision-gateway/Dockerfile`: `--platform=$BUILDPLATFORM` on the build stage + `ARG BUILDPLATFORM` / `ARG TARGETOS` / `ARG TARGETARCH` + GOARCH-aware build command. The stale "go.sum lands in a future release" comment is removed (OTel deps in v0.0.2 brought it in).
- `.github/workflows/ci.yml`: image-publish job gains `platforms: linux/amd64,linux/arm64`.
- ADR-0004 (Accepted): multi-arch image publish.

### Performance impact

CI build +30 seconds; runtime zero delta between native amd64 and arm64; Apple Silicon pull no longer triggers Rosetta-2 emulation. The Jaeger trace's per-hop network cost on Apple Silicon drops ~10x to native wire time.

## [0.0.3] - 2023-03-25

Patch release. Closes the Kibana → Jaeger context-switch tax for operators investigating access-log alerts. The gateway.access JSON event gains two strictly-additive fields — `attrs.trace_id` + `attrs.span_id` — read from the active OTel SpanContext at entry-write time when `--otel-enabled` is set. Kibana rows now carry a direct link to the matching Jaeger trace; the operator workflow becomes Discover → Trace in two clicks instead of three context switches.

### Added

- `internal/middleware/accesslog.go`: `accessAttrs` struct gains `TraceID` + `SpanID` fields with `json:"...,omitempty"` tags. The middleware reads `oteltrace.SpanContextFromContext(r.Context())` at entry-build time and writes the IDs when `IsValid()` is true. v0.0.1 + v0.0.2 schema stays a strict subset (consumers parsing the older shape do not break).
- ADR-0003 (Accepted): access log carries trace_id + span_id for log/trace correlation. One design question answered: where to read the SpanContext (pick end-of-frame; matches the lazy posture of the other entry-time reads, no extra writer-wrapper state).

### Performance impact

~110 ns per traced request (SpanContextFromContext lookup + IsValid + two String() calls), ~11 ns per un-traced request (lookup + IsValid only). Below the existing access-log encode + write cost (~1 µs per entry). Negligible at any realistic gateway throughput.

## [0.0.2] - 2023-03-22

Tracing release. The gateway becomes a W3C trace context hop: `--otel-enabled` bootstraps an OTLP gRPC TracerProvider + propagator, emits one `gateway.request` server span per inbound request, opens a `gateway.proxy.upstream` client span per upstream call, and injects the `traceparent` header on the proxied request so the upstream service (markup-svc v0.1.5+) joins the same trace. Operators investigating bottlenecks now see the gateway / engine cost split as two stacked bars in Jaeger UI instead of treating the gateway as a black box. Closes the gap pricing-observability ADR-0002 left for the platform's front door.

### Added

- `internal/observability/otel/`: new package with three files.
  - `bootstrap.go`: `Bootstrap(ctx, instrumentationName) (trace.Tracer, Shutdown, error)`. Constructs the OTLP gRPC exporter, the batched `sdktrace.TracerProvider`, the detected resource, the global W3C TraceContext + Baggage propagator. Same shape as markup-svc's ADR-0016 bootstrap with the propagator addition (the gateway is a trace-context hop; markup-svc was a leaf).
  - `middleware.go`: `Middleware(tracer, http.Handler) http.Handler`. Extracts incoming trace context, opens a `gateway.request` span, wraps the response writer in a `statusWriter` that captures status + implements the `RouteRecorder` interface the proxy already uses. Sets `http.method` / `http.target` / `http.status_code` / `http.route` / `gateway.duration_ms` attributes and Error status on 5xx.
  - `transport.go`: `InstrumentedTransport{Tracer, Inner}` implementing `http.RoundTripper`. Opens a `gateway.proxy.upstream` client span per outbound call, injects `traceparent` via the global propagator, sets `upstream.status_code` from the response.
- `--otel-enabled` flag on `cmd/decision-gateway`: bootstraps the OTel SDK, wires the middleware + transport wrapper. Without the flag, no OTel code is in the request path (zero overhead, preserves the v0.0.1 behavior).
- `docker-compose.yaml`: gateway service gains `--otel-enabled` + `OTEL_SERVICE_NAME=decision-gateway` + `OTEL_EXPORTER_OTLP_ENDPOINT=http://host.docker.internal:4317` + `OTEL_EXPORTER_OTLP_PROTOCOL=grpc` + `OTEL_TRACES_SAMPLER=parentbased_always_on` + `extra_hosts: host.docker.internal:host-gateway` (matches the markup-svc service wiring).
- ADR-0002 (Accepted): gateway-side OTel tracing + W3C trace context propagation. Three design questions answered: custom middleware vs `otelhttp` contrib (pick custom; 50 LOC matching the project's small-middleware shape vs a third dep tree), root-only span vs root + upstream child (pick two spans; the delta is the gateway-overhead the operator wants), middleware vs RoundTripper as the child-span site (pick RoundTripper; the window is exactly the upstream cost, propagator inject happens at header-finalization).

### Changed

- `internal/proxy/proxy.New` signature gains a third parameter `transportWrapper func(http.RoundTripper) http.RoundTripper`. `nil` keeps the v0.0.1 behavior; the OTel-enabled binary passes a closure that builds the `InstrumentedTransport`. Test callers updated.
- Middleware composition order: `CorrelationID(Middleware(AccessLog(mux)))` when `--otel-enabled` is set (was `CorrelationID(AccessLog(mux))` and stays that way when the flag is off). The span surrounds AccessLog so `gateway.duration_ms` on the span matches `attrs.duration_ms` on the access event by construction.

### Dependencies

- `go.opentelemetry.io/otel` v1.11.2, `go.opentelemetry.io/otel/sdk` v1.11.2, `go.opentelemetry.io/otel/trace` v1.11.2, `go.opentelemetry.io/otel/exporters/otlp/otlptrace` v1.11.2 + `otlptracegrpc` v1.11.2. Transitive: `google.golang.org/grpc` v1.51.0, `google.golang.org/protobuf` v1.28.1. Matches the markup-svc + pricing-observability OTel version line.

## [0.0.1] - 2023-03-10

First public release. decision-gateway ships as the HTTP front door for the Pricing Decision Platform: a custom Go reverse-proxy gateway with correlation-ID propagation, a structured JSON access log per request, `/healthz` + `/readyz` probes, and a flag-driven cmd binary that operators run alongside markup-svc (or any future decision-shaped backend).

### Added

- `internal/gateway`: domain types + longest-prefix-match selection. `Route{Prefix, Backend}` declares a path prefix and its backend URL; `Router` selects a Route by longest prefix match over an O(N) walk (sub-microsecond for the expected v0.0.x route counts; a precomputed trie can land if the menu grows). `NewRouter` rejects empty list, empty prefix, nil backend, and duplicate prefix at construction so operator misconfiguration surfaces loudly. Routes are immutable after construction; safe for concurrent use without external synchronization.
- `internal/middleware`: cross-cutting HTTP concerns. `CorrelationID` reads the inbound `X-Correlation-ID` header or mints a UUID v4 (homegrown 30-line encoder, no external UUID dependency); stashes the value on `r.Context()` via an unexported key; echoes it on the response. `AccessLog` wraps an `http.Handler` and emits one JSON line per response with the `{time, level, msg, attrs={method, path, status, duration_ms, route, correlation_id}}` shape that matches `traffic-gen/internal/jsonlog` exactly so an aggregator parses both with one schema. JSON writes serialized via `sync.Mutex` around the encode + write window. `RouteRecorder` is the writer-side interface inner handlers stamp to surface the matched route to `AccessLog` after `next.ServeHTTP` returns — necessary because Go's `r.WithContext` does not propagate inner-frame context mutations back out.
- `internal/proxy`: reverse-proxy adapter. `proxy.New(router, backendTimeout)` returns an `http.Handler` that matches the inbound path against `router`, stamps the matched prefix on the writer wrapper, propagates the correlation ID from `r.Context()` to the outbound request, and forwards via a per-route `httputil.ReverseProxy`. Per-route proxies are built once at construction and reused for every matching request; `httputil.ReverseProxy` internally pools HTTP/1.1 connections so the steady-state cost is one reused connection per backend. Misses return `404` with `{"error":"no route matched"}` so a misconfigured route table cannot silently bypass.
- `internal/httpapi`: gateway-owned HTTP endpoints. `Healthz` returns `200 {"status":"ok"}` on `GET`, `405 + Allow: GET` otherwise. `Readyz` calls a `Ready func() (string, bool)` closure on every probe; `200 {"status":"ready"}` when ready, `503 {"status":"not_ready","reason":"..."}` otherwise. Shaped to match markup-svc's `internal/httpapi` byte-for-byte so operators reading both projects see the same wire response.
- `cmd/decision-gateway`: flag-driven binary. `--listen` (default `:8090`), repeatable `--route=PREFIX=>BACKEND` (the arrow separator matches markup-svc's `--route` DSL shape; operators wrap in shell quotes), `--backend-timeout` (default `5s`). `main`/`run` split mirrors markup-svc and traffic-gen so a test drives the binary with a captured ctx + stdout/stderr + synthetic args. Boot emits `gateway.boot` JSON event on stdout describing the listen address, route table, and backend timeout. Production middleware composition is `CorrelationID(AccessLog(mux))` so the correlation ID is on the request context when the access log reads it.
- `cmd/decision-gateway/Dockerfile`: two-stage `golang:1.18` build with `CGO_ENABLED=0` + `-trimpath` + `-ldflags="-s -w"`; runtime stage is `gcr.io/distroless/static-debian11:nonroot`. Runs as user 65532 with no shell and a read-only filesystem outside the working directory; final image ~15MB.
- CI workflow image-publish job: builds on every push and PR; publishes to `ghcr.io/helmedeiros/decision-gateway` on main pushes (`:main` + `:sha-<8>`) and tag pushes (`:<tag>` + `:sha-<8>`). The `tags: ['v*']` trigger has been on the workflow since the day-one scaffold — no retroactive fix story like markup-svc v0.1.2.
- `docker-compose.yaml` at the repo root: canonical three-service stack (markup-svc + decision-gateway + traffic-gen) wired through the gateway. Only `:8090` exposes on the host; markup-svc is reachable only through the gateway (the "single front door" the platform sketch calls for).
- `compose-fixtures/rules.csv` mirrors markup-svc's testdata so the markup-svc container has rules to evaluate.
- `docs/cookbook/three-service-smoke.md` walks operators through the bare-binary smoke; `docs/cookbook/compose-stack.md` walks through `docker compose up`. Both validated against the real end-to-end wire before commit.
- ADR-0001 (Accepted): HTTP gateway for the Pricing Decision Platform. Three design questions answered: custom Go gateway vs adopting an off-the-shelf gateway (Envoy / Traefik / nginx), v0.0.1 feature set (reverse proxy + correlation-ID + structured log + healthz/readyz; mTLS / retries / weighted routing / hot reload all deferred), configuration shape (flag-driven `--route` DSL).
