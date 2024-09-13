#!/usr/bin/env bash
# Cross-layer scientific harness for the Pricing Decision Platform.
#
# Drives traffic-gen at three constant rates (50 / 200 / 500 RPS) for 60s
# each against the canonical compose stack and asserts per-layer bars
# captured in scientific/platform-v0.1.0/REPORT.md.
#
# Precondition: BOTH compose stacks healthy.
#   docker compose up -d                                                   # this repo
#   cd ../pricing-observability && docker compose -f docker-compose.observability.yaml up -d
#
# Usage:
#   ./scripts/scientific-platform.sh                  # default: 50/200/500 RPS, 60s each
#   PHASES="100,400" PHASE_SECONDS=30 ./scripts/scientific-platform.sh
#
# Outputs a PASS/FAIL table per phase per bar plus an aggregate verdict.
# Operators re-run against the same stack to iterate.

set -euo pipefail

PROM_URL="${PROM_URL:-http://localhost:9090}"
GATEWAY_URL="${GATEWAY_URL:-http://localhost:8090}"
MARKUP_URL="${MARKUP_URL:-http://localhost:8080}"
PHASES="${PHASES:-50,200,500}"
PHASE_SECONDS="${PHASE_SECONDS:-60}"

red()    { printf '\033[31m%s\033[0m' "$1"; }
green()  { printf '\033[32m%s\033[0m' "$1"; }
yellow() { printf '\033[33m%s\033[0m' "$1"; }
bold()   { printf '\033[1m%s\033[0m' "$1"; }

note()   { echo "$(bold "==>") $*" >&2; }
pass()   { echo "  $(green PASS)  $*" >&2; }
fail()   { echo "  $(red FAIL)  $*" >&2; }
skip()   { echo "  $(yellow SKIP) $*" >&2; }

FAILED=0

require() {
  local label="$1" url="$2"
  if ! curl -fsS "$url" >/dev/null 2>&1; then
    fail "precondition: $label unreachable at $url"
    FAILED=1
  fi
}

note "verifying preconditions"
require "Prometheus" "$PROM_URL/-/healthy"
require "decision-gateway /healthz" "$GATEWAY_URL/healthz"
require "markup-svc /healthz (via gateway path)" "$MARKUP_URL/healthz"
if [ "$FAILED" -ne 0 ]; then
  fail "precondition failure; see notes above. Boot both compose stacks then re-run."
  exit 2
fi
pass "preconditions OK"

