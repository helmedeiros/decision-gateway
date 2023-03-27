# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
