#!/usr/bin/env bash
# Sourced only by the exact-main recovery script. No credentials leave the
# disposable restored cluster; this helper never contacts the source database.

# linux/amd64 child of the existing immutable CNPG 16.10 index (root
# 533e45e0a00c156bdab7c4d9a84fffa87259594725a3909c898e8645727d9555).
readonly TV_PG_IMAGE='ghcr.io/cloudnative-pg/postgresql@sha256:f231cb87520b2fb75561defa271f8cc019d35fca7eec447a0fbe6d39bb1d4214'

tvk() { k --request-timeout=20s "$@"; }

# Older jq builds discard fractional digits while parsing into binary doubles.
# Refuse them before any TV operation; range checks cannot recover lost input.
tv_exact_json_available() { jq -ne 'have_decnum' >/dev/null 2>&1; }

begin_tv_restore_namespace() {
  local present storage_class volumes
  storage_class=$(tvk get storageclass local-path -o json) || die 'drill storage class unavailable'
  jq -e '.provisioner=="rancher.io/local-path" and .reclaimPolicy=="Delete" and (.volumeBindingMode=="WaitForFirstConsumer" or .volumeBindingMode=="Immediate")' <<<"$storage_class" >/dev/null || die 'drill storage reclamation policy refused'
  present=$(tvk get namespace "$DRILL_NS" --ignore-not-found=true -o json) || die 'drill namespace inventory unavailable'
  [[ -z "$present" ]] || die 'TV drill namespace already exists'
  volumes=$(tvk get persistentvolumes -o json) || die 'drill storage inventory unavailable'
  jq -e --arg namespace "$DRILL_NS" 'all(.items[];.spec.claimRef.namespace!=$namespace)' <<<"$volumes" >/dev/null || die 'prior drill storage still exists'
  TV_OWNER_NONCE=$(cat /proc/sys/kernel/random/uuid)
  [[ "$TV_OWNER_NONCE" =~ ^[a-f0-9-]{36}$ ]] || die 'drill owner identity unavailable'
  jq -n --arg ns "$DRILL_NS" --arg owner "$TV_OWNER_NONCE" '{apiVersion:"v1",kind:"Namespace",metadata:{name:$ns,labels:{"kanz.io/purpose":"recovery-drill"},annotations:{"kanz.io/recovery-owner":$owner}}}' | tvk create -f - >/dev/null
  DRILL_UID=$(tvk get namespace "$DRILL_NS" -o json | jq -er --arg owner "$TV_OWNER_NONCE" '.metadata|select(.annotations."kanz.io/recovery-owner"==$owner)|.uid')
  [[ "$DRILL_UID" =~ ^[a-f0-9-]{36}$ ]] || die 'created drill identity unavailable'
}

