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
#    kanz-orders carries the whole ORDER PATH — the OMS order store (orders /
#    positions / position_fills) and both venue adapters' venue_orders views — and
#    is the one whose omission is SILENT rather than read-only: the OMS starts
#    against an unpromoted-or-empty store and logs SweepInterrupted count=0, the
#    same line a healthy clean start produces (#60). Promoting it promotes the
#    order and the exchange mapping as ONE timeline; a half-promoted order path
#    leaves the reconciler holding exchange orders it cannot attribute.
#    kanz-compliance carries the REG-02 evidence chain (audit_chain_links). Its
#    omission is the quietest of all: nothing fails, the platform simply comes up
#    able to trade and unable to prove anything about it.
#    Every cluster named in postgres/cluster.yaml belongs in this list —
#    test/arch/dr_postgres_coverage_test.go fails if a mapped cluster is missing
#    from it.
if step postgres; then
  echo "-- [1/5] promote Postgres replicas"
  for c in kanz-risk kanz-registry kanz-books kanz-orders kanz-compliance; do
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
  # so a partial topology doesn't wedge the failover — which also means a name
  # missing from this list looks exactly like a name in it that failed: nothing is
  # printed either way. Every covered service belongs here. `oms` is the money path
  # (kanz-orders); `alternatives` was covered by kanz-books but absent from this
  # list, so its readiness was never waited on and the omission was masked. The
  # venue adapters share kanz-orders with the OMS, and an OMS that is ready while
  # its adapters are not is an OMS that can admit an order it cannot work —
  # waiting on them is what makes the promoted order path usable rather than
  # merely present. `regulatory` (kanz-compliance) is here for the opposite
  # reason: nothing downstream blocks on it, so if it never becomes ready that is
  # visible only if something waited.
  #
  # !! `tv-sync` IS WAITED ON HERE AND STEP 4's OWN `scale --all --replicas=2`
  #    ABOVE BREAKS IT. infra/deploy/tv-sync-deploy.yaml pins replicas: 1 and
  #    states it is a CORRECTNESS bound, not a capacity one: two pods split the
  #    fact stream, so each folds part of it and every pod's book is wrong. The
  #    blanket scale overrides that pin on every failover. This wait line does not
  #    cause it — the scale does — but tv-sync is listed rather than quietly
  #    omitted precisely so the conflict is visible instead of being an absence
  #    nobody reads. Left as-is deliberately: narrowing `--all` is a change to
  #    every service's failover behaviour and belongs to its own issue, not to
  #    #60's store placement.
  for d in risk-engine api-gateway accounting alternatives wealth datamaster oms venue-binance venue-okx regulatory tv-sync market-data; do
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