# query — run a PromQL instant query and emit the first scalar/sample value.
# Returns NaN if no data. We always grep against well-known label sets so a
# missing series surfaces clearly.
query() {
  local q="$1"
  local v
  v=$(curl -fsS --get --data-urlencode "query=$q" "$PROM_URL/api/v1/query" \
    | python3 -c '
import json, sys
d = json.load(sys.stdin)
r = d.get("data", {}).get("result", [])
if not r:
    print("NaN"); sys.exit(0)
v = r[0].get("value", [None, "NaN"])[1]
print(v)
')
  if [ "$v" = "NaN" ]; then
    fail "PromQL returned empty series: $q"
    FAILED=1
    echo "0"
    return
  fi
  echo "$v"
}

wait_for_sample() {
  local q="$1" deadline=$((SECONDS + 30))
  while [ "$SECONDS" -lt "$deadline" ]; do
    local v
    v=$(curl -fsS --get --data-urlencode "query=$q" "$PROM_URL/api/v1/query" \
      | python3 -c 'import json,sys; d=json.load(sys.stdin); r=d.get("data",{}).get("result",[]); print(r[0]["value"][1] if r else "NaN")')
    [ "$v" != "NaN" ] && return 0
    sleep 1
  done
  return 1
}

# delta — instant query for the increase over a range.
delta() {
  local metric="$1" window="$2"
  query "sum(increase(${metric}[${window}]))"
}

# p99 from a histogram, over a window.
p99_ms() {
  local hist="$1" filter="$2" window="$3"
  query "histogram_quantile(0.99, sum by (le) (rate(${hist}_bucket{${filter}}[${window}]))) * 1000"
}

# Run one traffic-gen phase at the requested rate against the compose
# stack's existing traffic-gen target. Uses `docker compose run --rm` so
# we get a one-shot driver alongside the long-running one.
run_phase() {
  local rate="$1" seconds="$2" rc
  note "running phase: steady:${rate} for ${seconds}s"
  docker compose -f docker-compose.yaml run --rm --no-deps \
    -e TG_TARGET="http://decision-gateway:8090/decide" \
    traffic-gen \
    --target="http://decision-gateway:8090/decide" \
    --profile="steady:${rate}" \
    --duration="${seconds}s" \
    --otel-enabled \
    --metrics-listen=:9102
  rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "traffic-gen exited rc=$rc at rate=${rate}; downstream bars will assert against partial data"
    FAILED=1
  fi
}

assert_le() {
  local name="$1" got="$2" bar="$3"
  awk -v g="$got" -v b="$bar" 'BEGIN { exit !(g <= b) }' \
    && pass "$name: $got <= $bar" \
    || { fail "$name: $got > $bar"; FAILED=1; }
}

assert_eq_zero() {
  local name="$1" got="$2"
  awk -v g="$got" 'BEGIN { exit !(g+0 == 0) }' \
    && pass "$name: 0 drops" \
    || { fail "$name: $got drops (want 0)"; FAILED=1; }
}

assert_ge() {
  local name="$1" got="$2" floor="$3"
  awk -v g="$got" -v f="$floor" 'BEGIN { exit !(g >= f) }' \
    && pass "$name: $got >= $floor" \
    || { fail "$name: $got < $floor"; FAILED=1; }
}

run_and_assert_phase() {
  local rate="$1" seconds="$2" window
  window="${seconds}s"

  # Pre-phase counter snapshots (for delta-based bars only).
  local pre_drops pre_produced pre_flushed
  pre_drops=$(query 'sum(markup_decision_sink_dropped_total)')
  pre_produced=$(query 'sum(markup_decide_total{outcome="ok"})')
  pre_flushed=$(query 'sum(markup_decision_sink_flushed_total)')

  run_phase "$rate" "$seconds"
  wait_for_sample "sum(rate(markup_decide_total[${window}]))" \
    || { fail "Prometheus did not deliver a markup_decide_total sample within 30s of phase end"; FAILED=1; }

  note "asserting bars for phase: steady:${rate}"

  # Layer A — driver via the markup-svc decide counter (the sidecar
  # traffic-gen does not expose its prom listener to the host, so we
  # observe the rate as it lands at the consumer).
  local driver_rate
  driver_rate=$(query "sum(rate(markup_decide_total[${window}]))")
  awk -v g="$driver_rate" -v t="$rate" 'BEGIN { d=(g-t)/t; if (d<0) d=-d; exit !(d <= 0.15) }' \
    && pass "Driver_RateMatchesTarget: ${driver_rate} ≈ ${rate} (±15%)" \
    || { fail "Driver_RateMatchesTarget: ${driver_rate} vs ${rate} target"; FAILED=1; }

  # Layer B — gateway
  local gw_req_p99 gw_upstream_p99 gw_err_rate
  gw_req_p99=$(p99_ms "traces_spanmetrics_duration_milliseconds" \
    'service_name="decision-gateway",span_kind="SPAN_KIND_SERVER"' "$window")
  gw_upstream_p99=$(p99_ms "traces_spanmetrics_duration_milliseconds" \
    'service_name="decision-gateway",span_name="gateway.proxy.upstream"' "$window")
  gw_err_rate=$(query "sum(rate(gateway_requests_total{status=~\"5..\"}[${window}])) / sum(rate(gateway_requests_total[${window}]))")
  assert_le "Gateway_RequestP99 (ms)" "$gw_req_p99" 10
  assert_le "Gateway_UpstreamP99 (ms)" "$gw_upstream_p99" 8
  assert_le "Gateway_ErrorRate" "$gw_err_rate" 0.001

  # Layer C — markup-svc
  local mk_p99 mk_err_rate
  mk_p99=$(p99_ms "markup_decide_duration_seconds" '' "$window")
  mk_err_rate=$(query "sum(rate(markup_decide_total{outcome=\"error\"}[${window}])) / sum(rate(markup_decide_total[${window}]))")
  assert_le "Markup_DecideP99 (ms)" "$mk_p99" 5
  assert_le "Markup_ErrorRate" "$mk_err_rate" 0.001

  # Layer D — sink
  local post_drops post_produced post_flushed delta_drops delta_produced delta_flushed
  post_drops=$(query 'sum(markup_decision_sink_dropped_total)')
  post_produced=$(query 'sum(markup_decide_total{outcome="ok"})')
  post_flushed=$(query 'sum(markup_decision_sink_flushed_total)')
  delta_drops=$(awk -v a="$post_drops" -v b="$pre_drops" 'BEGIN { print a-b }')
  delta_produced=$(awk -v a="$post_produced" -v b="$pre_produced" 'BEGIN { print a-b }')
  delta_flushed=$(awk -v a="$post_flushed" -v b="$pre_flushed" 'BEGIN { print a-b }')
  assert_eq_zero "Sink_DropRateIsZero" "$delta_drops"

  local flush_ratio
  flush_ratio=$(awk -v f="$delta_flushed" -v p="$delta_produced" 'BEGIN { if (p<=0) print 0; else print f/p }')
  assert_ge "Sink_FlushedRateMatchesProduction" "$flush_ratio" 0.95

  local bytes_delta
  bytes_delta=$(query "sum(increase(markup_decision_sink_flushed_bytes_total[${window}]))")
  assert_ge "Sink_AtLeastOneByteFlushedPerPhase" "$bytes_delta" 1
}

IFS=',' read -r -a PHASE_ARR <<< "$PHASES"
for rate in "${PHASE_ARR[@]}"; do
  run_and_assert_phase "$rate" "$PHASE_SECONDS"
done

echo
if [ "$FAILED" -eq 0 ]; then
  pass "all bars cleared across $(echo $PHASES | tr , ' ') phases"
  exit 0
else
  fail "one or more bars failed; see per-phase notes above"
  exit 1
fi
