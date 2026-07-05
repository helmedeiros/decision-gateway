# scientific/platform-session-v0.1.0 — session-driven cross-layer harness (pre-registration)

Companion to `scientific/platform-v0.1.0/` (the pre-existing rate-mode harness driving `/decide` directly against markup-svc). This one drives the **session-mode arc**: traffic-gen `--session=N` → funnel-sim → markup-svc → MinIO → pricing-metrics-aggregator, with observability propagation into Jaeger + Prometheus + Elasticsearch.

Pre-registered per [markup-svc ADR-0012](https://github.com/helmedeiros/markup-svc/blob/main/docs/architecture/decisions/0012-scientific-harness.md). Two-commit discipline: this commit locks the bars; the measurement commit runs `scripts/scientific-platform-session.sh` against a live stack and fills in the measured column of `REPORT.md`. Bars do NOT move between the two commits.

## What this harness covers vs. its sibling

| Harness | Driver mode | Downstream services | New arc pieces? |
|---|---|---|---|
| `scientific-platform.sh` | `steady:N` rate | markup-svc (via decision-gateway) | no — pre-Learning-Loop |
| `scientific-platform-session.sh` (this) | `--session=N` | funnel-sim → markup-svc | funnel-sim, aggregator, session-mode |

Both share the same PromQL-anchored-window methodology (`@ phase_end_ts` modifier + settle-sleep + `wait_for_sample`) and the same PASS/FAIL bar-table output shape. Operators re-run either against the same stack; failing bars in one do not affect the other.

## Precondition

Both compose stacks up:

```bash
docker compose -f decision-gateway/docker-compose.yaml \
               -f decision-gateway/docker-compose.scientific-override.yaml up -d
docker compose -f pricing-observability/docker-compose.observability.yaml up -d
```

Plus a compose-side scrape target for markup-svc that agrees with the scientific-override port remap. If `docker-compose.scientific-override.yaml` moves markup-svc's host port off `:8080`, the pricing-observability Prometheus scrape target for markup-svc must move to match. See the harness's precondition check.

## Pre-registered bars (status: pre-registered)

Bars are conservative (each is 3–10× under first-principles cost) so the harness clears them on any reasonable laptop. The measurement commit fills in the measured column; bars stay pinned.

### Layer A — Session driver (traffic-gen)

| Bar | Threshold | Rationale |
|---|---|---|
| `Session_ImpressionsMatchExpected` | `abs(agg_imp - N*offers) / (N*offers) <= 0.15` | Aggregator's `pricing_impressions_last_window` matches `traffic-gen.session.done.searches × session-offers-per-search` within 15%. 15% covers legitimate window-boundary noise on short runs (2–3 events straddling the window edge); anything larger means the count is drifting. |

### Layer B — funnel-sim (the downstream stand-in)

| Bar | Threshold | Rationale |
|---|---|---|
| `Funnel_SearchP99` | `≤ 20 ms` | Wall-clock of `POST /search` incl. the N-way markup-svc fan-out. Session mode uses `offer_count=3` — 3 fan-outs at markup-svc's own p99 = 5 ms bar + serial fan-out + JSON encode + sink enqueue budgets to comfortably ≤ 20 ms. |
| `Funnel_BookingInitP99` | `≤ 5 ms` | No fan-out; pure state-map insert + event emit. Fits well under 5 ms. |
| `Funnel_ErrorRate` | `≤ 0.001` | funnel-sim's own error rate — a 5xx from `/booking/*` or `/search` means the state machine or the sink broke. |
| `Funnel_SinkDropRateIsZero` | `= 0` | funnel-events sink drop counter — `funnel_event_sink_dropped_total`. Any drop is a substrate incident. |

### Layer C — markup-svc

| Bar | Threshold | Rationale |
|---|---|---|
| `Markup_DecideP99` | `≤ 5 ms` | Same bar as the pre-Learning-Loop harness — session mode does not change markup-svc's decide path. Kept for cross-reference. |
| `Markup_DecideOkRate` | `≥ 0` | Absolute counter — at least one `outcome=ok` decision. Zero would mean either markup-svc isn't scraped or every fan-out is failing. |

### Layer D — pricing-metrics-aggregator (the always-on continuous surface)

| Bar | Threshold | Rationale |
|---|---|---|
| `Agg_ImpressionsMin` | `≥ 10` | Aggregator recognized the run's impressions. Below 10 = the funnel-events sink didn't flush before the aggregator tick, or the aggregator's Hive-prefix walk missed the batches. |
| `Agg_PurchasesMin` | `≥ 1` | At least one successful booking made it through the full state machine + sink flush. |
| `Agg_ConversionRateMin` | `≥ 0.05` | 5% floor. Below that either P_click is misconfigured OR the aggregator is measuring the WRONG window and picking up empty stretches. |
| `Agg_TrafficGenMatch` | `abs(agg_imp - tg_expected) / tg_expected ≤ 0.15` | Cross-check: aggregator's impressions match traffic-gen's `searches × offers`. Same threshold as Layer A bar (this is the same measurement from the other direction). |
| `Agg_RunFailureRateIsZero` | `= 0` | `pricing_metrics_aggregator_runs_total{outcome="error"}` did NOT tick during the run. |

### Layer E — Distributed tracing

| Bar | Threshold | Rationale |
|---|---|---|
| `Jaeger_ServicesMin` | `≥ 4` | traffic-gen + funnel-sim + markup-svc + decision-gateway all appear as service nodes in Jaeger. |
| `Jaeger_MultiServiceTracesMin` | `≥ 1` | At least one trace contains ≥ 3 service names — proves the W3C traceparent propagates across the whole chain. |

### Layer F — Substrate (MinIO)

| Bar | Threshold | Rationale |
|---|---|---|
| `MinIO_SearchBatchesMin` | `≥ 1` | funnel-events/search-v1/ has at least one gzipped-JSONL batch from the run. |
| `MinIO_BookingBatchesMin` | `≥ 1` | funnel-events/booking-v1/ has at least one batch. |

### Layer G — Structured logs (Elasticsearch)

| Bar | Threshold | Rationale |
|---|---|---|
| `ES_FunnelAccessLogsMin` | `≥ 1` | Filebeat picked up funnel-sim.access events, decoded them, and shipped them to `platform-logs-*`. |

### Layer H — Alerting (AlertManager rules loaded)

| Bar | Threshold | Rationale |
|---|---|---|
| `Alerts_LoadedCount` | `= 4` | All four pricing-metrics-aggregator alert rules (per pricing-observability ADR-0022) are loaded and evaluated. Fewer means a config drift. |
| `Alerts_AllInactiveUnderNominal` | `all rules state=inactive` | Under nominal traffic the alerts should not fire. A `firing` state during the harness run means either the harness misconfigured the traffic mix OR the rule threshold is mis-calibrated for the harness. |

## What's NOT bar-tested here

- **markup-svc `_labeled` gauges from aggregator ADR-0002.** The harness runs the aggregator with default `--include-labels=""` (env-only surface). Per-arm / per-segment bars are the next harness (`platform-session-labeled-v0.1.0`).
- **AlertManager end-to-end delivery.** Rules being loaded is bar-tested; whether an actual alert reaches the alert-sink is out of scope. `pricing-observability/scripts/verify-registry-observability.sh` shows the shape for that when the model-registry alert path lands.
- **Sustained-load performance.** This is a functional cross-layer verification, not a load test. The rate-mode `scientific-platform.sh` is where 500 RPS bars live.

## Files in this directory

- `README.md` — this file (pre-registration).
- `REPORT.md` — measured values (filled in by the measurement commit).