cleanup_tv_restore_namespace() {
  [[ -n "${TV_OWNER_NONCE:-}" ]] || return 0
  local current uid deadline volumes claims
  current=$(tvk get namespace "$DRILL_NS" --ignore-not-found=true -o json 2>/dev/null) || return 1
  if [[ -z "$current" ]]; then
    volumes=$(tvk get persistentvolumes -o json 2>/dev/null) || return 1
    jq -e --arg namespace "$DRILL_NS" 'all(.items[];.spec.claimRef.namespace!=$namespace)' <<<"$volumes" >/dev/null || return 1
    DRILL_UID=''; TV_OWNER_NONCE=''; return 0
  fi
  uid=$(jq -er .metadata.uid <<<"$current") || return 1
  if [[ -z "$DRILL_UID" ]]; then
    # A create may succeed server-side even when its client response is lost.
    jq -e --arg owner "$TV_OWNER_NONCE" '.metadata.annotations."kanz.io/recovery-owner"==$owner' <<<"$current" >/dev/null || return 1
    DRILL_UID=$uid
  fi
  [[ "$uid" == "$DRILL_UID" && "$uid" =~ ^[a-f0-9-]{36}$ ]] || return 1
  # Namespace ownership is established above. Refuse retained storage and save
  # only public PV identities; never force-delete volumes or finalizers.
  claims=$(tvk -n "$DRILL_NS" get persistentvolumeclaims -o json 2>/dev/null) || return 1
  volumes=$(tvk get persistentvolumes -o json 2>/dev/null) || return 1
  jq -e --arg namespace "$DRILL_NS" --argjson claims "$claims" 'all(.items[]|select(.spec.claimRef.namespace==$namespace); . as $pv | any($claims.items[];.metadata.uid==$pv.spec.claimRef.uid and .metadata.name==$pv.spec.claimRef.name and .spec.volumeName==$pv.metadata.name))' <<<"$volumes" >/dev/null || return 1
  jq -e --arg namespace "$DRILL_NS" 'all(.items[]|select(.spec.claimRef.namespace==$namespace);.spec.persistentVolumeReclaimPolicy=="Delete" and .spec.storageClassName=="local-path" and (.spec.claimRef.uid|type)=="string")' <<<"$volumes" >/dev/null || return 1
  jq -n --arg uid "$uid" '{apiVersion:"v1",kind:"DeleteOptions",preconditions:{uid:$uid}}' | tvk delete --raw "/api/v1/namespaces/$DRILL_NS" -f - >/dev/null 2>&1 || return 1
  deadline=$((SECONDS+120))
  while ((SECONDS<deadline)); do
    current=$(tvk get namespace "$DRILL_NS" --ignore-not-found=true -o json 2>/dev/null) || return 1
    if [[ -z "$current" ]]; then
      # PV object deletion confirms controller cleanup, not physical media erasure.
      volumes=$(tvk get persistentvolumes -o json 2>/dev/null) || return 1
      if jq -e --arg namespace "$DRILL_NS" 'all(.items[];.spec.claimRef.namespace!=$namespace)' <<<"$volumes" >/dev/null; then
        DRILL_UID=''; TV_OWNER_NONCE=''; return 0
      fi
    fi
    if [[ -n "$current" ]]; then [[ "$(jq -r .metadata.uid <<<"$current")" == "$uid" ]] || return 1; fi
    sleep 2
  done
  return 1
}

