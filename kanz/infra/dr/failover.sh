#!/bin/sh
# DR-01d — failover orchestration.
#
# Drives the region cutover in the order the architecture requires: durable state
# first (Postgres), then the log + spine (Kafka already replicated by DR-01a;
# rebuild NATS), then the stateless services, then traffic. Each step is
# idempotent and re-runnable, so a partial failover can be resumed.
#
# This is runbook-as-code: it IS the procedure in docs/runbooks/dr.md, executable.
# A human runs it during a declared incident or the DR-01e drill, watching each
# gate. It does not auto-trigger — region failover is a human decision.
#
#   DR_CTX=<dr-kube-context> ./failover.sh           # full cutover
#   DR_CTX=<ctx> STEP=postgres ./failover.sh         # run a single step
#
# Requires: kubectl (+ cnpg plugin), the DR cluster reachable as $DR_CTX.
set -eu

DR_CTX="${DR_CTX:?set DR_CTX to the DR cluster kube-context}"
DATA_NS="${DATA_NS:-kanz-data}"
MSG_NS="${MSG_NS:-kanz-messaging}"
SVC_NS="${SVC_NS:-kanz-services}"
STEP="${STEP:-all}"
k() { kubectl --context "$DR_CTX" "$@"; }

step() { [ "$STEP" = "all" ] || [ "$STEP" = "$1" ]; }

echo "== DR failover → $DR_CTX (step=$STEP) =="

# 1) Promote the Postgres warm standbys to standalone primaries (DR-01b). PITR
#    replicas become writable; services will repoint their DSN here.
#    kanz-books carries every PARITY-02 book-of-record store (accounting IBOR
#    ledger, alternatives fund book, wealth household book, datamaster golden
#    records + exception queue), so a DR cutover that omits it leaves those
#    services read-only — it MUST be promoted alongside risk + registry (PARITY-05e).
if step postgres; then
  echo "-- [1/5] promote Postgres replicas"
  for c in kanz-risk kanz-registry kanz-books; do
    k cnpg promote "$c" -n "$DATA_NS" || true   # no-op if already promoted
    k cnpg status   "$c" -n "$DATA_NS" | head -3
  done
fi

# 2) Bring up the NATS spine + provision streams. Kafka is already present in DR
#    (DR-01a MirrorMaker2 — verify it, don't recreate it).
if step messaging; then
  echo "-- [2/5] Kafka (DR-01a) reachable + NATS streams"
  k -n "$MSG_NS" get pods -l app=kafka -o name | head -1 >/dev/null \
    || { echo "FATAL: DR Kafka not present — DR-01a replication must be running"; exit 1; }
  k apply -f nats/ -n "$MSG_NS" 2>/dev/null || k apply -f ../nats/
  k -n "$MSG_NS" wait --for=condition=complete job/nats-bootstrap --timeout=180s || true
fi

# 3) Reconstruct the NATS live spine from the DR Kafka log (DR-01c).
if step rebuild; then
  echo "-- [3/5] rebuild NATS spine from the Kafka log"
  k -n "$MSG_NS" delete job/nats-rebuild --ignore-not-found
  k apply -f nats/rebuild-job.yaml
  k -n "$MSG_NS" wait --for=condition=complete job/nats-rebuild --timeout=600s
  k -n "$MSG_NS" logs job/nats-rebuild --tail=5
fi

# 4) Re-bootstrap services in DR: they read the promoted DSN from their CSI secret
#    (the DR overlay points it at the promoted cluster) and consume the rebuilt
#    spine. Scale up + wait for readiness.
if step services; then
  echo "-- [4/5] start services"
  k -n "$SVC_NS" scale deploy --all --replicas=2
  # Wait on every service the DR drill exercises a synthetic transaction against
  # (PARITY-05e), not just risk + gateway — an unready book-of-record service is a
  # store that promoted but does not serve. A missing deploy is tolerated (|| true)
  # so a partial topology doesn't wedge the failover.
  for d in risk-engine api-gateway accounting wealth datamaster market-data; do
    k -n "$SVC_NS" rollout status deploy/"$d" --timeout=300s || true
  done
fi

# 5) Shift traffic to the DR region. The global LB / DNS weight is the actual
#    cutover; this flips the gateway's external endpoint to DR.
if step traffic; then
  echo "-- [5/5] shift traffic to DR (confirm readiness first)"
  k -n "$SVC_NS" get deploy api-gateway -o jsonpath='{.status.readyReplicas}' | grep -q '[1-9]' \
    || { echo "FATAL: api-gateway not ready — not shifting traffic"; exit 1; }
  echo ">> Now flip DNS/global-LB weight to the DR region (provider-specific;"
  echo ">> e.g. Route53/Cloud DNS failover record or the ingress GSLB)."
fi

echo "== failover step(s) '$STEP' complete =="
