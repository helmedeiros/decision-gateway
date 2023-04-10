# 6. h2c (HTTP/2 cleartext) upstream transport

## Status

Accepted — `--upstream-h2c` (default false) switches the per-route `httputil.ReverseProxy.Transport` from the tuned `http.Transport` to a `golang.org/x/net/http2.Transport{AllowHTTP: true, ...}`. When on, the gateway speaks HTTP/2 cleartext to backends via prior knowledge; one TCP connection per backend carries N concurrent requests. When off, the existing HTTP/1.1 transport with `MaxIdleConnsPerHost=128` keeps working. The compose flips the flag on now that markup-svc v0.1.11 supports h2c.

## Context

ADR-0005 dropped the gateway → markup-svc network hop from ~600 µs to ~310 µs median by tuning `MaxIdleConnsPerHost`. That removed the TCP handshake tax for the >2-conn-per-host case but kept HTTP/1.1's per-connection serialization: each TCP connection can only serve one in-flight request at a time, so 500 QPS still needs 128 conns to absorb sustained concurrency. HTTP/2 multiplexing collapses that to one or two long-lived connections regardless of QPS.

markup-svc/ADR-0022 added h2c on the server side. This ADR is the matching client.

The flag is default-off so operators upgrading the gateway without upgrading markup-svc keep the working HTTP/1.1 path. Compose flips it on because the canonical platform stack runs them in lock-step.

## Decision

`internal/proxy` gains an `UpstreamProtocol` enum (`UpstreamHTTP1` default, `UpstreamH2C`). `proxy.New` takes it as a parameter. `newTunedTransport` dispatches: `UpstreamH2C` builds a `&http2.Transport{AllowHTTP: true, DialTLSContext: net.Dialer.DialContext}`; `UpstreamHTTP1` builds the existing tuned `http.Transport`. The `transportWrapper` (OTel instrumentation) wraps either base unchanged — the `gateway.proxy.upstream` span + traceparent injection work identically across protocols.

cmd adds `--upstream-h2c` bool flag. Two new tests exercise both branches: an h2c upstream sees `r.Proto == "HTTP/2.0"`; an HTTP/1.1 upstream sees `"HTTP/1.1"`.

## Consequences

### Closed

- HTTP/2 multiplexing on the gateway → markup-svc hop: one TCP conn per backend, frame-interleaved. Expected median drop from ~310 µs to ~50–100 µs at sustained QPS once the platform is on the v0.0.6 + markup-svc v0.1.11 pair.
- Pool-sizing flags (`--upstream-max-idle-conns`, `--upstream-idle-timeout`) become moot in h2c mode — `http2.Transport` manages connections differently. Operators flipping the flag can leave the pool flags at their defaults.
- HTTP/1.1 fallback remains the safe default; rolling upgrades flip h2c on after the backend supports it.

### Not closed

- TLS-terminated HTTP/2 to upstreams. Out of scope for the current dev posture (Docker network is plaintext); production deployments with TLS-fronted backends use `http2.ConfigureTransport(httpTransport)` instead.
- ALPN negotiation. `AllowHTTP: true` uses prior knowledge — the gateway sends HTTP/2 frames directly. A future TLS-terminated path would do ALPN.
- gRPC. The protocol substrate (h2c) is in place; turning gRPC on is a backend-side decision.
- Measurement bar. The latency drop is the operator-visible value; needs a fresh trace sample after the compose bump to confirm.