load_tv_restore_pair() {
  tv_exact_json_available || die 'TV proof requires exact-decimal JSON parsing'
  [[ ${#TV_PAIR_BASE64} -le 16384 && -n "$TV_PAIR_BASE64" ]] || die 'TV release request missing or oversized'
  TV_PAIR=$(printf '%s' "$TV_PAIR_BASE64" | base64 -d) || die 'TV release request encoding invalid'
  jq -e '
    (keys|sort)==["destination_tag","images","release_sha","release_tag","runtime_platform_digest","schema_version","signer_identity","signer_issuer"] and
    (.runtime_platform_digest|test("^sha256:[0-9a-f]{64}$")) and
    .schema_version==1 and (.release_sha|test("^[0-9a-f]{40}$")) and
    (.release_tag|test("^v[0-9]+\\.[0-9]+\\.[0-9]+$")) and
    .destination_tag==("release-"+.release_tag) and
    .signer_identity==("https://github.com/eighred/kanz/.github/workflows/release.yml@refs/tags/"+.release_tag) and
    .signer_issuer=="https://token.actions.githubusercontent.com" and
    (.images|keys|sort)==["kanz-migrate","tv-sync"] and
    all(.images|to_entries[]; . as $member |
      (.value|keys|sort)==["destination","source"] and
      (.value.destination|test("^012619468098\\.dkr\\.ecr\\.ap-northeast-1\\.amazonaws\\.com/"+$member.key+"@sha256:[0-9a-f]{64}$")) and
      .value.source==("ghcr.io/eighred/"+.key+"@"+(.value.destination|split("@")[1])))
  ' <<<"$TV_PAIR" >/dev/null || die 'TV release request shape refused'
  TV_IMAGE=$(jq -r '.images."tv-sync".destination' <<<"$TV_PAIR")
  TV_RUNTIME_IMAGE="${TV_IMAGE%@*}@$(jq -r .runtime_platform_digest <<<"$TV_PAIR")"
  TV_VERSION=$(jq -r .release_tag <<<"$TV_PAIR")
  TV_SOURCE=$(jq -r .release_sha <<<"$TV_PAIR")
}

tv_restore_role_script() {
  cat <<'ROLE_SCRIPT'
set -Eeuo pipefail
umask 077
host=$(cat /run/secrets/postgres/host)
case "$host" in kanz-postgres-drill-rw|kanz-postgres-drill-rw.kanz-recovery-drill|kanz-postgres-drill-rw.kanz-recovery-drill.svc|kanz-postgres-drill-rw.kanz-recovery-drill.svc.cluster.local) ;; *) exit 64 ;; esac
export PGSSLMODE=verify-full PGSSLROOTCERT=/run/postgres-ca/ca.crt
export PGPASSWORD=$(cat /run/secrets/postgres/password)
# Refuse before generating or submitting a password if server-side SQL logging
# could expose it. These settings apply only to the disposable restored cluster.
logging=$(psql --host "$host" --username kanz_restore --dbname tv_sync --no-psqlrc --tuples-only --no-align --set=ON_ERROR_STOP=1 --command "SELECT current_setting('log_statement')='none' AND current_setting('log_min_error_statement')='panic' AND current_setting('log_error_verbosity')='terse' AND current_setting('log_min_duration_statement')='-1' AND current_setting('log_min_duration_sample')='-1' AND current_setting('log_transaction_sample_rate')='0' AND current_setting('log_parameter_max_length')='0' AND current_setting('log_parameter_max_length_on_error')='0' AND current_setting('shared_preload_libraries')='' AND NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='kanz_tv_replay')" 2>/dev/null)
[[ "$logging" == t ]] || exit 64
export KANZ_REPLAY_PASSWORD=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
[[ "$KANZ_REPLAY_PASSWORD" =~ ^[0-9a-f]{64}$ ]] || exit 64
psql --host "$host" --username kanz_restore --dbname tv_sync --no-psqlrc --quiet --set=ON_ERROR_STOP=1 >/dev/null 2>&1 <<'SQL'
\getenv replay_password KANZ_REPLAY_PASSWORD
SELECT format('CREATE ROLE kanz_tv_replay LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION PASSWORD %L', :'replay_password') \gexec
REVOKE ALL ON DATABASE tv_sync FROM PUBLIC;
GRANT CONNECT ON DATABASE tv_sync TO kanz_tv_replay;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO kanz_tv_replay;
GRANT SELECT ON tv_facts,tv_checkpoints TO kanz_tv_replay;
SQL
posture=$(psql --host "$host" --username kanz_restore --dbname tv_sync --no-psqlrc --tuples-only --no-align --set=ON_ERROR_STOP=1 --command "SELECT EXISTS(SELECT 1 FROM pg_roles r WHERE rolname='kanz_tv_replay' AND NOT rolsuper AND NOT rolbypassrls AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolinherit AND NOT rolreplication AND NOT EXISTS(SELECT 1 FROM pg_auth_members WHERE member=r.oid)) AND (SELECT count(*)=2 FROM pg_class WHERE oid IN ('tv_facts'::regclass,'tv_checkpoints'::regclass) AND relrowsecurity AND relforcerowsecurity AND relowner<>(SELECT oid FROM pg_roles WHERE rolname='kanz_tv_replay'))" 2>/dev/null)
[[ "$posture" == t ]] || exit 64
printf 'postgresql://kanz_tv_replay:%s@%s:5432/tv_sync?sslmode=verify-full&sslrootcert=/run/postgres-ca/ca.crt' "$KANZ_REPLAY_PASSWORD" "$host" > /run/replay/database-url
chgrp 65532 /run/replay/database-url
chmod 0440 /run/replay/database-url
unset PGPASSWORD KANZ_REPLAY_PASSWORD
ROLE_SCRIPT
}

tv_restore_job_manifest() {
  jq -n --arg namespace "$DRILL_NS" --arg owner "$DRILL_UID" --arg pg "$TV_PG_IMAGE" --arg image "$TV_RUNTIME_IMAGE" --arg version "$TV_VERSION" --arg init "$(tv_restore_role_script)" '
  {apiVersion:"batch/v1",kind:"Job",metadata:{name:"tv-sync-replay",namespace:$namespace,annotations:{"kanz.io/recovery-owner":$owner}},spec:{backoffLimit:0,activeDeadlineSeconds:240,template:{metadata:{labels:{app:"tv-sync-replay"}},spec:{restartPolicy:"Never",automountServiceAccountToken:false,nodeSelector:{"kubernetes.io/os":"linux","kubernetes.io/arch":"amd64"},securityContext:{runAsNonRoot:true,runAsUser:65532,runAsGroup:65532,fsGroup:65532,seccompProfile:{type:"RuntimeDefault"}},initContainers:[{name:"runtime-role",image:$pg,command:["bash","-ceu",$init],securityContext:{runAsUser:26,allowPrivilegeEscalation:false,readOnlyRootFilesystem:true,capabilities:{drop:["ALL"]}},resources:{requests:{cpu:"25m",memory:"32Mi"},limits:{cpu:"250m",memory:"128Mi"}},volumeMounts:[{name:"owner-credentials",mountPath:"/run/secrets/postgres",readOnly:true},{name:"runtime-dsn",mountPath:"/run/replay"},{name:"postgres-ca",mountPath:"/run/postgres-ca",readOnly:true}]}],containers:[{name:"verify",image:$image,command:["/tv-sync-replay"],env:[{name:"TV_SYNC_TENANT",value:"__system__"},{name:"TV_SYNC_RETENTION",value:"168h"},{name:"TV_SYNC_REPLAY_EXPECTED_VERSION",value:$version},{name:"TV_SYNC_DATABASE_URL_FILE",value:"/run/replay/database-url"}],securityContext:{allowPrivilegeEscalation:false,readOnlyRootFilesystem:true,capabilities:{drop:["ALL"]}},resources:{requests:{cpu:"100m",memory:"128Mi"},limits:{cpu:"1",memory:"1Gi"}},volumeMounts:[{name:"runtime-dsn",mountPath:"/run/replay",readOnly:true},{name:"postgres-ca",mountPath:"/run/postgres-ca",readOnly:true}]}],volumes:[{name:"owner-credentials",secret:{secretName:"kanz-postgres-drill-app",defaultMode:288}},{name:"runtime-dsn",emptyDir:{medium:"Memory",sizeLimit:"1Mi"}},{name:"postgres-ca",secret:{secretName:"kanz-postgres-drill-ca",defaultMode:288}}]}}}}'
}

# jq uses IEEE-754 numbers: refuse counters outside its exact integer domain.
# UTC timestamp freshness is bound to this invocation, not a reusable receipt.
validate_tv_restore_result() {
  local proof=$1 tenant=$2 started=$3 finished=$4
  tv_exact_json_available || return 1
  jq -e --arg version "$TV_VERSION" --arg tenant "$tenant" --argjson started "$started" --argjson finished "$finished" '
    def integer: type=="number" and (tostring|test("^(0|[1-9][0-9]*)$")) and isfinite and floor==. and .>=0 and .<=9007199254740991;
    (.checked_at|sub("\\.[0-9]+Z$";"Z")) as $stamp |
    ($stamp|fromdateiso8601) as $checked |
    ($checked|strftime("%Y-%m-%dT%H:%M:%SZ"))==$stamp and
    (keys|sort)==["checked_at","checkpoint_seq","fact_count","input_sha256","retention_seconds","schema_version","snapshot_sha256","source_version","state_sha256","status","tenant_sha256","watermark"] and
    .schema_version==1 and .status=="VERIFIED" and .source_version==$version and .tenant_sha256==$tenant and
    all([.schema_version,.retention_seconds,.fact_count,.checkpoint_seq,.watermark][];integer) and
    .retention_seconds==604800 and .fact_count>0 and .fact_count<=100000 and .checkpoint_seq>0 and .watermark>=.checkpoint_seq and
    all([.input_sha256,.snapshot_sha256,.state_sha256][];type=="string" and test("^[0-9a-f]{64}$")) and
    (.checked_at|type)=="string" and (.checked_at|test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]{1,9})?Z$")) and
    $checked >= $started and $checked <= $finished and ($finished-$checked)<=300
  ' <<<"$proof" >/dev/null
}

