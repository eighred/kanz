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

# EVERY PATH BELOW IS RELATIVE TO THIS SCRIPT, NOT TO THE CALLER (#629).
#
# There was no cd, no dirname and no BASH_SOURCE, and the paths resolved only
# with cwd=infra/dr — which no document states and NEITHER RUNBOOK USES. Both
# invoke it from the module root (docs/runbooks/dr.md:33,
# docs/runbooks/dr-drill.md:68), where `nats/` and `../nats/` are two
# directories that do not exist. Under `set -eu` that aborted the cutover at
# step 2 of 5: Postgres promoted, no spine, no services, no traffic — and the
# only message named `../nats/`, two directories from the real path, because the
# first arm's stderr went to /dev/null.
#
# This is the one line that makes the runbooks' documented invocation work.
cd "$(dirname "${BASH_SOURCE[0]}")"

DR_CTX="${DR_CTX:?set DR_CTX to the DR cluster kube-context}"
DATA_NS="${DATA_NS:-kanz-data}"
MSG_NS="${MSG_NS:-kanz-messaging}"
SVC_NS="${SVC_NS:-kanz-services}"
STEP="${STEP:-all}"
k() { kubectl --context "$DR_CTX" "$@"; }

# wait_rollout_ready polls an Argo Rollout until readyReplicas reaches its
# spec, or the deadline passes (#629).
#
# POLLING RATHER THAN `kubectl rollout status`, and the reason is not style:
# that verb only understands Deployment, DaemonSet and StatefulSet. An Argo
# Rollout needs the `kubectl argo rollouts` plugin, which this script's own
# "Requires:" line does not list and a DR operator may not have. The status
# subresource needs no plugin at all.
wait_rollout_ready() {
  name="$1"
  deadline=$(( $(date +%s) + 300 ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    want="$(k -n "$SVC_NS" get rollout "$name" -o jsonpath='{.spec.replicas}' 2>/dev/null || true)"
    got="$(k -n "$SVC_NS" get rollout "$name" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
    if [ -n "$want" ] && [ "${got:-0}" -ge "$want" ]; then
      echo "rollout \"$name\" ready (${got}/${want})"
      return 0
    fi
    sleep 5
  done
  return 1
}

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
  for c in kanz-risk kanz-registry kanz-books kanz-orders kanz-compliance kanz-identity; do
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
  # THE SPINE MANIFESTS ARE NAMED, ONE BY ONE, AND NEVER THE DIRECTORY (#629).
  #
  # This was `k apply -f nats/ ... || k apply -f ../nats/`. `infra/dr/nats/`
  # contains only README.md and rebuild-job.yaml, so `apply -f nats/` applied the
  # REBUILD job (in step 2, before any NATS exists to publish into — step 3 then
  # deleted and re-applied it) and EXITED 0. The `||` therefore never fired, so
  # ../nats/ was never applied: no namespace, no StatefulSet, no nats-bootstrap.
  # The wait below then burned 180s on a Job that had never been created and
  # discarded the result with `|| true`, and step 2 reported success with no
  # spine standing.
  #
  # AND THE FALLBACK WOULD HAVE BEEN WORSE THAN THE BUG. `../nats/` also holds
  # bootstrap-job-dev-plaintext.yaml, whose own first line reads "DEV-ONLY
  # plaintext bootstrap. NEVER apply this to a cluster that has SPIRE." Applying
  # the directory during a declared incident would have applied that too. This is
  # the #92 shape a third time: a path convention that silently decides which
  # manifests exist.
  #
  # tenancy.yaml IS REQUIRED AND IS THE EASY ONE TO MISS. nats.conf does
  # `include "tenants.conf"`, which arrives from the nats-tenants ConfigMap that
  # file defines — without it the broker does not start. It is excluded from the
  # ApplicationSet on purpose ("applied by their own tooling"), which is exactly
  # why naming files rather than a directory has to name it explicitly.
  for m in namespace.yaml tenancy.yaml nats.yaml bootstrap-job.yaml; do
    k apply -n "$MSG_NS" -f "../nats/$m"
  done

  # FATAL, NOT `|| true`. bootstrap-job.yaml is what creates every JetStream
  # stream; a spine with no streams accepts no publish and binds no consumer, so
  # continuing to step 3 would rebuild a log into nothing. The step above already
  # treats a missing DR Kafka as fatal for the same reason.
  k -n "$MSG_NS" wait --for=condition=complete job/nats-bootstrap --timeout=180s     || { echo "FATAL: nats-bootstrap did not complete — the DR spine has no JetStream streams," >&2
         echo "       so nothing can be published or consumed. Check the Job's logs:" >&2
         echo "       kubectl --context $DR_CTX -n $MSG_NS logs job/nats-bootstrap" >&2
         exit 1; }
fi

# 3) Reconstruct the NATS live spine from the DR Kafka log (DR-01c).
if step rebuild; then
  echo "-- [3/5] rebuild NATS spine from the Kafka log"
  k -n "$MSG_NS" delete job/nats-rebuild --ignore-not-found
  k apply -f nats/rebuild-job.yaml

  # WAIT ON EITHER OUTCOME, NOT JUST THE GOOD ONE.
  #
  # This was `wait --for=condition=complete --timeout=600s`. A Job that FAILS never
  # satisfies that condition, so a fast refusal became ten minutes of silence and
  # then "timed out waiting for condition" — a message naming no cause, during a
  # region failover, with the actual reason buried in a log nobody had reached yet.
  #
  # That mattered little when the rebuild could only exit 0. It matters now: the
  # rebuild REFUSES on a Kafka topic that does not exist (#93), which is the
  # likeliest real failure here — a tenant onboarded without its topics — and it is
  # precisely the answer the operator needs in the first minute, not the tenth.
  deadline=$(( $(date +%s) + 600 ))
  while :; do
    done_st=$(k -n "$MSG_NS" get job/nats-rebuild \
      -o jsonpath='{.status.conditions[?(@.type=="Complete")].status}' 2>/dev/null || echo "")
    fail_st=$(k -n "$MSG_NS" get job/nats-rebuild \
      -o jsonpath='{.status.conditions[?(@.type=="Failed")].status}' 2>/dev/null || echo "")
    [ "$done_st" = "True" ] && break
    if [ "$fail_st" = "True" ]; then
      echo "FATAL: nats-rebuild FAILED — the spine was not reconstructed. Full log:"
      k -n "$MSG_NS" logs job/nats-rebuild --tail=100 || true
      echo "FATAL: a 'missing topic' refusal means the topic was never provisioned for that"
      echo "       tenant (infra/kafka/tenancy.yaml). Do NOT proceed to step 4: the services"
      echo "       would come up against a spine holding none of that tenant's history."
      exit 1
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "FATAL: nats-rebuild neither completed nor failed within 600s — it is stuck, which is"
      echo "       a different fault from a refusal. Check the pod, not the topic list:"
      k -n "$MSG_NS" logs job/nats-rebuild --tail=100 || true
      exit 1
    fi
    sleep 5
  done
  k -n "$MSG_NS" logs job/nats-rebuild --tail=5
fi

# 4) Re-bootstrap services in DR: they read the promoted DSN from their CSI secret
#    (the DR overlay points it at the promoted cluster) and consume the rebuilt
#    spine. Scale up + wait for readiness.
if step services; then
  echo "-- [4/5] start services"
  # SCALE BY SELECTOR, NEVER `--all` (#149).
  #
  # `scale deploy --all --replicas=2` used to stand here, and it overrode every
  # manifest's own replica pin. Eight Deployments in this namespace pin
  # `replicas: 1` as a CORRECTNESS bound, not a cost one, and each failure it
  # causes OUTLIVES the scale-down that "fixes" it:
  #
  #   venue-binance / venue-okx  two pods sign with the SAME exchange API key, so
  #                              the request weight doubles and the account can be
  #                              BANNED — mid-incident, on the money path.
  #   archiver                   single writer to the durable log of record; a
  #                              second pod can INVERT per-key order in the log
  #                              every later rebuild and audit reads.
  #   market-ingest / tv-sync    each folds a partitioned stream; two pods fold two
  #                              competing books, so BOTH replicas' state is wrong.
  #   compliance                 same fold, on the evidence chain.
  #   lake-sink                  holds a ReadWriteOnce PVC; pod two cannot mount it
  #                              and never schedules — the only one that surfaces.
  #   postgres (dev rig)         two emptyDir replicas are two unrelated databases
  #                              behind one Service name.
  #
  # A banned key, an inverted log and a corrupted book are not recovered by scaling
  # back to 1. So "we'd have noticed and fixed it" is not a defence: by the time it
  # is visible the damage is already durable.
  #
  # The label `kanz.io/singleton: "true"` on the Deployment's own metadata is the
  # SINGLE copy of that fact — it lives in the manifest, beside the pin it
  # describes, so a NEW singleton service is protected the day it is added rather
  # than the day someone remembers to edit this script. A hardcoded list here would
  # be the second copy, and the second copy is the one that drifts.
  # test/arch/dr_singleton_replicas_test.go fails if a Deployment pins replicas: 1
  # without the label, if the label sits on a Deployment that does not pin 1, or if
  # an unqualified `scale deploy --all` returns to this file.
  #
  # Both lines are idempotent WITH RESPECT TO INTENT: re-running a partial failover
  # now restores each Deployment to the count its manifest asks for, where the old
  # blanket scale re-inflated to 2 anything an operator had manually corrected to 1.
  #
  # UNDER GITOPS THIS SCALING IS USUALLY REDUNDANT — AND THAT IS NOT A REASON TO
  # LEAVE IT WRONG. infra/gitops/applicationset.yaml syncs kanz/infra/deploy with
  # `automated: {prune: true, selfHeal: true}`, and spec.replicas is part of that
  # desired state, so on a cluster Argo CD is still reconciling this drift is
  # reverted within a reconcile interval and the Deployments were already at their
  # manifest counts to begin with. The drift window is exactly long enough to burn
  # an exchange weight budget or interleave a log write, and Argo undoing the pod
  # count does not undo either. The scaling stays because the case it exists for is
  # the one where reconciliation is NOT running — Argo CD in the failed region,
  # or no reconciler pointed at the DR cluster at all.
  #
  # THAT SECOND CASE IS NOW THE ONLY CASE, and this file no longer has to hedge
  # about it. This comment used to read "the `dr` destination never pointed at a
  # real cluster (both env entries in that ApplicationSet still resolve to
  # kubernetes.default.svc)" — true when written, and #153 acted on it: the `dr`
  # entry was a duplicate Application managing the PRIMARY, so it was removed
  # rather than left asserting a topology that does not exist. There is one env
  # entry now, and nothing whatsoever reconciles a DR region.
  #
  # So the scaling below is not "usually redundant, kept for an edge case". It is
  # the only thing that sets these replica counts during a cutover, and it stays
  # until a DR cluster (#106) and an Argo CD inside it both exist. When they do,
  # the honest form of this step becomes `argocd app sync <the DR Application>` /
  # `kubectl apply -f`, taking the replica count from the manifest by
  # construction rather than by selector. Do not write that command here before
  # the Application it names exists — the previous version of this comment named
  # `kanz-dr-workloads`, which by then was already an Application that only
  # duplicated the primary, and #153 deleted it.
  k -n "$SVC_NS" scale deploy -l '!kanz.io/singleton' --replicas=2
  k -n "$SVC_NS" scale deploy -l 'kanz.io/singleton'  --replicas=1
  # ROLLOUTS TOO (#629). `deploy` resolves to deployments.apps and matches no
  # Rollout, so risk-engine — the service that computes the fund's risk — was
  # never scaled up by this step at all. Argo Rollouts serves the scale
  # subresource, so `kubectl scale` works on it without the plugin.
  k -n "$SVC_NS" scale rollout -l '!kanz.io/singleton' --replicas=2 2>/dev/null || true
  k -n "$SVC_NS" scale rollout -l 'kanz.io/singleton'  --replicas=1 2>/dev/null || true
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
  # `tv-sync` and the venue adapters are singletons (`replicas: 1`), and the scale
  # above now respects that pin instead of overriding it (#149) — so waiting on
  # them here is a wait for ONE ready pod, which is what their manifests define as
  # healthy. `rollout status` is satisfied by the manifest's own count, so nothing
  # in this list needs to know which entries are singletons.
  # risk-engine IS NOT IN THIS LIST, AND THAT IS THE FIX (#629). It is a
  # kind: Rollout (infra/deploy/risk-engine-rollout.yaml) and there is no
  # Deployment by that name anywhere, so `rollout status deploy/risk-engine`
  # returned `deployments.apps "risk-engine" not found` on every run, forever —
  # and `|| true` erased it. The service that computes the fund's risk was the
  # one entry in this list whose check COULD NEVER PASS, and the script was
  # written so that read identically to a pass.
  notready=""
  for d in api-gateway accounting alternatives wealth datamaster oms venue-binance venue-okx regulatory tv-sync market-data; do
    k -n "$SVC_NS" rollout status deploy/"$d" --timeout=300s || notready="$notready deploy/$d"
  done
  for r in risk-engine; do
    wait_rollout_ready "$r" || notready="$notready rollout/$r"
  done

  # A FAILURE IS PRINTED NOW. The comment above argues for tolerating a MISSING
  # workload so a partial topology does not wedge the failover, and that still
  # holds — this does not abort. What it no longer does is stay silent: the same
  # comment already admitted "a name missing from this list looks exactly like a
  # name in it that failed: nothing is printed either way", and that is how a
  # permanently-failing entry survived. Tolerating is not the same as hiding.
  if [ -n "$notready" ]; then
    echo "WARNING: not Ready within the wait:$notready" >&2
    echo "         The region is promoted and traffic will shift in step 5 regardless." >&2
    echo "         Check these before declaring the cutover complete." >&2
  fi
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
