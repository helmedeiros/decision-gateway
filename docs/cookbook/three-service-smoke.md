# Run markup-svc + decision-gateway + traffic-gen on one host

## Problem

You want to confirm the three services talk on the wire: traffic-gen drives load at the gateway, the gateway forwards each request to markup-svc with the correlation ID propagated, and the gateway's JSON access log shows the per-request shape (route, status, duration_ms, correlation_id) operators want on a dashboard.

## Recipe

Build all three binaries:

```sh
# in markup-svc
go build -o ./markup-server ./cmd/markup-server

# in decision-gateway
go build -o ./decision-gateway ./cmd/decision-gateway

# in traffic-gen
go build -o ./traffic-gen ./cmd/traffic-gen
```

Start markup-svc on a non-default port:

```sh
./markup-server \
  --rules=./cmd/markup-server/testdata/rules.csv \
  --listen=:18080
```

Start the gateway on another port, pointing `/decide` and `/admin` at the markup-svc backend. Note the route values are quoted — `=>` contains `>` which most shells interpret as a redirection operator.

```sh
./decision-gateway \
  --listen=:18090 \
  "--route=/decide=>http://localhost:18080" \
  "--route=/admin=>http://localhost:18080"
```

You should see the boot event on the gateway's stdout (one JSON line; the `python3 -m json.tool` filter pretty-prints):

```sh
./decision-gateway ... | head -1 | python3 -m json.tool
# {
#   "time": "...",
#   "level": "info",
#   "msg": "gateway.boot",
#   "attrs": {
#     "backend_timeout": "5s",
#     "listen": ":18090",
#     "routes": [
#       {"backend": "http://localhost:18080", "prefix": "/decide"},
#       {"backend": "http://localhost:18080", "prefix": "/admin"}
#     ]
#   }
# }
```

Wait for the gateway to be ready:

```sh
until curl -fs http://localhost:18090/readyz > /dev/null; do sleep 0.1; done
```

Send a request through the gateway with a known correlation ID:

```sh
curl -i -X POST \
  -H "X-Correlation-ID: smoke-1" \
  -H "Content-Type: application/json" \
  -d '{"customer_tier":"enterprise"}' \
  http://localhost:18090/decide
```

The response carries `X-Correlation-ID: smoke-1` and the markup-svc decision body. The gateway's stdout shows one new JSON access event:

```json
{"time":"...","level":"info","msg":"gateway.access","attrs":{"method":"POST","path":"/decide","status":200,"duration_ms":0.4,"route":"/decide","correlation_id":"smoke-1"}}
```

Run traffic-gen against the gateway for two seconds at 100 QPS:

```sh
./traffic-gen \
  --target=http://localhost:18090/decide \
  --qps=100 \
  --duration=2s \
  --seed=99
```

You should see:

- traffic-gen's stderr ends with `poster: done attempts=200 ... successes=79 not_matches=92 ... transport_errors=0` (counts vary with the seed; the success/not-match split reflects markup-svc's three-rule testdata).
- The gateway's stdout has ~200 `gateway.access` JSON lines, each with a fresh `correlation_id` UUID v4 (traffic-gen does not set the header, so the gateway mints one per request).

## What's happening

The composition stack at the gateway is:

```
http.Server
  → CorrelationID middleware (reads X-Correlation-ID or mints UUID v4; stamps on response; stashes on ctx)
    → AccessLog middleware (starts timer, wraps response writer)
      → mux
          /healthz → httpapi.Healthz
          /readyz  → httpapi.Readyz
          /        → proxy.Handler
                       Router.Match(path) → Route
                       writer.SetMatchedRoute(prefix)  // for access log
                       outbound.Header.Set(X-Correlation-ID, ctx.id)  // for backend
                       httputil.ReverseProxy → backend
```

The CorrelationID middleware sits OUTSIDE AccessLog because Go's `http.Request.WithContext` does not propagate context mutations back out of inner frames — AccessLog reads the request context AFTER `next.ServeHTTP` returns, so the value has to be on the context before AccessLog runs. The matched-route value flows the opposite way (inner-to-outer) via a writer-side `RouteRecorder` interface the proxy stamps; AccessLog reads it off the wrapper after the inner returns. See `internal/middleware/doc.go` for the full rationale.

## What to check after

- `curl http://localhost:18090/healthz` returns `200` with `{"status":"ok"}`.
- `curl http://localhost:18090/readyz` returns `200` with `{"status":"ready"}`.
- A `POST /decide` to the gateway returns the same response a direct `POST /decide` to markup-svc would, with the gateway's `X-Correlation-ID` header echoed.
- The gateway's stdout boot event names every configured route under `attrs.routes`.
- Per-request access events on the gateway's stdout carry `attrs.route` matching the configured prefix and `attrs.correlation_id` matching the inbound header (or a fresh UUID v4 when the inbound header was absent).
- markup-svc's stdout shows traffic arriving — its own logs confirm the gateway forwarded the requests rather than the client landing on markup-svc directly.
- `transport_errors=0` in traffic-gen's summary once the gateway is ready (the `until` poll above eliminates the boot-window race that otherwise shows up as a small `transport_errors` count).

## Mistakes to avoid

- **Unquoted `--route` arguments**: `=>` contains `>` which most shells interpret as a redirection operator. Always quote: `"--route=/decide=>http://localhost:18080"`.
- **Skipping the readiness wait**: the gateway flips `/readyz` to 200 after a brief grace window. Starting traffic-gen before that produces a handful of `transport_errors` at the start of the run; the `until` curl loop above avoids the race.
- **Pointing traffic-gen at markup-svc directly** (not the gateway): the wire works, but the gateway's access log shows nothing — you lose the cross-service correlation IDs and the per-route latency view.
- **Forgetting `--listen` on different ports** for markup-svc and the gateway: both default to ports that may collide. The recipe uses `:18080` and `:18090` so a developer running other servers doesn't fight for the default ports.

## Relevant ADRs and flags

- decision-gateway [ADR-0001](../architecture/decisions/0001-http-gateway.md) — gateway design + composition.
- markup-svc ADR-0003 — `/decide` body shape (what the gateway forwards).
- markup-svc ADR-0013 — `/healthz` + `/readyz` (the gateway's own probes match this shape).
- traffic-gen ADR-0001 — Generator + Poster ports.
- gateway flags: `--listen`, `--route` (repeatable), `--backend-timeout`.