qualify_tv_restored_snapshot() {
  local archive_sha=$1 cluster_uid job job_uid pods pod_name proof image_id deadline current job_started
  job_started=$(date -u +%s)
  cluster_uid=$(tvk -n "$DRILL_NS" get cluster kanz-postgres-drill -o jsonpath='{.metadata.uid}')
  [[ "$cluster_uid" =~ ^[a-f0-9-]{36}$ && "$DRILL_UID" =~ ^[a-f0-9-]{36}$ ]] || die 'restored cluster ownership unknown'
  tv_restore_job_manifest | tvk create -f - >/dev/null
  job=$(tvk -n "$DRILL_NS" get job tv-sync-replay -o json)
  job_uid=$(jq -er --arg owner "$DRILL_UID" '.metadata|select(.annotations."kanz.io/recovery-owner"==$owner)|.uid' <<<"$job")
  [[ "$job_uid" =~ ^[a-f0-9-]{36}$ ]] || die 'verifier Job identity unknown'
  deadline=$((SECONDS+300))
  while ((SECONDS<deadline)); do
    job=$(tvk -n "$DRILL_NS" get job tv-sync-replay -o json)
    jq -e --arg uid "$job_uid" '.metadata.uid==$uid' <<<"$job" >/dev/null || die 'verifier Job replaced'
    if jq -e 'any(.status.conditions[]?;.type=="Failed" and .status=="True")' <<<"$job" >/dev/null; then die 'restored replay refused'; fi
    if jq -e 'any(.status.conditions[]?;.type=="Complete" and .status=="True")' <<<"$job" >/dev/null; then break; fi
    sleep 2
  done
  jq -e --arg image "$TV_RUNTIME_IMAGE" 'any(.status.conditions[]?;.type=="Complete" and .status=="True") and .spec.template.spec.containers[0].image==$image' <<<"$job" >/dev/null || die 'restored replay did not complete'
  pods=$(tvk -n "$DRILL_NS" get pods -l "controller-uid=$job_uid" -o json)
  jq -e --arg uid "$job_uid" --arg image "$TV_RUNTIME_IMAGE" --arg pg "$TV_PG_IMAGE" '
    (.items|length)==1 and all(.items[];
      any(.metadata.ownerReferences[]?;.kind=="Job" and .name=="tv-sync-replay" and .uid==$uid and .controller==true) and
      .status.phase=="Succeeded" and .spec.containers[0].image==$image and .spec.initContainers[0].image==$pg and
      (.status.containerStatuses|length)==1 and (.status.initContainerStatuses|length)==1 and
      .status.containerStatuses[0].name=="verify" and .status.containerStatuses[0].state.terminated.exitCode==0 and
      .status.initContainerStatuses[0].state.terminated.exitCode==0 and
      (.status.containerStatuses[0].imageID|endswith("@"+($image|split("@")[1]))) and
      (.status.initContainerStatuses[0].imageID|endswith("@"+($pg|split("@")[1]))))
  ' <<<"$pods" >/dev/null || die 'restored replay loaded-image evidence refused'
  pod_name=$(jq -r '.items[0].metadata.name' <<<"$pods")
  image_id=$(jq -r '.items[0].status.containerStatuses[0].imageID' <<<"$pods")
  # Only the stamped verifier logs are read. Init/SQL errors are never exported.
  proof=$(tvk -n "$DRILL_NS" logs "$pod_name" -c verify --limit-bytes=8192)
  local tenant_sha
  tenant_sha=$(printf '%s' '__system__' | sha256sum | awk '{print $1}')
  validate_tv_restore_result "$proof" "$tenant_sha" "$job_started" "$(date -u +%s)" || die 'restored replay result refused'
  current=$(tvk -n "$DRILL_NS" get cluster kanz-postgres-drill -o jsonpath='{.metadata.uid}')
  [[ "$current" == "$cluster_uid" ]] || die 'restored database cluster replaced'
  current=$(tvk get namespace "$DRILL_NS" -o jsonpath='{.metadata.uid}')
  [[ "$current" == "$DRILL_UID" ]] || die 'restored namespace replaced'
  jq -n --arg archive "$archive_sha" --arg cluster "$cluster_uid" --arg namespace "$DRILL_UID" --arg image "$TV_IMAGE" --arg runtime "$TV_RUNTIME_IMAGE" --arg loaded "$image_id" --arg source "$TV_SOURCE" --arg version "$TV_VERSION" --arg migration "$(jq -r '.images."kanz-migrate".destination' <<<"$TV_PAIR")" --argjson proof "$proof" '{status:"RESTORED_REPLAY_VERIFIED",archive_sha256:$archive,restored_cluster_uid:$cluster,namespace_uid:$namespace,source_release_sha:$source,source_version:$version,verifier_image:$image,runtime_platform_image:$runtime,loaded_image:$loaded,paired_migration_image:$migration,snapshot:$proof}'
}
