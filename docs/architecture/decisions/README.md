# Architecture Decision Records

Each file in this folder captures one architecture decision made on the decision-gateway codebase, following the standard ADR shape (Status / Context / Decision / Consequences).

New decisions get the next number and a short kebab-case slug:

```
NNNN-short-decision-name.md
```

`scripts/check-adrs.sh` (wired into `make ci-local`) verifies that:

1. Every ADR file is indexed in this README.
2. Every README link points at a file that exists.
3. Every ADR file has a `## Status` line with one of: `Proposed`, `Accepted`, `Superseded by ADR-NNNN`, `Deprecated`.
4. Every ADR file has the four standard sections: `## Status`, `## Context`, `## Decision`, `## Consequences`.

## Index

| # | Title | Status |
|---|---|---|
| [0001](0001-http-gateway.md) | HTTP gateway for the Pricing Decision Platform | ✅ Accepted |
| [0002](0002-otel-tracing.md) | Gateway-side OTel tracing + W3C trace context propagation | ✅ Accepted |
| [0003](0003-access-log-trace-correlation.md) | Access log carries trace_id + span_id for log/trace correlation | ✅ Accepted |
| [0004](0004-multi-arch-images.md) | Multi-arch (linux/amd64 + linux/arm64) image publish | ✅ Accepted |
| [0005](0005-upstream-connection-pool-tuning.md) | Upstream connection pool tuning | ✅ Accepted |
| [0006](0006-h2c-upstream.md) | h2c (HTTP/2 cleartext) upstream transport | ✅ Accepted |
| [0007](0007-prometheus-metrics-endpoint.md) | Prometheus /metrics endpoint on the gateway | ✅ Accepted |
| [0008](0008-routes-hot-reload.md) | Hot reload of the route table via POST /admin/routes | ✅ Accepted |
| [0009](0009-route-stamp-chain.md) | Chain SetMatchedRoute through middleware wrappers | ✅ Accepted |
