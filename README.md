# decision-gateway

HTTP front door for the Pricing Decision Platform. Routes incoming `/decide` and `/admin/*` traffic at backend services (markup-svc today; more decision-shaped backends as the platform grows), stamps a correlation ID on every request, and emits one structured JSON log per request for downstream aggregation.

## Status

Pre-release. This is the day-one scaffold: ADR-0001 (Proposed) describes the v0.0.1 feature set (reverse proxy + correlation-ID middleware + structured access log + `/healthz` + `/readyz`) and the custom-Go-gateway-vs-off-the-shelf tradeoff. The first adapter and `cmd/decision-gateway` land in subsequent commits of the same release window.

## Architecture

Hexagonal. `internal/gateway.Route` declares a path prefix and a backend URL; `internal/gateway.Router` selects a Route by longest-prefix match; the HTTP integration (a future commit) wraps `httputil.ReverseProxy` per route and composes correlation-ID + structured-log middleware around it.

| Package | Role |
|---|---|
| `internal/gateway` | domain: `Route`, `Router`, the matching logic |

(More rows land as the HTTP middleware and the reverse-proxy adapter ship.)

## Architecture decision records

See [`docs/architecture/decisions/`](docs/architecture/decisions/). ADR-0001 covers the custom-vs-off-the-shelf decision and the v0.0.1 menu.

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
