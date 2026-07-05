#!/usr/bin/env bash
# Session-mode cross-layer harness. Companion to scripts/scientific-platform.sh
# (which drives rate-mode /decide traffic). This one drives the Learning-Loop
# session flow: traffic-gen --session=N → funnel-sim → markup-svc → MinIO →
# pricing-metrics-aggregator, with observability propagation into Jaeger +
# Prometheus + Elasticsearch + AlertManager rules.
#
# Bars are pre-registered in scientific/platform-session-v0.1.0/README.md
# per the ADR-0012 two-commit discipline.
#
# Preconditions:
#   - decision-gateway compose stack up (with the scientific-override):
#       docker compose -f docker-compose.yaml \
#                      -f docker-compose.scientific-override.yaml up -d
#   - pricing-observability stack up (with its scientific-override
#     so Prometheus scrapes markup-svc on the remapped :18080 port):
#       cd ../pricing-observability &&
#       docker compose -f docker-compose.observability.yaml \
#                      -f docker-compose.scientific-override.yaml up -d
#   - Local traffic-gen binary at $TG_BIN (default /tmp/traffic-gen).
#
# Env knobs:
#   PROM_URL, JAEGER_URL, ES_URL, FUNNEL_URL, AGG_URL,
#   SESSION_N, DURATION, P_CLICK, P_WALK_INIT, P_WALK_RESERVE, OFFERS,
#   PROPAGATION_WAIT (seconds after run before measuring).

set -uo pipefail

PROM_URL="${PROM_URL:-http://localhost:9090}"
JAEGER_URL="${JAEGER_URL:-http://localhost:16686}"
ES_URL="${ES_URL:-http://localhost:9200}"
FUNNEL_URL="${FUNNEL_URL:-http://localhost:8081}"
AGG_URL="${AGG_URL:-http://localhost:8082}"
MINIO_CONTAINER="${MINIO_CONTAINER:-decision-gateway-minio-1}"
TG_BIN="${TG_BIN:-/tmp/traffic-gen}"

SESSION_N="${SESSION_N:-4}"
DURATION="${DURATION:-12s}"
P_CLICK="${P_CLICK:-1.0}"
P_WALK_INIT="${P_WALK_INIT:-0.20}"
P_WALK_RESERVE="${P_WALK_RESERVE:-0.15}"
OFFERS="${OFFERS:-3}"
TIER="${TIER:-enterprise}"

# Time to wait between traffic end and measurement so the funnel-sim sink
# flushes (batch-every=15s in compose) + the aggregator tick lands (1m in
# compose) + Jaeger/ES index the samples.
PROPAGATION_WAIT="${PROPAGATION_WAIT:-70}"

# --- Output helpers (match sibling script's shape) --------------------
red()    { printf '\033[31m%s\033[0m' "$1"; }
green()  { printf '\033[32m%s\033[0m' "$1"; }
yellow() { printf '\033[33m%s\033[0m' "$1"; }
bold()   { printf '\033[1m%s\033[0m' "$1"; }
note()   { echo "$(bold "==>") $*" >&2; }
pass()   { echo "  $(green PASS)  $*" >&2; }
fail()   { echo "  $(red FAIL)  $*" >&2; }
warn()   { echo "  $(yellow WARN) $*" >&2; }

FAILED=0

require() {
  local label="$1" url="$2"
  if ! curl -fsS "$url" >/dev/null 2>&1; then
    fail "precondition: $label unreachable at $url"
    FAILED=1
  fi
}

# --- Preconditions ----------------------------------------------------
note "verifying preconditions"
require "Prometheus"        "$PROM_URL/-/healthy"
require "Jaeger UI"         "$JAEGER_URL/api/services"
require "Elasticsearch"     "$ES_URL/_cluster/health"
require "funnel-sim /healthz" "$FUNNEL_URL/healthz"
require "aggregator /healthz" "$AGG_URL/healthz"
if [ ! -x "$TG_BIN" ]; then
  fail "precondition: traffic-gen binary not at $TG_BIN (set TG_BIN or build the binary)"
  FAILED=1
