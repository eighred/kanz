#!/usr/bin/env bash
# Invoked only by the exact-main operator wrapper. No credential values or Pod
# logs are read. Rollback restores Kubernetes configuration, never SQL history.
set -Eeuo pipefail
readonly expected_cluster_uid=86ea9839-5806-461d-b2d1-afd1ff5d87ff
readonly migration_image=012619468098.dkr.ecr.ap-northeast-1.amazonaws.com/kanz-migrate@sha256:147db5e4edbb6151f44f1b2c799d14d44437298dc00e19d8ad983d54ab333a24
readonly pg_image=ghcr.io/cloudnative-pg/postgresql@sha256:533e45e0a00c156bdab7c4d9a84fffa87259594725a3909c898e8645727d9555
k() { k3s kubectl --request-timeout=20s "$@"; }
uid_ok() { [[ "$1" =~ ^[0-9a-f-]{36}$ ]]; }
terminal_job() {
    jq -e '(.status.active // 0)==0 and any(.status.conditions[]?; .type=="Complete" and .status=="True")' "$1" >/dev/null || return 1
    local uid
    uid=$(jq -er '.metadata.uid' "$1") || return 1
    uid_ok "$uid" || return 1
    k -n kanz-data get pods -l "batch.kubernetes.io/controller-uid=${uid}" -o json |
        jq -e 'all(.items[]; .status.phase=="Succeeded" or .status.phase=="Failed")' >/dev/null
}
delete_owned() {
    local kind=$1 name=$2 uid=$3 path
    uid_ok "$uid" || return 1
    case "$kind" in
      jobs) path="/apis/batch/v1/namespaces/kanz-data/jobs/${name}" ;;
      secretproviderclasses) path="/apis/secrets-store.csi.x-k8s.io/v1/namespaces/kanz-data/secretproviderclasses/${name}" ;;
      *) return 1 ;;
    esac
    jq -cn --arg uid "$uid" '{apiVersion:"v1",kind:"DeleteOptions",propagationPolicy:"Foreground",preconditions:{uid:$uid}}' |
        k delete --raw "$path" -f - >/dev/null || return 1
    k -n kanz-data wait --for=delete "$kind/$name" --timeout=60s >/dev/null
}
work=$(mktemp -d /var/tmp/kanz-tv-prepare.XXXXXXXX)
chmod 700 "$work"
run_id=$(cat /proc/sys/kernel/random/uuid)
uid_ok "$run_id"
changed=0; success=0; cleanup_ok=1
cleanup() {
    local rc=$? name uid current
    trap - EXIT
    if [[ "$changed" = 1 && "$success" != 1 ]]; then
        # Only Jobs created by this invocation can be stopped on failure. Never
        # delete/recreate a preexisting Job or undo provisioned database state.
        for name in tv-sync-migration tv-sync-provisioner; do
            if [[ ! -s "$work/before-$name" ]]; then
                current=$(k -n kanz-data get "job/$name" --ignore-not-found=true -o json) || { cleanup_ok=0; continue; }
                if [[ -n "$current" ]]; then
                    uid=$(jq -er --arg owner "$run_id" 'select(.metadata.annotations["kanz.io/preparation-owner"]==$owner)|.metadata.uid' <<<"$current") || { cleanup_ok=0; continue; }
                    if [[ -s "$work/new-$name" && "$uid" != "$(cat "$work/new-$name")" ]]; then cleanup_ok=0; continue; fi
                    delete_owned jobs "$name" "$uid" || cleanup_ok=0
                fi
            fi
        done
        if [[ "$cleanup_ok" = 1 && -s "$work/cm-before" ]]; then
            current=$(k -n kanz-data get configmap/postgres-provisioner -o json) || cleanup_ok=0
            if [[ "$cleanup_ok" = 1 ]]; then
                jq -e --slurpfile before "$work/cm-before" --slurpfile desired "$work/desired" '.metadata.uid==$before[0].metadata.uid and (.data==($desired[0].items[]|select(.kind=="ConfigMap")|.data) or .data==$before[0].data)' <<<"$current" >/dev/null || cleanup_ok=0
            fi
            if [[ "$cleanup_ok" = 1 ]]; then
                jq --argjson current "$current" '{apiVersion:"v1",kind:"ConfigMap",metadata:($current.metadata|del(.managedFields)),data:.data}' "$work/cm-before" |
                    k replace -f - >/dev/null || cleanup_ok=0
            fi
        fi
        if [[ ! -s "$work/spc-before" && "$cleanup_ok" = 1 ]]; then
            current=$(k -n kanz-data get secretproviderclass/tv-sync-role-passwords --ignore-not-found=true -o json) || cleanup_ok=0
            if [[ "$cleanup_ok" = 1 && -n "$current" ]]; then
                uid=$(jq -er --arg owner "$run_id" 'select(.metadata.annotations["kanz.io/preparation-owner"]==$owner)|.metadata.uid' <<<"$current") || cleanup_ok=0
                if [[ "$cleanup_ok" = 1 ]]; then delete_owned secretproviderclasses tv-sync-role-passwords "$uid" || cleanup_ok=0; fi
            fi
        fi
        if [[ "$cleanup_ok" = 1 ]]; then echo preparation-rollback-configuration-restored; else echo preparation-rollback-unverified >&2; fi
    fi
    rm -f "$payload" "$manifest"
    rm -rf -- "$work"
    [[ "$cleanup_ok" = 1 ]] || exit 1
    exit "$rc"
}
trap cleanup EXIT
base64 -d "$payload" | gzip -d > "$manifest"
printf '%s  %s\n' "$expected_hash" "$manifest" | sha256sum --check --status
[[ "$(k get namespace/kube-system -o jsonpath='{.metadata.uid}')" = "$expected_cluster_uid" ]]
k -n kanz-data get serviceaccount/postgres-provisioner >/dev/null
k -n kanz-data wait --for=condition=Ready cluster/kanz-testnet-postgres --timeout=30s >/dev/null
for name in postgres-provisioner postgres-migrations; do
    k -n kanz-data get "job/$name" -o json > "$work/base-$name"
    terminal_job "$work/base-$name" || { echo preparation-refused-base-job-not-complete >&2; exit 1; }
