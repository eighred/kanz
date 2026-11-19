#!/usr/bin/env bash
# Exercise the ordinary restore's namespace functions against a private API.
set -euo pipefail
source "$(dirname "$0")/../testnet-data-plane-recovery.sh" restore
work=$(mktemp -d)
cluster="dr-owner-$(cat /proc/sys/kernel/random/uuid | cut -c1-12)"
config="$work/kubeconfig"
cleanup() {
  local rc=$?
  kind delete cluster --name "$cluster" --kubeconfig "$config" || rc=1
  rm -rf -- "$work"
  exit "$rc"
}
existing=$(kind get clusters)
if grep -Fxq "$cluster" <<<"$existing"; then rmdir "$work"; exit 1; fi
trap cleanup EXIT
kind create cluster --name "$cluster" --kubeconfig "$config" --wait 120s \
  --image 'kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed'
k() { kubectl --kubeconfig "$config" --context "kind-$cluster" "$@"; }
k create -f - >/dev/null <<'EOF'
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: local-path}
provisioner: rancher.io/local-path
reclaimPolicy: Delete
volumeBindingMode: WaitForFirstConsumer
EOF
k create namespace "$DRILL_NS"
foreign=$(k get ns "$DRILL_NS" -o jsonpath='{.metadata.uid}')
if (begin_tv_restore_namespace); then echo 'Existing namespace accepted' >&2; exit 1; fi
cleanup_tv_restore_namespace
[[ "$(k get ns "$DRILL_NS" -o jsonpath='{.metadata.uid}')" == "$foreign" ]]
# Remove only the fixture-created control, with the same UID precondition
# required of the operator cleanup.
jq -n --arg uid "$foreign" '{kind:"DeleteOptions",apiVersion:"v1",preconditions:{uid:$uid}}' |
  k delete --raw "/api/v1/namespaces/$DRILL_NS" -f - >/dev/null
k wait --for=delete "namespace/$DRILL_NS" --timeout=60s
begin_tv_restore_namespace
[[ "$DRILL_UID" == "$(k get ns "$DRILL_NS" -o jsonpath='{.metadata.uid}')" ]]
if (DRILL_UID="$foreign"; cleanup_tv_restore_namespace); then echo 'Stale UID accepted' >&2; exit 1; fi
[[ -n "$(k get ns "$DRILL_NS" -o name)" ]]
cleanup_tv_restore_namespace
[[ -z "$DRILL_UID" && -z "$TV_OWNER_NONCE" && -z "$(k get ns "$DRILL_NS" --ignore-not-found -o name)" ]]

# The same isolated API provides real NotFound, empty and non-terminal lists.
check_writer_namespace kanz-services
k create namespace kanz-services >/dev/null
check_writer_namespace kanz-services
k create -n kanz-services -f - >/dev/null <<'EOF'
apiVersion: v1
kind: Pod
metadata: {name: writer}
spec:
  schedulingGates: [{name: kanz.io/fixture}]
  containers: [{name: writer, image: registry.k8s.io/pause:3.10, imagePullPolicy: Never}]
EOF
if (check_writer_namespace kanz-services); then echo 'Pending writer accepted' >&2; exit 1; fi
echo 'Verified writer namespace absent, empty and non-terminal states with the private API'

k create namespace "$MSG_NS" >/dev/null
k -n "$MSG_NS" create -f - >/dev/null <<'EOF'
apiVersion: batch/v1
kind: Job
metadata: {name: nats-bootstrap}
spec:
  suspend: true
  template:
    metadata: {labels: {app: nats-bootstrap}}
    spec:
      schedulingGates: [{name: kanz.io/fixture}]
      restartPolicy: Never
      containers: [{name: bootstrap, image: registry.k8s.io/pause:3.10, imagePullPolicy: Never}]
EOF
# Job admission copies the template labels onto this Job, including the
# generated selector that caused the controller to adopt the old runner.
k -n "$MSG_NS" get job nats-bootstrap -o json | jq -e '
  .metadata.labels.app=="nats-bootstrap" and
  (.metadata.labels."batch.kubernetes.io/controller-uid"==.metadata.uid) and
  (.spec.selector.matchLabels."batch.kubernetes.io/controller-uid"==.metadata.uid)' >/dev/null
create_backup_runner
first_uid=$RUNNER_UID
k -n "$MSG_NS" get pod "$NATS_RUNNER" -o json | jq -e --arg uid "$first_uid" --arg owner "$RUNNER_NONCE" '
  .metadata.uid==$uid and .metadata.annotations."kanz.io/recovery-owner"==$owner and
  (.metadata.ownerReferences|length)==0 and .metadata.labels=={app:"nats-bootstrap"}' >/dev/null
if (create_backup_runner); then echo 'Replaced an existing backup runner' >&2; exit 1; fi
if (RUNNER_NONCE=foreign; cleanup_backup_runner); then echo 'Foreign cleanup succeeded' >&2; exit 1; fi
[[ "$(k -n "$MSG_NS" get pod "$NATS_RUNNER" -o jsonpath='{.metadata.uid}')" == "$first_uid" ]]
RUNNER_UID='' # Simulate loss of the create response.
cleanup_backup_runner
create_backup_runner
[[ -n "$RUNNER_UID" && "$RUNNER_UID" != "$first_uid" ]]
if (RUNNER_UID=$first_uid; cleanup_backup_runner); then echo 'Stale UID accepted' >&2; exit 1; fi
[[ "$(k -n "$MSG_NS" get pod "$NATS_RUNNER" -o jsonpath='{.metadata.uid}')" == "$RUNNER_UID" ]]
cleanup_backup_runner
[[ -z "$(k -n "$MSG_NS" get pod "$NATS_RUNNER" --ignore-not-found -o name)" ]]
echo 'Verified backup runner ownership and cleanup with the private API'