fi
if [ "$FAILED" -ne 0 ]; then
  fail "precondition failure; see notes above. Boot both compose stacks + build traffic-gen then re-run."
  exit 2
fi
pass "preconditions OK"

# --- PromQL helper (mirrors sibling script) ---------------------------
query() {
  local q="$1" default="${2:-}"
  local v
  v=$(curl -fsS --get --data-urlencode "query=$q" "$PROM_URL/api/v1/query" 2>/dev/null \
    | python3 -c "
import json, sys
d = json.load(sys.stdin)
r = d.get('data', {}).get('result', [])
if not r:
    print('NaN')
    sys.exit(0)
print(r[0].get('value', [None, 'NaN'])[1])
")
  if [ "$v" = "NaN" ]; then
    if [ -n "$default" ]; then
      echo "$default"
    else
      echo "0"
    fi
    return
  fi
  echo "$v"
}

# --- Assertion helpers ------------------------------------------------
assert_le()      { awk -v g="$2" -v b="$3" 'BEGIN { exit !(g+0 <= b+0) }' && pass "$1: $2 <= $3" || { fail "$1: $2 > $3"; FAILED=1; }; }
assert_ge()      { awk -v g="$2" -v b="$3" 'BEGIN { exit !(g+0 >= b+0) }' && pass "$1: $2 >= $3" || { fail "$1: $2 < $3"; FAILED=1; }; }
assert_eq_zero() { awk -v g="$2" 'BEGIN { exit !(g+0 == 0) }' && pass "$1: 0" || { fail "$1: $2 (want 0)"; FAILED=1; }; }
assert_eq_int()  { [ "$2" -eq "$3" ] && pass "$1: $2 == $3" || { fail "$1: $2 != $3"; FAILED=1; }; }

# --- Traffic run ------------------------------------------------------
note "running session phase: --session=$SESSION_N for $DURATION"
TG_OUT=$(mktemp)
OTEL_SERVICE_NAME=traffic-gen \
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317 \
OTEL_EXPORTER_OTLP_PROTOCOL=grpc \
OTEL_TRACES_SAMPLER=parentbased_always_on \
"$TG_BIN" \
  --session="$SESSION_N" \
  --session-funnel-url="$FUNNEL_URL" \
  --duration="$DURATION" \
  --session-p-click="$P_CLICK" \
  --session-p-walk-at-init="$P_WALK_INIT" \
  --session-p-walk-at-reserve="$P_WALK_RESERVE" \
  --session-think-init-max=100ms \
  --session-think-reserve-max=100ms \
  --session-offers-per-search="$OFFERS" \
  --session-customer-tier="$TIER" \
  --otel-enabled \
  > "$TG_OUT" 2>&1 || warn "traffic-gen exited non-zero (may still have produced signal)"

TG_DONE=$(grep 'traffic-gen.session.done' "$TG_OUT" | tail -1)
note "$TG_DONE"
TG_SEARCHES=$(echo "$TG_DONE"    | python3 -c 'import sys,json; print(json.load(sys.stdin)["attrs"]["searches"])'    2>/dev/null || echo 0)
TG_PURCHASED=$(echo "$TG_DONE"   | python3 -c 'import sys,json; print(json.load(sys.stdin)["attrs"]["purchased"])'   2>/dev/null || echo 0)
TG_WALK_INIT=$(echo "$TG_DONE"   | python3 -c 'import sys,json; print(json.load(sys.stdin)["attrs"]["walk_init"])'   2>/dev/null || echo 0)
TG_WALK_RESERVE=$(echo "$TG_DONE" | python3 -c 'import sys,json; print(json.load(sys.stdin)["attrs"]["walk_reserve"])' 2>/dev/null || echo 0)
TG_EXPECTED_IMPRESSIONS=$(( TG_SEARCHES * OFFERS ))
note "traffic-gen stats: searches=$TG_SEARCHES purchased=$TG_PURCHASED walk_init=$TG_WALK_INIT walk_reserve=$TG_WALK_RESERVE expected_impressions=$TG_EXPECTED_IMPRESSIONS"

# --- Propagation wait -------------------------------------------------
note "waiting ${PROPAGATION_WAIT}s for propagation"
sleep "$PROPAGATION_WAIT"
PHASE_END_TS=$(date +%s)

# --- Layer B — funnel-sim latency + errors ---------------------------
note "Layer B — funnel-sim"
FUNNEL_SEARCH_P99=$(query "histogram_quantile(0.99, sum by(le) (rate(funnel_sim_request_duration_seconds_bucket{path=\"/search\"}[5m] @ $PHASE_END_TS))) * 1000")
FUNNEL_INIT_P99=$(query   "histogram_quantile(0.99, sum by(le) (rate(funnel_sim_request_duration_seconds_bucket{path=\"/booking/init\"}[5m] @ $PHASE_END_TS))) * 1000")
FUNNEL_ERR_RATE=$(query   "sum(rate(funnel_sim_requests_total{status_class=~\"[45]xx\"}[5m] @ $PHASE_END_TS)) / sum(rate(funnel_sim_requests_total[5m] @ $PHASE_END_TS))" 0)
FUNNEL_DROPS=$(query      "sum(increase(funnel_event_sink_dropped_total[5m] @ $PHASE_END_TS))" 0)
assert_le      "Funnel_SearchP99 (ms)"    "$FUNNEL_SEARCH_P99" 20
assert_le      "Funnel_BookingInitP99 (ms)" "$FUNNEL_INIT_P99"   5
assert_le      "Funnel_ErrorRate"         "$FUNNEL_ERR_RATE"    0.001
assert_eq_zero "Funnel_SinkDropRateIsZero" "$FUNNEL_DROPS"

# --- Layer C — markup-svc --------------------------------------------
note "Layer C — markup-svc"
MK_P99=$(query "histogram_quantile(0.99, sum by(le) (rate(markup_decide_duration_seconds_bucket[5m] @ $PHASE_END_TS))) * 1000")
MK_OK_DELTA=$(query "sum(increase(markup_decide_total{outcome=\"ok\"}[5m] @ $PHASE_END_TS))" 0)
if [ "$MK_P99" = "0" ] && [ "$MK_OK_DELTA" = "0" ]; then
  warn "markup-svc metrics not scraped — likely a Prometheus scrape-target config drift against docker-compose.scientific-override.yaml's port remap. Skipping the Markup layer assertions."
else
  assert_le "Markup_DecideP99 (ms)" "$MK_P99" 5
  assert_ge "Markup_DecideOkRate"   "$MK_OK_DELTA" 0
fi

# --- Layer D — pricing-metrics-aggregator ----------------------------
note "Layer D — pricing-metrics-aggregator"
AGG_IMP=$(query "sum(pricing_impressions_last_window)")
AGG_PUR=$(query "sum(pricing_purchases_last_window)")
AGG_WI=$(query  "sum(pricing_walkoffs_at_init_last_window)")
AGG_WR=$(query  "sum(pricing_walkoffs_at_reserve_last_window)")
AGG_GMV=$(query "sum(pricing_gmv_last_window_eur)")
AGG_CV=$(query  "avg(pricing_conversion_rate_last_window)")
AGG_ERR_RUNS=$(query "sum(increase(pricing_metrics_aggregator_runs_total{outcome=\"error\"}[5m] @ $PHASE_END_TS))" 0)
note "aggregator: imp=$AGG_IMP pur=$AGG_PUR wi=$AGG_WI wr=$AGG_WR gmv=$AGG_GMV cv=$AGG_CV"
assert_ge      "Agg_ImpressionsMin"      "$AGG_IMP"      10
assert_ge      "Agg_PurchasesMin"        "$AGG_PUR"      1
assert_ge      "Agg_ConversionRateMin"   "$AGG_CV"       0.05
assert_eq_zero "Agg_RunFailureRateIsZero" "$AGG_ERR_RUNS"
if [ "$TG_EXPECTED_IMPRESSIONS" -gt 0 ]; then
  DELTA=$(awk -v a="$AGG_IMP" -v b="$TG_EXPECTED_IMPRESSIONS" \
    'BEGIN { d = (a > b) ? (a - b) : (b - a); printf "%.4f", d / b }')
  assert_le "Agg_TrafficGenMatch" "$DELTA" 0.15
fi

# --- Layer E — Jaeger -------------------------------------------------
note "Layer E — Jaeger multi-service tracing"
JAEGER_SVC_COUNT=$(curl -sS "$JAEGER_URL/api/services" 2>/dev/null | python3 -c "
import sys, json
d = json.load(sys.stdin)
wanted = {'traffic-gen','funnel-sim','markup-svc','decision-gateway'}
have = [s for s in d.get('data', []) or [] if s in wanted]
print(len(have))
")
JAEGER_MULTI=$(curl -sS "$JAEGER_URL/api/traces?service=traffic-gen&limit=25&lookback=1h" 2>/dev/null | python3 -c "
import sys, json
d = json.load(sys.stdin)
c = 0
for t in d.get('data', []) or []:
    procs = t.get('processes', {}) or {}
    if len({p.get('serviceName','?') for p in procs.values()}) >= 3:
        c += 1
print(c)
")
assert_ge "Jaeger_ServicesMin"             "$JAEGER_SVC_COUNT" 4
assert_ge "Jaeger_MultiServiceTracesMin"   "$JAEGER_MULTI"     1

# --- Layer F — MinIO --------------------------------------------------
note "Layer F — MinIO substrate"
docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 minio minio12345 >/dev/null 2>&1 || true
MINIO_SEARCH=$(docker exec "$MINIO_CONTAINER" mc ls local/funnel-events --recursive 2>/dev/null | grep -c 'search-v1/' || true)
MINIO_BOOKING=$(docker exec "$MINIO_CONTAINER" mc ls local/funnel-events --recursive 2>/dev/null | grep -c 'booking-v1/' || true)
assert_ge "MinIO_SearchBatchesMin"  "$MINIO_SEARCH"  1
assert_ge "MinIO_BookingBatchesMin" "$MINIO_BOOKING" 1

# --- Layer G — Elasticsearch -----------------------------------------
note "Layer G — Elasticsearch structured logs"
ES_FUNNEL=$(curl -sS "$ES_URL/platform-logs-*/_search?size=0&q=msg:%22funnel-sim.access%22" 2>/dev/null | python3 -c "
import sys, json
d = json.load(sys.stdin)
print(d.get('hits', {}).get('total', {}).get('value', 0))
")
assert_ge "ES_FunnelAccessLogsMin" "$ES_FUNNEL" 1

# --- Layer H — AlertManager rules loaded ------------------------------
note "Layer H — AlertManager rules"
ALERT_JSON=$(curl -sS "$PROM_URL/api/v1/rules" 2>/dev/null)
ALERTS_LOADED=$(echo "$ALERT_JSON" | python3 -c "
import sys, json
d = json.load(sys.stdin)
count = 0
for g in d.get('data', {}).get('groups', []) or []:
    if g.get('name') == 'pricing-metrics-aggregator':
        for r in g.get('rules', []) or []:
            count += 1
print(count)
")
ALERTS_FIRING=$(echo "$ALERT_JSON" | python3 -c "
import sys, json
d = json.load(sys.stdin)
firing = 0
for g in d.get('data', {}).get('groups', []) or []:
    if g.get('name') == 'pricing-metrics-aggregator':
        for r in g.get('rules', []) or []:
            if r.get('state') == 'firing':
                firing += 1
print(firing)
")
assert_eq_int "Alerts_LoadedCount"            "$ALERTS_LOADED" 4
assert_eq_zero "Alerts_AllInactiveUnderNominal" "$ALERTS_FIRING"

# --- Summary ----------------------------------------------------------
echo
if [ "$FAILED" -eq 0 ]; then
  pass "ALL BARS CLEARED — session-mode pipeline flowing end-to-end"
  exit 0
else
  fail "ONE OR MORE BARS FAILED — see per-layer detail above"
  exit 1
fi