done
k create --dry-run=client -f "$manifest" -o json |
    jq -s '{apiVersion:"v1",kind:"List",items:[.[]|if .kind=="List" then .items[] else . end]}' > "$work/desired"
jq -e '[.items[]|(.kind+"/"+.metadata.namespace+"/"+.metadata.name)]|sort==["ConfigMap/kanz-data/postgres-provisioner","Job/kanz-data/tv-sync-migration","Job/kanz-data/tv-sync-provisioner","SecretProviderClass/kanz-data/tv-sync-role-passwords"]' "$work/desired" >/dev/null
k -n kanz-data get configmap/postgres-provisioner -o json > "$work/cm-before"
# Successful absence is distinct from a failed API call. Existing fixed Jobs
# must already be complete; their immutable templates are never replaced here.
for name in tv-sync-provisioner tv-sync-migration; do
    k -n kanz-data get "job/$name" --ignore-not-found=true -o json > "$work/before-$name"
    if [[ -s "$work/before-$name" ]]; then terminal_job "$work/before-$name" || { echo preparation-refused-existing-job-not-complete >&2; exit 1; }; fi
done
k -n kanz-data get secretproviderclass/tv-sync-role-passwords --ignore-not-found=true -o json > "$work/spc-before"
k apply --server-side --dry-run=server --field-manager=kanz-tv-prepare -f "$manifest" -o json |
    jq -s '{apiVersion:"v1",kind:"List",items:[.[]|if .kind=="List" then .items[] else . end]}' > "$work/admitted"
