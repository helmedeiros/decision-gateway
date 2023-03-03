# decision-gateway

HTTP front door for the Pricing Decision Platform. Routes incoming `/decide` and `/admin/*` traffic at backend services (markup-svc today; more decision-shaped backends as the platform grows), stamps a correlation ID on every request, and emits one structured JSON log per request for downstream aggregation.

## Status

Pre-release. The v0.0.1 surface from ADR-0001 ships in the current development cycle: longest-prefix Router + per-route `httputil.ReverseProxy` adapter + correlation-ID middleware + JSON access log middleware + `/healthz` / `/readyz` probes + flag-driven cmd binary. Tag `v0.0.1` lands once the Dockerfile + image-publish workflow update + docker-compose rewire land.

```sh
./decision-gateway \
  --listen=:8090 \
  "--route=/decide=>http://markup-svc:8080" \
  "--route=/admin=>http://markup-svc:8080"
```

The boot event lands on stdout as one JSON line describing the listen address, route table, and backend timeout. Each request emits one `gateway.access` JSON line with `method`, `path`, `status`, `duration_ms`, `route`, `correlation_id`.

## Capability matrix

| Capability | Surface | Status |
|---|---|---|
| Longest-prefix Router | `internal/gateway.Route` + `internal/gateway.Router` | ✅ |
| Reverse-proxy adapter (per-route `httputil.ReverseProxy`) | `internal/proxy` | ✅ |
| Correlation-ID middleware (read `X-Correlation-ID` or mint UUID v4) | `internal/middleware.CorrelationID` | ✅ |
| Structured JSON access log (interops with traffic-gen `jsonlog` schema) | `internal/middleware.AccessLog` | ✅ |
| `/healthz` + `/readyz` probes (matches markup-svc ADR-0013 shape) | `internal/httpapi.Healthz` + `Readyz` | ✅ |
| Flag-driven cmd (`--listen`, `--route` repeatable, `--backend-timeout`) | `cmd/decision-gateway` | ✅ |
| Dockerfile + image-publish CI | `cmd/decision-gateway/Dockerfile` + workflow | ⏳ next release window |
| docker-compose stack (markup-svc + gateway + traffic-gen) | `docker-compose.yaml` | ⏳ next release window |
| mTLS / JWT / WAF | not yet | ⏳ deferred to its own ADR when a real consumer asks |
| Retries / circuit breakers / weighted routing | not yet | ⏳ deferred per ADR-0001 NOT-closed |
| Hot reload of the route table (e.g., `POST /admin/routes`) | not yet | ⏳ deferred per ADR-0001 NOT-closed |

## Architecture

Hexagonal. `internal/gateway.Route` declares a path prefix and a backend URL; `internal/gateway.Router` selects a Route by longest-prefix match; `internal/proxy.Handler` wraps the Router in an `http.Handler` that forwards via a per-route `httputil.ReverseProxy`. The cross-cutting middlewares live in `internal/middleware`; the gateway's own HTTP endpoints (`/healthz`, `/readyz`) live in `internal/httpapi`. `cmd/decision-gateway` is the application: flag parsing, lifecycle, structured boot event.

| Package | Role |
|---|---|
| `internal/gateway` | domain: `Route`, `Router`, longest-prefix-match selection logic |
| `internal/middleware` | cross-cutting HTTP: correlation-ID propagation + structured JSON access log + `RouteRecorder` interface for inner-to-outer route stamping |
| `internal/proxy` | reverse-proxy adapter: per-route `httputil.ReverseProxy`, correlation-ID propagation to backend, route stamping on writer |
| `internal/httpapi` | gateway-owned HTTP endpoints: `Healthz`, `Readyz` (probes shaped to match markup-svc ADR-0013) |
| `cmd/decision-gateway` | application: `--listen`, `--route` repeatable, `--backend-timeout`; main/run split mirroring markup-svc and traffic-gen |

## Composition order

Wire `CorrelationID` OUTSIDE `AccessLog`. Go's `http.Request.WithContext` does not propagate inner-frame context mutations back out, so `AccessLog` reads the correlation ID from the request context only when stashed by a middleware that sits above it. The matched-route value flows the opposite way (inner-to-outer) via the writer-side `RouteRecorder` interface the proxy stamps. See [`internal/middleware/doc.go`](internal/middleware/doc.go) for the full rationale.

## Architecture decision records

See [`docs/architecture/decisions/`](docs/architecture/decisions/). ADR-0001 (Accepted) covers the custom-vs-off-the-shelf decision and the v0.0.1 menu.

## Cookbook

See [`docs/cookbook/`](docs/cookbook/). The day-one recipe walks through running markup-svc + decision-gateway + traffic-gen on one host and confirming the JSON access events flow.

## Standing rules

Inherited from markup-svc and traffic-gen:

- ADR for every architectural change (Status / Context / Decision / Consequences).
- `make ci-local` passes before every commit.
- 80% coverage floor enforced by CI.
- Conventional Commits (`type(scope): subject`).
- Annotated tags on every release; image publishing comes when the binary ships a Dockerfile.

## Building

```sh
go test ./...        # unit tests with -race
make ci-local        # the same checks CI runs
```

## License

MIT, matching the rest of the Pricing Decision Platform repos.
