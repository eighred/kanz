#!/bin/sh
# SRE-01c steady-state verifier. Queries the OBS-01 Prometheus for the signals
# each chaos experiment's hypothesis asserts. Run it BEFORE (baseline), DURING
# (expect the degraded signal), and AFTER (expect recovery + no burn) a fault.
#
#   PROM=http://localhost:9090 ./verify.sh <experiment>
#   experiment in broker-kill | network-partition | latency-injection | pod-eviction
#
# Exit non-zero if a "must-hold" invariant is violated (no SLO fast-burn, no
# data loss) — so SRE-01d's GameDay workflow can gate on it. POSIX sh: it runs
# both at a dev shell and in the GameDay verify Task (curl image, /bin/sh).
set -eu
PROM="${PROM:-http://prometheus.kanz-observability.svc:9090}"
EXP="${1:?usage: verify.sh <experiment>}"

q() { # q <promql> -> scalar (empty if no data)
  curl -sgG "$PROM/api/v1/query" --data-urlencode "query=$1" \
    | grep -oE '"value":\[[0-9.]+,"[^"]*"\]' | grep -oE '"[^"]*"\]$' | tr -d '"]' || true
}

fail=0
check() { # check <label> <promql-bool> ; promql must return 1 (hold) / 0 (violated)
  v="$(q "$2")"
  if [ "$v" = "1" ]; then echo "  ok   $1"; else echo "  FAIL $1 (=$v)"; fail=1; fi
}
note() { echo "  ..   $1 = $(q "$2")"; }

echo "[$EXP] @ $PROM"

# Invariant across ALL experiments: no SLO is in fast burn (the dip must fit the
# error budget). This is the load-bearing "behaves as designed" assertion.
check "no SLO fast-burn" 'absent(ALERTS{alertname="SLOFastBurn",alertstate="firing"})'

case "$EXP" in
  broker-kill)
    note "bus consumer lag (expect rise then drain)" 'sum(kanz_bus_consumer_lag)'
    note "DLQ-routed errors (expect bounded)"        'sum(increase(kanz_bus_consume_total{result="error"}[10m]))'
    check "no sequence gap (events delayed, not lost)" 'sum(increase(kanz_data_gap_missing_total[15m])) == bool 0'
    ;;
  network-partition)
    note "max staleness lag (expect > 30s during)" 'max(kanz_data_staleness_lag_seconds)'
    check "no sequence gap on heal"                'sum(increase(kanz_data_gap_missing_total[15m])) == bool 0'
    ;;
  latency-injection)
    note "recompute p99 (expect bounded, breaker sheds)" 'histogram_quantile(0.99, sum(rate(kanz_risk_recompute_duration_seconds_bucket[5m])) by (le))'
    check "gateway latency not fast-burning" 'absent(ALERTS{alertname="SLOFastBurn",slo="query-latency",alertstate="firing"})'
    ;;
  pod-eviction)
    note "gateway 5xx rate (expect ~flat)" 'sum(rate(kanz_gateway_requests_total{code=~"5.."}[5m]))'
    note "recompute throughput (expect advancing)" 'sum(rate(kanz_risk_recompute_total[5m]))'
    ;;
  az-kill)
    # INFRA-01e: losing an AZ must leave the system serving (multi-AZ spread +
    # PDB + KEDA floor). Throughput keeps advancing; the survivors are spread
    # across the remaining zones.
    note "gateway 5xx rate (expect ~flat)" 'sum(rate(kanz_gateway_requests_total{code=~"5.."}[5m]))'
    note "recompute throughput (expect advancing)" 'sum(rate(kanz_risk_recompute_total[5m]))'
    check "no sequence gap (no data lost to the AZ loss)" 'sum(increase(kanz_data_gap_missing_total[15m])) == bool 0'
    ;;
  autoscale)
    # INFRA-01e: under injected lag the autoscaler must REACT — pending climbs,
    # then replicas rise to work it down (the PRED-14 premise). Run during the
    # lag injection; expect pending elevated AND the HPA above its floor.
    note "pending backlog (expect elevated during injection)" 'sum(kanz_bus_pending_messages{group="risk-engine"})'
    check "autoscaler scaled out (replicas > min 3)" 'max(kube_horizontalpodautoscaler_status_current_replicas{horizontalpodautoscaler=~".*risk-engine.*"}) > bool 3'
    ;;
  *) echo "unknown experiment: $EXP" >&2; exit 2 ;;
esac

exit $fail