for name in tv-sync-provisioner tv-sync-migration; do
    if [[ -s "$work/before-$name" ]]; then
        jq -e --arg name "$name" --slurpfile before "$work/before-$name" '.items[]|select(.kind=="Job" and .metadata.name==$name)|.spec==$before[0].spec' "$work/admitted" >/dev/null
        jq -e --slurpfile before "$work/cm-before" '.items[]|select(.kind=="ConfigMap")|.data==$before[0].data' "$work/desired" >/dev/null
    fi
done
if [[ -s "$work/spc-before" ]]; then
    jq -e --slurpfile before "$work/spc-before" '.items[]|select(.kind=="SecretProviderClass")|.spec==$before[0].spec' "$work/desired" >/dev/null
fi
[[ "$apply" = 1 ]] || { echo preparation-dry-run-verified; exit 0; }
changed=1
# Apply non-Job resources first, then each fixed Job sequentially. Recording UID
# immediately after creation binds both success evidence and failure cleanup.
# Preserve existing metadata and its resourceVersion for a compare-and-swap.
jq --slurpfile before "$work/cm-before" '.items[]|select(.kind=="ConfigMap")|.metadata=($before[0].metadata|del(.managedFields))' "$work/desired" | k replace -f - >/dev/null
if [[ ! -s "$work/spc-before" ]]; then
    jq --arg owner "$run_id" '.items[]|select(.kind=="SecretProviderClass")|.metadata.annotations["kanz.io/preparation-owner"]=$owner' "$work/desired" | k create -f - >/dev/null
fi
for name in tv-sync-provisioner tv-sync-migration; do
    if [[ ! -s "$work/before-$name" ]]; then
        jq --arg name "$name" --arg owner "$run_id" '.items[]|select(.kind=="Job" and .metadata.name==$name)|.metadata.annotations["kanz.io/preparation-owner"]=$owner' "$work/desired" | k create -f - -o json > "$work/created-$name"
        jq -er '.metadata.uid' "$work/created-$name" > "$work/new-$name"
    fi
    # A bounded polling loop avoids treating one short API-watch timeout as a
    # completed migration; any non-complete outcome at the deadline refuses.
    complete=0
    for attempt in $(seq 1 120); do
        k -n kanz-data get "job/$name" -o json > "$work/current-$name"
        if jq -e 'any(.status.conditions[]?; .type=="Failed" and .status=="True")' "$work/current-$name" >/dev/null; then break; fi
        if jq -e 'any(.status.conditions[]?; .type=="Complete" and .status=="True")' "$work/current-$name" >/dev/null; then complete=1; break; fi
        sleep 5
    done
    [[ "$complete" = 1 ]] || { echo preparation-job-not-complete >&2; exit 1; }
    uid=$(jq -er '.metadata.uid' "$work/current-$name"); uid_ok "$uid"
    if [[ -s "$work/new-$name" ]]; then [[ "$uid" = "$(cat "$work/new-$name")" ]]; else [[ "$uid" = "$(jq -er '.metadata.uid' "$work/before-$name")" ]]; fi
    container=provisioner; image=$pg_image
    if [[ "$name" = tv-sync-migration ]]; then container=migrate-tv-sync; image=$migration_image; fi
    jq -e --arg container "$container" --arg image "$image" 'any(.spec.template.spec.containers[]; .name==$container and .image==$image)' "$work/current-$name" >/dev/null
    k -n kanz-data get pods -l "batch.kubernetes.io/controller-uid=$uid" -o json |
        jq -e --arg uid "$uid" --arg name "$name" --arg container "$container" --arg image "$image" '[.items[]|select(any(.metadata.ownerReferences[]?; .kind=="Job" and .name==$name and .uid==$uid and .controller==true))|select(.status.phase=="Succeeded")|select(any(.spec.containers[]; .name==$container and .image==$image))|select(any(.status.containerStatuses[]; .name==$container and .state.terminated.exitCode==0 and (.imageID|endswith("@"+($image|split("@")[1])))))]|length==1' >/dev/null
    echo "preparation-job-verified=$name"
done
success=1
echo tv-sync-database-preparation-verified
