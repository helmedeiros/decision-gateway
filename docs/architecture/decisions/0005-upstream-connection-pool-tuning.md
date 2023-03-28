# 5. Upstream connection pool tuning

## Status

Accepted — the per-route `httputil.ReverseProxy`'s `Transport` is built via `newTunedTransport` which sets `MaxIdleConnsPerHost: 128` (vs stdlib default of 2) and `IdleConnTimeout: 90s` so steady-state QPS reuses TCP connections instead of opening + closing per request. Operators tune via `--upstream-max-idle-conns` + `--upstream-idle-timeout` flags. The change targets the dominant bottleneck identified by ADR-0017's instrumented trace: the gateway → markup-svc network hop measured ~591µs on native arm64 after ADR-0004 (multi-arch) landed, and at typical platform QPS that cost is mostly TCP open + close overhead rather than wire time.

## Context

ADR-0002 shipped the gateway-side OTel spans + W3C trace context propagation. ADR-0004 (multi-arch images) dropped the Apple Silicon emulation tax — the dev-stack Jaeger trace now reads native arm64 wire times. The latest measured per-span medians on native arm64:

| Span | Median duration |
|---|---|
| `traffic.request` (total client-side) | 1416 µs |
| `gateway.request` (gateway server-side) | 769 µs |
| `gateway.proxy.upstream` (upstream RoundTrip) | 629 µs |
| `markup.decider.decide` | 38 µs |
| `markup.engine.evaluate` | 20 µs |

Cost breakdown by adjacent-span subtraction:

- traffic-gen → gateway network RT: 647 µs
- gateway internal (routing + proxy setup): 140 µs
- gateway → markup-svc network RT: 591 µs ← this ADR's target
- decider decorator overhead: 18 µs
- pure engine work: 20 µs

The two network hops add up to 1238 µs of the 1416 µs total — 87% of user-facing latency. The gateway → markup-svc hop is something the gateway can control (it owns the outbound transport); the traffic-gen → gateway hop is symmetric on the inbound side and out of scope here.

The current proxy code builds the per-route Transport like this:

```go
var base http.RoundTripper = http.DefaultTransport
if timeout > 0 {
    base = &http.Transport{
        ResponseHeaderTimeout: timeout,
    }
}
```

When `--backend-timeout` is set (the platform compose sets `5s`), the code constructs a bare `&http.Transport{ResponseHeaderTimeout: timeout}`. Every other field defaults to its zero value. The relevant one: `MaxIdleConnsPerHost: 0` falls back to Go's `DefaultMaxIdleConnsPerHost = 2`. At the platform's default `exp:10->500@5m` ramp profile, sustained QPS exceeds 2 concurrent connections within seconds. Every excess request opens a fresh TCP connection (~3-way handshake + slow-start), serves the request, then either closes (if the pool is full) or returns to the pool. The pool fills at 2 conns, so most requests close — TCP open + close overhead dominates the network hop.

The `http.DefaultTransport` path (used when `backendTimeout == 0`, which only happens in tests) has the same `MaxIdleConnsPerHost: 2` limitation but at least sets idle-conn-timeout + dial timeouts to sane defaults. So the production path is actually *worse* than the test path — a bug.

Two design questions.

### 1. Flag + sensible default vs hard-coded value

Picking a single hard-coded value (say, 128) is simpler. Operators with unusual deployment shapes — many backends, sharded fleet, etc. — would need a recompile.

Picking a flag with a sensible default (128) gives operators a tuning knob. Cost: two new flags (`--upstream-max-idle-conns` + `--upstream-idle-timeout`) on the cmd surface; flag-help-text-line count grows by two.

**Pick flag + default.** The default 128 handles the canonical compose + the typical multi-route deployment (3-5 backends, ~500 QPS); operators with bigger fleets dial up; operators investigating tail-latency dial down (with smaller pools, slow + flaky upstream connections fail faster). The flag pattern matches the rest of the cmd surface (`--backend-timeout` etc).

### 2. Should we also tune dial timeouts + keep-alive intervals?

The stdlib `http.DefaultTransport` sets a `net.Dialer{Timeout: 30s, KeepAlive: 30s}` and `TLSHandshakeTimeout: 10s`, `IdleConnTimeout: 90s`, `ExpectContinueTimeout: 1s`. The current bare `&http.Transport{ResponseHeaderTimeout: timeout}` skips all of these — they default to zero which means "no timeout / no keep-alive probe / immediate response wait."

The pragmatic choice: copy the `http.DefaultTransport` posture for the non-flag-tunable fields (TLSHandshakeTimeout, ExpectContinueTimeout) so the gateway behaves like a well-configured production client. The flag-tunable ones (`MaxIdleConnsPerHost`, `IdleConnTimeout`) are the ones operators want to control because they directly affect connection-pool behavior under load.

Connection-level keep-alive (`net.Dialer.KeepAlive`) is a TCP-layer feature distinct from HTTP keep-alive (the pool). Operators occasionally need to tune this for long-idle deployments behind aggressive NATs; we leave it at the zero-default for now and ship a flag if/when a real need appears.

**Pick: copy `http.DefaultTransport`'s non-tunable defaults, expose only the two pool levers as flags.**

## Decision

`internal/proxy/proxy.go`:

