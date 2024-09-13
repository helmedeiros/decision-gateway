# scientific/platform-v0.1.0 — cross-layer harness report

Pre-registered bars + measured values for the **full Pricing Decision Platform pipeline** under controlled multi-rate load. See [markup-svc ADR-0012](https://github.com/helmedeiros/markup-svc/blob/main/docs/architecture/decisions/0012-scientific-harness.md) for the underlying scientific-harness methodology and the two-commit pre-registration discipline.

This is the platform-wide harness — it complements the per-component harnesses already shipped:

- `markup-svc/scientific/v0.1.4/` — engine + decorator microbenchmarks (`/decide` core)
- `markup-svc/scientific/v0.1.19/` — body-based reload
- `markup-svc/scientific/v0.1.22/` — shadow-Decider dispatchShadow path
- `markup-svc/scientific/v0.1.23/` — decision-sink Publish hot path
- `model-registry/scientific/v0.0.4-v0.0.6/` — registry substrate + observability + shadow side
- `traffic-gen/scientific/v0.0.X/` — generator throughput

What none of those measure is **the production-realistic pipeline end-to-end** — traffic-gen → decision-gateway → markup-svc → MinIO under sustained load at different rates. This harness fills that gap.

> **Status: pre-registered.** Bars are committed below. The measurement commit runs the harness against a live compose stack and populates the measured columns. Bars do NOT move between pre-registration and measurement.

## What the harness drives

The canonical decision-gateway `docker-compose.yaml` boots traffic-gen → decision-gateway → markup-svc → MinIO + model-registry as a four-service runtime. `scripts/scientific-platform.sh` swaps the default ramp profile for a multi-phase constant-rate driver (`steady:50` → `steady:200` → `steady:500`, 60 seconds each) and scrapes Prometheus on each phase boundary to capture per-layer p99 latency + per-layer event throughput + sink-side delivery counts.

## Precondition

- `decision-gateway/docker-compose.yaml` is running. `docker compose up -d` from this repo, with the platform stack healthy (`docker compose ps --format json` shows all services Up).
- `pricing-observability/docker-compose.observability.yaml` is running alongside it. Prometheus exposes the platform metrics scrape at `http://localhost:9090`.
- The harness queries Prometheus directly; the operator does NOT need a separate Prometheus client.

If those preconditions are not met, the harness aborts with an actionable error before driving any load.

## Reference host

- Local-laptop default reference: Apple M4 / arm64 / macOS, Docker Desktop. Same host class as the per-component harnesses.
- All bars carry generous timer-instrumentation headroom so the harness clears them on any reasonable laptop. A 5× margin against the underlying first-principles cost is the default unless documented otherwise.

## Pre-registered bars (status: pre-registered)

The harness asserts each bar at every phase. A failing bar at any phase fails the harness; the per-phase output table makes the failing rate visible.

### Layer A — Driver (traffic-gen)

The traffic-gen side proves the harness is actually driving the load it advertises. Without this the downstream measurements are meaningless.

| Benchmark | Bar | Layer | Why this bound |
|-----------|-----|-------|----------------|
| `Driver_RateMatchesTarget` | abs(measured − target) / target ≤ 0.10 | `traffic_request_total{outcome="ok"}` rate over the phase window | Honest sanity check that traffic-gen is hitting the requested rate. ±10% allows for the first/last second of each phase being partial and for the bench host being mildly contended. A wider drift means the harness is not actually driving what the report says it is. |

### Layer B — Edge proxy (decision-gateway)

| Benchmark | Bar @ 50 RPS | Bar @ 200 RPS | Bar @ 500 RPS | Layer | Why this bound |
|-----------|--------------|---------------|---------------|-------|----------------|
| `Gateway_RequestP99` | p99 ≤ 10 ms | p99 ≤ 10 ms | p99 ≤ 10 ms | `gateway.request` server-span p99 from `traces_spanmetrics_duration_milliseconds_bucket{service_name="decision-gateway",span_kind="SPAN_KIND_SERVER"}` | The gateway is a passthrough proxy with minimal in-process work. At any of the tested rates it should clear 10 ms p99 with substantial headroom; the 10 ms ceiling is the existing `GatewayRequestP99Slow` alert threshold, so this bar is the same one operators are already monitoring against. |
| `Gateway_UpstreamP99` | p99 ≤ 8 ms | p99 ≤ 8 ms | p99 ≤ 8 ms | `gateway.proxy.upstream` span p99 | The upstream-call span is gateway work + markup-svc work. The 8 ms ceiling tracks the v0.0.X gateway harness's measured p99 (629 µs) with a ~13× generous margin to absorb cross-laptop variance. |
| `Gateway_ErrorRate` | ≤ 0.1% | ≤ 0.1% | ≤ 0.1% | `gateway_requests_total{status=~"5.."}` / `gateway_requests_total` | The gateway is the canonical edge for the platform; any non-zero 5xx rate at sustained traffic means a real defect. The 0.1% ceiling is the existing alert threshold. |

### Layer C — Decision engine (markup-svc)

| Benchmark | Bar @ 50 RPS | Bar @ 200 RPS | Bar @ 500 RPS | Layer | Why this bound |
|-----------|--------------|---------------|---------------|-------|----------------|
| `Markup_DecideP99` | p99 ≤ 5 ms | p99 ≤ 5 ms | p99 ≤ 5 ms | `markup_decide_duration_seconds` histogram p99 | Matches the `MarkupDecideP99Slow` alert threshold. v0.1.4 measured the engine + decorators at ~2.4 µs p99; the 5 ms ceiling covers JSON / HTTP / shadow-goroutine envelope. |
| `Markup_ErrorRate` | ≤ 0.1% | ≤ 0.1% | ≤ 0.1% | `markup_decide_total{outcome="error"}` / `markup_decide_total` | Same threshold as the `MarkupDecideErrorRateHigh` alert. |

### Layer D — Decision-event substrate (markup-svc → MinIO)

This is the freshly-shipped path from ADR-0036. The bars below validate the operational claims ADR-0036 makes.

| Benchmark | Bar @ 50 RPS | Bar @ 200 RPS | Bar @ 500 RPS | Layer | Why this bound |
|-----------|--------------|---------------|---------------|-------|----------------|
| `Sink_DropRateIsZero` | 0 drops | 0 drops | 0 drops | `markup_decision_sink_dropped_total{reason}` delta over the phase | Under sustained healthy MinIO at any of the tested rates, drops must stay at zero. A non-zero drop count means either the queue is undersized for the rate (raise `--decision-sink-queue-size`) or the substrate is unhealthy (page MinIO). At 500 RPS sustained for 60 s, 30k decision events are produced; a 10k-queue with flush every 5 m / 10 MB triggers flush by byte budget every ~15 s, so the queue never saturates. The bar is structural: zero, not "small". |
| `Sink_FlushedRateMatchesProduction` | flushed / produced ≥ 0.95 | flushed / produced ≥ 0.95 | flushed / produced ≥ 0.95 | `markup_decision_sink_flushed_total` / `markup_decide_total{outcome=ok}` | Most decisions should land in MinIO before the phase ends. The 0.95 floor accounts for events still buffered at phase-end that flush in the next window. A lower ratio means the flush goroutine is falling behind production (raise the batch sizes) or the goroutine is dead (page). |
| `Sink_AtLeastOneByteFlushedPerPhase` | bytes delta ≥ 1 | bytes delta ≥ 1 | bytes delta ≥ 1 | `markup_decision_sink_flushed_bytes_total` delta over the phase | Wire-level sanity check: zero bytes flushed under sustained load means the flush path is broken (credentials regression, bucket-policy denial, queue not draining). A direct object-count check via `mc ls` would catch a wrong-bucket regression that still emits bytes; that needs an object-count Prom metric on the sink and is parked in Not closed. |

## Method

```bash
# Precondition: both compose stacks healthy.
docker compose up -d                            # decision-gateway repo
cd ../pricing-observability && docker compose -f docker-compose.observability.yaml up -d
cd ../decision-gateway

# Run the harness. Prints a phase-by-phase PASS/FAIL table.
make scientific-platform
```

The driver:

1. **Verifies preconditions.** Probes `localhost:9090/-/healthy` (Prometheus), `localhost:8090/healthz` (gateway), `localhost:8080/healthz` (markup-svc — exposed on the gateway side in compose). Aborts with an actionable error on any miss.
2. **For each phase (50 / 200 / 500 RPS, 60 s each):**
   - Captures pre-phase counter snapshots via Prometheus `/api/v1/query` for every metric named in the bars.
   - Sets the traffic-gen profile (via a sidecar `docker compose run --rm` invocation that overrides `--profile=steady:N --duration=60s`).
   - Waits for the traffic-gen to exit (~60 s + drain).
   - Captures post-phase counter snapshots.
   - Computes per-bar deltas and percentiles via PromQL (histogram_quantile at end-of-phase + rate(...) over the phase window).
   - Compares to the pre-registered bar; tabulates PASS / FAIL.
3. **Outputs a measurement table** with one row per bar per phase, plus an aggregate PASS / FAIL summary.

The harness does NOT tear down the compose stack — operators iterate by re-running `make scientific-platform` against the same stack.

## Measured numbers (status: deferred to measurement commit)

_The measurement commit runs the harness on the reference host and fills the tables below. Format mirrors the per-component harness measurement tables — one row per phase per bar, with the measured value and the pass/fail verdict._

## Analysis

_To be filled in by the measurement commit, with at minimum:_

- Per-layer p99 trend across the three rates (does latency scale, plateau, or saturate?)
- Sink-side delivery ratio at each rate (does the flush goroutine keep up?)
- Any bar that cleared with thin margin (< 2× headroom) → call out as a regression-watch target for the next harness iteration.
- Cross-layer correlations: does gateway p99 track markup-svc p99, or do they diverge?

## Not closed (deferred to follow-on harness iterations)

- **Direct object-count assertion.** The current Sink-side wire check asserts bytes flushed, not object count. A wrong-bucket regression that still emits bytes would pass. Needs a new `markup_decision_sink_objects_total` counter on the sink (one increment per successful PUT) so the harness can assert object-count deltas directly.
- **Shadow-fast-path-specific bar.** A bar isolating the shadow-dispatch cost (separate from full /decide envelope) needs a dedicated histogram — `markup_shadow_dispatch_duration_seconds` or similar — that the markup-svc sink doesn't currently expose. Parked until the histogram lands.
- **Spike profile.** This harness uses steady-state phases. A spike test (`steady:200` → spike to `1500` for 10 s → back to `steady:200`) measures the queue's spike absorption behavior; valuable but not in v0.1.0.
- **Saturating rate.** None of the three phases push the platform to a clear bottleneck. A `steady:5000` phase would surface saturation — useful for capacity planning, requires a higher-spec bench host.
- **Long-window stability.** A 24 h sustained `steady:100` test would measure observability cost drift, GC pressure, file-handle growth. Multi-hour bench, deferred.
- **Multi-instance.** Today markup-svc is one container in the compose. Multi-instance behavior (sticky sessions, sink writes from N processes to the same bucket) is its own harness iteration.
- **Outcome attribution path.** Once outcome-event ingestion lands (deferred markup-svc ADR), the harness extends to drive synthetic outcomes back to the substrate and assert the join completeness. Linked from the C4 "Learning Loop" arrow.
