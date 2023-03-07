# Run the canonical three-service platform stack

## Problem

You want the full Pricing Decision Platform up in one command — traffic-gen pushing through decision-gateway into markup-svc — with structured logs from all three flowing through `docker compose logs`. This is the long-running, operator-facing setup that supersedes the bare-binary smoke from the [three-service-smoke](three-service-smoke.md) recipe.

## Recipe

The repo ships `docker-compose.yaml` at the root. Pull the three public images and bring everything up:

```sh
docker compose up
```

You should see:

```
markup-svc-1        | markup-server: listening on :8080 (3 rules, model v1, adapter inmemory, source /etc/markup/rules.csv)
decision-gateway-1  | {"time":"...","level":"info","msg":"gateway.boot","attrs":{"backend_timeout":"5s","listen":":8090","routes":[{"backend":"http://markup-svc:8080","prefix":"/decide"},{"backend":"http://markup-svc:8080","prefix":"/admin"}]}}
traffic-gen-1       | {"time":"...","level":"info","msg":"traffic-gen.boot","attrs":{"target":"http://decision-gateway:8090/decide","profile":{"kind":"exp","start_qps":10,"end_qps":500,"duration":"5m0s"}, ...}}
decision-gateway-1  | {"time":"...","level":"info","msg":"gateway.access","attrs":{"method":"POST","path":"/decide","status":200,"duration_ms":0.3,"route":"/decide","correlation_id":"..."}}
```

The default profile is `exp:10->500@5m` — five-minute exponential ramp from 10 to 500 QPS, then hold 500 until `docker compose down`. The default persona mix is the `default` preset from traffic-gen ADR-0002.

## Recipe — slice logs by service

```sh
# Just the gateway's structured access events:
docker compose logs --no-log-prefix -f decision-gateway | jq -c '. | {msg, attrs}'

# Just markup-svc's stdout:
docker compose logs -f markup-svc

# Filter the gateway access events down to 5xx only (useful for
# narrowing during an incident):
docker compose logs --no-log-prefix -f decision-gateway \
  | jq -c 'select(.msg == "gateway.access" and .attrs.status >= 500)'
```

## Recipe — change the load shape

Override traffic-gen's command with `docker compose run --rm` so the new container picks up your spec:

```sh
docker compose run --rm traffic-gen \
  --target=http://decision-gateway:8090/decide \
  --profile=linear:100->5000@15m \
  --preset=stress-no-match
```

Inside the compose network the gateway is reachable as `decision-gateway:8090` (not `localhost:8090`). The host's `localhost:8090` works via the published port mapping in `docker-compose.yaml`.

## Recipe — inspect the gateway from the host

```sh
curl http://localhost:8090/healthz
# {"status":"ok"}

curl http://localhost:8090/readyz
# {"status":"ready"}

# Send a request with a known correlation ID; the gateway propagates
# it to markup-svc and stamps it on the response.
curl -i -X POST \
  -H "X-Correlation-ID: from-host" \
  -H "Content-Type: application/json" \
  -d '{"customer_tier":"enterprise"}' \
  http://localhost:8090/decide
```

The `decision-gateway-1` container's logs will show one new `gateway.access` event with `correlation_id: "from-host"`.

## What's happening

The compose network puts the three services on the same internal DNS so they resolve each other by service name (`markup-svc`, `decision-gateway`). Only `decision-gateway`'s :8090 maps to the host port; markup-svc has no host-side exposure so a client outside the stack must go through the gateway. That's the "single front door" the platform sketch calls for.

The call chain on `/decide`:

```
host curl -> :8090 host port
          -> decision-gateway container :8090
              -> CorrelationID middleware (read or mint header)
                -> AccessLog middleware (start timer, wrap writer)
                  -> proxy.Handler (match /decide -> markup-svc:8080)
                    -> markup-svc container :8080
                      -> markup-svc engine returns Decision (or no-match)
                    <- response
              <- gateway access JSON event on stdout
              <- header X-Correlation-ID echoed on response
          <- host receives the response
```

traffic-gen does the same call chain, just driving it at a configurable rate.

## What to check after

- `docker compose ps` shows all three containers `running`.
- `curl http://localhost:8090/healthz` from the host returns `200`.
- `docker compose logs --no-log-prefix decision-gateway | head -1 | jq .` parses cleanly and shows `msg: "gateway.boot"` with the two configured routes under `attrs.routes`.
- `docker compose logs --no-log-prefix decision-gateway | grep "gateway.access" | wc -l` grows over time as traffic-gen pushes load.
- traffic-gen's stderr `poster: done` line (visible after `docker compose stop traffic-gen` or at the end of a `docker compose run --rm traffic-gen ...` invocation) shows `transport_errors=0` once the stack is stable.
- Each gateway access event carries `attrs.correlation_id` (either inherited from traffic-gen / curl headers or minted as UUID v4 by the gateway when absent). The same value appears in the corresponding markup-svc-side log if observability is wired further (next-release-window work).

## Mistakes to avoid

- **Pointing traffic-gen at markup-svc directly** (`http://markup-svc:8080/decide`): the request bypasses the gateway, the gateway's access log shows nothing, and you lose cross-service correlation. Always target the gateway when running the canonical stack.
- **Using `localhost` from inside a container**: a container's `localhost` is itself, not the host. Use the service name (`http://markup-svc:8080`, `http://decision-gateway:8090`). `localhost` works only from the host.
- **Trying to pull `decision-gateway:v0.0.1` before the tag publishes**: the compose file pins the tag for reproducibility, but the image only exists after the v0.0.1 tag push on the decision-gateway repo. Use `:main` if the tag is not out yet by overriding the image in a `compose.override.yaml`.
- **Forgetting to mount the rules.csv volume**: markup-svc fails boot with a "no rules" error if the volume mount is missing. The compose file ships the fixture in `compose-fixtures/`; do not delete that path.
- **Running `docker compose down` and then `docker compose up` immediately**: docker-compose tears down the network on `down`, so a follow-up `up` recreates it. Containers come up with the same names but fresh internal IPs — that's fine, but in-flight connections from any external client are reset.

## Relevant ADRs and flags

- decision-gateway [ADR-0001](../architecture/decisions/0001-http-gateway.md) — gateway design + composition.
- markup-svc ADR-0003 (`/decide` body shape), ADR-0013 (`/healthz` + `/readyz` probes).
- traffic-gen ADR-0001 (Generator + Poster ports), ADR-0002 (persona presets), ADR-0003 (rate profiles).
- gateway flags: `--listen`, `--route` (repeatable), `--backend-timeout`.
- traffic-gen flags: `--target`, `--profile`, `--qps`, `--preset`, `--seed`, `--duration`, `--timeout`.
- markup-svc flags relevant to this stack: `--rules`, `--listen`.