- New exported type `PoolConfig{ MaxIdleConnsPerHost int; IdleConnTimeout time.Duration }`. Zero values fall back to defaults applied inside `newTunedTransport` (128 / 90s).
- `proxy.New` signature gains a `pool PoolConfig` parameter (third position; before `transportWrapper`).
- New unexported helper `newTunedTransport(timeout, pool, transportWrapper)` constructs `&http.Transport{...}` with the pool values + the response-header timeout + stdlib-default values for the rest (`TLSHandshakeTimeout: 10s`, `ExpectContinueTimeout: 1s`, `MaxConnsPerHost: 0` for unlimited active, `DisableKeepAlives: false`, `MaxIdleConns: 0` for unlimited total — the per-host cap is the real lever).
- `newReverseProxy` delegates to `newTunedTransport` for its `rp.Transport`.
- Tests in `internal/proxy/proxy_test.go` updated to pass `proxy.PoolConfig{}` (uses defaults).

`cmd/decision-gateway/main.go`:

- New flags `--upstream-max-idle-conns` (int, default 128) + `--upstream-idle-timeout` (Duration, default 90s).
- A `proxy.PoolConfig{MaxIdleConnsPerHost: *upstreamMaxIdleConns, IdleConnTimeout: *upstreamIdleTimeout}` is built and passed into `proxy.New`.
- The flag help text names ADR-0005 so operators reading `--help` can find the context.

The default values (128 / 90s) are chosen to make the platform's canonical compose work well out of the box without anyone touching flags. The 128 number is big enough to absorb the platform's default 500-QPS sustained traffic against a single backend (typical idle pool occupancy = QPS × median request duration in seconds = 500 × 0.0006s = ~0.3 conns sustained; the headroom handles bursts and slow upstreams). The 90s idle timeout matches the historical `http.DefaultTransport` value and stays under typical NAT timeouts.

## Consequences

### Closed by this ADR

- Gateway → markup-svc network hop drops from ~591 µs median (TCP open + close per request at QPS > 2 sustained) to whatever wire RTT + serialization actually take. Expected: ~50-150 µs median on native Docker bridge, depending on body size + connection-pool warmth. Validation post-restart: rerun the load profile, compare the `gateway.proxy.upstream` median in Jaeger.
- The latent bug where `--backend-timeout 5s` produced a Transport WORSE than the default is closed: the tuned Transport copies the rest of `http.DefaultTransport`'s settings.
- Operators with multi-backend deployments (gateway routing to several markup-svc model versions) can size the per-host pool against their actual fleet — `--upstream-max-idle-conns=512` for big-fleet deployments, `--upstream-max-idle-conns=8` for small ones.

### NOT closed by this ADR

- TCP-level keep-alive probes (`net.Dialer.KeepAlive`). Lands behind a flag if a real long-idle-NAT-behind-aggressive-firewall case appears.
- HTTP/2 to the upstream. Multiplexing N requests on a single TCP connection eliminates the per-connection contention entirely, but markup-svc would need to upgrade its server to support HTTP/2 (Go's stdlib supports it on tls.Conn out of the box; plain-text HTTP/2 needs `golang.org/x/net/http2/h2c` on both ends). Worth doing for the platform; tracked separately.
- Outbound timeouts beyond the response-header timeout: idle / write / per-request total. Lands if a wedged-backend scenario actually bites.
- Per-route pool sizing. The current PoolConfig is gateway-wide; all routes share the same per-host pool size. For a multi-route deployment where one route is hot (`/decide`, 500 QPS) and another is cold (`/admin`, 1 QPS), the same 128 cap applies to both. The cost of an unused 128-conn pool on a cold backend is just the per-pool memory (~10 KB); not worth per-route tunability today.
- Adaptive sizing. The pool size is static. A future ADR could watch live QPS + adjust the pool, but that's a real-time-control-loop problem and operators have not asked.

### Performance impact

- **Connection acquisition cost**: drops from ~500-1000 µs (full TCP handshake + slow-start for excess requests beyond the 2-conn cap) to ~10-100 ns (atomic pop from the pool's free list) for the common case. The first 128 requests after a gateway restart still open new connections; after that, the pool is warm.
- **Memory**: ~10 KB per idle connection (Linux TCP socket overhead). At 128 conns per backend × 1 backend = ~1.3 MB. Negligible.
- **File descriptors**: 1 per idle conn. Default container ulimit is 1024 so the gateway can sustain ~7 backends at 128 conns each before hitting limits. Operators with bigger fleets bump the ulimit.
- **Latency tail**: the IdleConnTimeout = 90s default keeps idle conns alive long enough that a request after a 60-second QPS lull still finds a warm conn. Production NAT tables time out at ~5 min, so 90s is safely under that.
- **Connection-pool fairness**: when several requests race to grab the same slot, Go's connection pool serializes them. The 128-conn cap means up to 128 concurrent in-flight requests; beyond that, the 129th request waits until a slot frees. In a `MaxConnsPerHost: 0` (unlimited active) config, a 129th request opens a fresh conn instead of waiting — that's the right behavior for a latency-sensitive gateway.

### Validation strategy

- Unit tests stay green (the per-route Transport is internal to `proxy.New`'s construction; the test surface doesn't exercise the pool behavior directly, but exercises the unchanged-from-outside ServeHTTP path).
- Live smoke after the compose restart: the median `gateway.proxy.upstream` span duration in Jaeger drops from ~600 µs to ~100 µs at the same QPS. The `traffic.request` total drops by roughly the same delta. If the drop is smaller than expected, check ramp time — the pool needs ~128 requests to fully warm.
- The cookbook recipe in pricing-observability ADR-0002 keeps working: same compose flags, same traces, faster numbers.
