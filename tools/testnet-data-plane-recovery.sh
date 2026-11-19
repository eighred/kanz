#!/usr/bin/env bash
set -Eeuo pipefail

# This script runs only on the SSM-controlled K3s host. AWS credentials remain
# in the host instance profile; database and Redis credentials remain inside
# their CSI/Secret-mounted containers. Neither crosses kubectl stdout.

readonly OPERATION=${1:-}
readonly RECOVERY_BUCKET=${2:-}
readonly RECOVERY_KMS_KEY_ARN=${3:-}
readonly RELEASE_COMMIT=${4:-unknown}
readonly TV_MODE=${5:-off}
readonly TV_PAIR_BASE64=${6:-}
readonly BACKUP_SELECTION=${7:-}
readonly RECOVERY_REGION=ap-northeast-3
readonly DATA_NS=kanz-data
readonly MSG_NS=kanz-messaging
readonly DRILL_NS=kanz-recovery-drill
readonly PG_HOST=kanz-testnet-postgres-rw.kanz-data.svc.cluster.local
DATABASES=(accounting audit identity oms regulatory risk_engine venue_binance venue_okx)
readonly PG_IMAGE='ghcr.io/cloudnative-pg/postgresql:16.10-standard-bookworm@sha256:533e45e0a00c156bdab7c4d9a84fffa87259594725a3909c898e8645727d9555'
readonly REDIS_IMAGE='redis@sha256:bb186d083732f669da90be8b0f975a37812b15e913465bb14d845db72a4e3e08'
readonly NATS_IMAGE='nats@sha256:d4ac35882ac65aff236cd65b9d3fa4d24332c681e1a85f94eedccd3cdd65b1da'
readonly NATS_BOX_IMAGE='natsio/nats-box@sha256:ffce8bd103383f179f8c7f11cf645726acf5d17280706c530c3b342dbe16334c'
RECOVERY_WORK=''
RECOVERY_ARCHIVE=''
RUNNER_UID=''
RUNNER_NONCE=''
readonly NATS_RUNNER=nats-backup-runner
DRILL_UID=''
TV_OWNER_NONCE=''
TV_PAIR=''
TV_IMAGE=''
TV_VERSION=''
TV_SOURCE=''

k() { k3s kubectl "$@"; }
die() { printf 'recovery-refused: %s\n' "$*" >&2; exit 2; }

if (( $# > 7 )) || { (( $# == 7 )) && [[ "$OPERATION" != restore || -z "$BACKUP_SELECTION" ]]; }; then
  die 'a nonempty backup selection is valid only for a restore'
fi

if [[ "$OPERATION" == restore || "$TV_MODE" == qualify ]]; then
  source "$(dirname "${BASH_SOURCE[0]}")/tv-sync-restored-proof.sh"
fi
case "$TV_MODE" in
  off) [[ -z "$TV_PAIR_BASE64" ]] || die 'TV release inputs require explicit qualification' ;;
  qualify)
    load_tv_restore_pair
    DATABASES+=(tv_sync)
    ;;
  *) die 'unsupported TV recovery mode' ;;
esac
readonly -a DATABASES

require_boundary() {
  [[ "$RECOVERY_BUCKET" == kanz-testnet-tokyo-recovery-* ]] || die 'unexpected recovery bucket'
  [[ "$RECOVERY_KMS_KEY_ARN" == arn:aws:kms:ap-northeast-3:*:key/* ]] || die 'unexpected recovery KMS key'
  aws sts get-caller-identity --query Account --output text | grep -qx '012619468098' || die 'wrong AWS account'
  k get namespace "$DATA_NS" >/dev/null
  k get namespace "$MSG_NS" >/dev/null
  if [[ "$TV_MODE" == qualify ]]; then
    [[ "$(tvk get namespace kube-system -o jsonpath='{.metadata.uid}')" == 86ea9839-5806-461d-b2d1-afd1ff5d87ff ]] || die 'TV recovery cluster identity mismatch'
  fi
}

check_writer_namespace() {
  local ns=$1 namespace pods count
  namespace=$(k get namespace "$ns" --ignore-not-found -o json) || die "cannot establish writer namespace $ns"
  [[ -n "$namespace" ]] || return 0
  jq -se --arg ns "$ns" 'length==1 and (.[0] | .kind=="Namespace" and .metadata.name==$ns and
    (.metadata.uid|type=="string" and length>0) and .status.phase=="Active")' \
    <<<"$namespace" >/dev/null || die "incomplete writer namespace $ns"
  pods=$(k -n "$ns" get pods -o json) || die "cannot read writer pods in $ns"
  count=$(jq -ser --arg ns "$ns" '
    if length==1 then .[0] else error("expected one pod list") end |
    if ((.kind=="List" or .kind=="PodList") and (.items|type=="array") and
        ((.metadata.continue // "")=="") and all(.items[];
          .kind=="Pod" and .metadata.namespace==$ns and
          (.metadata.name|type=="string" and length>0) and
          (.metadata.uid|type=="string" and length>0) and
          (.status.phase|type=="string" and length>0)))
    then [.items[] | select(.status.phase!="Succeeded" and .status.phase!="Failed")] | length
    else error("incomplete pod inventory") end' <<<"$pods") || die "incomplete writer pods in $ns"
  [[ "$count" == 0 ]] || die "$ns has $count non-terminal pod(s); quiesce every writer before a cross-database backup"
}

refuse_active_writers() {
  local ns
  for ns in kanz-services kanz-venues; do check_writer_namespace "$ns"; done
  [[ "$(k -n "$DATA_NS" get job postgres-provisioner -o jsonpath='{.status.succeeded}')" == 1 ]] || die 'PostgreSQL roles are not provisioned'
  [[ "$(k -n "$DATA_NS" get job postgres-migrations -o jsonpath='{.status.succeeded}')" == 1 ]] || die 'PostgreSQL migrations are not complete'
  [[ "$(k -n "$MSG_NS" get job nats-bootstrap -o jsonpath='{.status.succeeded}')" == 1 ]] || die 'NATS topology is not provisioned'
}

create_backup_runner() {
  local job_json created
  job_json=$(k -n "$MSG_NS" get job nats-bootstrap -o json)
  RUNNER_NONCE=$(cat /proc/sys/kernel/random/uuid)
  created=$(printf '%s' "$job_json" | jq \
    --arg pod "$NATS_RUNNER" --arg container bootstrap --arg owner "$RUNNER_NONCE" '
      .kind="Pod" | .apiVersion="v1" | .metadata.name=$pod |
      del(.metadata.uid,.metadata.resourceVersion,.metadata.creationTimestamp,
          .metadata.generation,.metadata.annotations,.metadata.ownerReferences,.status) |
      .metadata.annotations={"kanz.io/recovery-owner":$owner} |
      .spec=.spec.template.spec | del(.spec.template) |
      .spec.restartPolicy="Never" |
      (.spec.containers[] | select(.name==$container) | .command)=["sleep","1800"] |
      (.spec.containers[] | select(.name==$container) | .args)=[]' |
    k -n "$MSG_NS" create -f - -o json) || die 'could not create the backup runner'
  RUNNER_UID=$(backup_runner_uid <<<"$created") || die 'invalid created backup runner identity'
}

backup_runner_uid() {
  jq -er --arg ns "$MSG_NS" --arg name "$NATS_RUNNER" --arg owner "$RUNNER_NONCE" '
    select(.kind=="Pod" and .metadata.namespace==$ns and .metadata.name==$name and
           .metadata.annotations."kanz.io/recovery-owner"==$owner) |
    .metadata.uid | strings | select(test("^[0-9a-f-]{36}$"))'
}

cleanup_backup_runner() {
  [[ -n "$RUNNER_NONCE" ]] || return 0
  local current uid
  current=$(k -n "$MSG_NS" get pod "$NATS_RUNNER" --ignore-not-found -o json) || return 1
  if [[ -n "$current" ]]; then
    # The nonce also identifies an owned pod if the create response was lost.
    uid=$(backup_runner_uid <<<"$current") || return 1
    [[ -z "$RUNNER_UID" || "$RUNNER_UID" == "$uid" ]] || return 1
    jq -n --arg uid "$uid" '{kind:"DeleteOptions",apiVersion:"v1",preconditions:{uid:$uid}}' |
      k delete --raw "/api/v1/namespaces/$MSG_NS/pods/$NATS_RUNNER" -f - >/dev/null || return 1
    k -n "$MSG_NS" wait --for=delete "pod/$NATS_RUNNER" --timeout=90s >/dev/null || return 1
  fi
  RUNNER_UID=''
  RUNNER_NONCE=''
}

wait_for_pod_selector() {
  local namespace=$1 selector=$2 timeout_seconds=$3 deadline
  deadline=$(( SECONDS + timeout_seconds ))
  while (( SECONDS < deadline )); do
    if [[ -n "$(k -n "$namespace" get pod -l "$selector" -o name 2>/dev/null)" ]]; then
      return 0
    fi
    sleep 2
  done
  die "no pod appeared for selector $selector in namespace $namespace"
}

# The bootstrap principal backs up __system__ only. The isolated replacement
# has no account configuration, so the CLI restores that account into $G.
# Both old v1 archives and new backups contain the pinned server's detailed jsz.
nats_account_state() {
  jq -ceS --arg account "$1" '
    def name: if type=="string" and length>0 then . else error("missing identity") end;
    def count: if type=="number" and .>=0 and .<=9007199254740991 and floor==.
               then . else error("missing or imprecise sequence/count") end;
    def position: {consumer_seq:(.consumer_seq|count),stream_seq:(.stream_seq|count)};
    def named: sort_by(.name) |
      if (map(.name)|unique|length)==length then . else error("duplicate identity") end;
    [.account_details[] | select(.name==$account and .id==$account)] |
    if length!=1 then error("expected one backup account") else .[0].stream_detail end |
    if type!="array" or length==0 then error("missing stream inventory") else . end |
    map(. as $stream |
      {name:(.name|name), messages:(.state.messages|count), bytes:(.state.bytes|count),
       first_seq:(.state.first_seq|count), last_seq:(.state.last_seq|count),
       consumers:((.consumer_detail // []) | map(
         if .stream_name!=$stream.name or (.config|type)!="object" or
             (.config.durable_name!=null and .config.durable_name!=.name)
           then error("inconsistent consumer identity") else . end |
         {name:(.name|name), durable:.config.durable_name, delivered:(.delivered|position),
          ack_floor:(.ack_floor|position), ack_pending:(.num_ack_pending|count),
          pending:(.num_pending|count)}) | named)} |
      if (.consumers|length)!=($stream.state.consumer_count|count)
        then error("incomplete consumer inventory") else . end) | named'
}

nats_restore_evidence() {
  jq -c '{nats_streams:length,nats_consumers:(map(.consumers|length)|add),nats_messages:(map(.messages)|add),
    nats_account_state:{format:"named-streams-consumer-state-v1",source:"__system__",restored:"$G"}}'
}

# PostgreSQL 16 provides sha256(bytea). JSON framing distinguishes fields and
# row boundaries; C collation orders sets independently of database locale.
# The original single-set ledger predates the set_name column.
migration_ledger_sql() {
  cat <<'SQL'
select encode(sha256(convert_to(coalesce(jsonb_agg(
    jsonb_build_array(set_name, version, name, checksum)
    order by set_name collate "C", version), '[]'::jsonb)::text, 'UTF8')), 'hex')
from (select coalesce(to_jsonb(m)->>'set_name','') as set_name,
             version, name, checksum from public.schema_migrations m) ledger
SQL
}

source_postgres_inventory_row() {
  k -n "$DATA_NS" exec "$1" -c postgres -- psql --username postgres --dbname "$2" \
    --no-psqlrc --tuples-only --no-align --set ON_ERROR_STOP=1 --command "
    select current_database(), (select count(*) from schema_migrations),
      (select count(*) from pg_class where relkind='r' and relnamespace='public'::regnamespace),
      (select count(*) from pg_class where relkind='r' and relrowsecurity and not relforcerowsecurity),
      pg_current_wal_lsn(), ($(migration_ledger_sql))"
}

restored_migration_ledger() {
  k -n "$DRILL_NS" exec postgres-restore -c restore -- bash -ceu '
    export PGPASSWORD="$(cat /run/secrets/postgres/password)"
    exec psql --host "$(cat /run/secrets/postgres/host)" --username kanz_restore --dbname "$1" \
      --no-psqlrc --tuples-only --no-align --set ON_ERROR_STOP=1 --command "$2"
  ' _ "$1" "$(migration_ledger_sql)"
}

verify_migration_ledger() {
  local expected=$1 observed=$2
  [[ "$expected" =~ ^[0-9a-f]{64}$ && "$observed" == "$expected" ]] ||
    die 'restored PostgreSQL migration ledger differs from source'
}

migration_ledger_evidence() {
  case "$1" in
    kanz-testnet-data-plane-v2) printf '%s\n' '{"state":"VERIFIED","format":"set-version-name-checksum-sha256-v1"}' ;;
    kanz-testnet-data-plane-v1) printf '%s\n' '{"state":"UNKNOWN","reason":"historical archive contains migration row counts only"}' ;;
    *) die 'unsupported migration ledger format' ;;
  esac
}

verify_postgres_inventory() {
  local dir=$1 format=$2 row database file
  local row_pattern='^([a-z_]+)\|[0-9]+\|[0-9]+\|0\|[0-9A-F]+/[0-9A-F]+'
  case "$format" in
    kanz-testnet-data-plane-v1) ;;
    kanz-testnet-data-plane-v2) row_pattern+='\|[0-9a-f]{64}' ;;
    *) die 'unsupported database inventory format' ;;
  esac
  shift 2
  local -A required=() seen=()
  for database in "$@"; do required[$database]=1; done
  [[ -f "$dir/source.tsv" && ! -L "$dir/source.tsv" && -r "$dir/source.tsv" ]] || die 'missing database inventory'
  # Restore consumes each newline-terminated row with read. Reject a truncated
  # final row rather than counting a database that restore would never visit.
  while IFS= read -r row || [[ -n "$row" ]]; do
    [[ "$row" =~ ${row_pattern}$ ]] || die 'invalid database inventory row'
    database=${BASH_REMATCH[1]}
    [[ -n "${required[$database]:-}" && -z "${seen[$database]:-}" ]] || die 'unexpected or duplicate database'
    seen[$database]=1
    file="$dir/$database.dump"
    [[ -f "$file" && ! -L "$file" && -r "$file" && "$(head -c 5 "$file")" == PGDMP ]] || die 'missing or unreadable custom database dump'
  done <"$dir/source.tsv"
  [[ "$(tail -c 1 "$dir/source.tsv" | od -An -tu1 | tr -d '[:space:]')" == 10 && ${#seen[@]} == "$#" ]] || die 'incomplete database inventory'
  for file in "$dir"/*.dump; do
    database=${file##*/}; database=${database%.dump}
    [[ "$database" =~ ^[a-z_]+$ && -n "${seen[$database]:-}" ]] || die 'unexpected database dump'
  done
}

# This isolated drill has no timestamp for an incident, the last recoverable
# write at that incident, or restoration of the operational service.
emit_restore_receipt() {
  local key=$1 sha=$2 release=$3 elapsed=$4 databases=$5 nats=$6 details=$7 version=$8
  [[ "$elapsed" =~ ^[0-9]+$ ]] || die 'invalid restore drill duration'
  jq -n --arg key "$key" --arg sha "$sha" --arg release "$release" --argjson elapsed "$elapsed" \
    --argjson databases "$databases" --argjson nats "$nats" --argjson details "$details" --arg version "$version" '
    $nats + $details + {format:"kanz-testnet-restore-drill-v1",status:"RESTORE_VERIFIED",
      key:$key,version_id:$version,sha256:$sha,source_release_commit:$release,databases:$databases,drill_elapsed_seconds:$elapsed,
      operational_recovery:{state:"UNKNOWN",rpo_seconds:null,rto_seconds:null,
        reason:"incident and operational recovery timestamps unavailable"}}'
}

backup() {
  require_boundary
  refuse_active_writers

  local started backup_id work archive key archive_sha primary
  started=$(date -u +%s)
  backup_id="$(date -u +%Y%m%dT%H%M%SZ)-$(printf '%s' "$RELEASE_COMMIT" | cut -c1-12)"
  RECOVERY_WORK=$(mktemp -d "/var/tmp/kanz-backup-${backup_id}.XXXXXX")
  work="$RECOVERY_WORK/payload"
  RECOVERY_ARCHIVE="$RECOVERY_WORK/bundle.tar.gz"
  archive=$RECOVERY_ARCHIVE
  key="testnet/data-plane/${backup_id}/bundle.tar.gz"

  cleanup_backup() {
    local rc=$?
    if ! cleanup_backup_runner; then printf '%s\n' 'recovery-refused: backup runner cleanup unverified' >&2; rc=2; fi
    [[ "$RECOVERY_WORK" == /var/tmp/kanz-backup-* ]] && rm -rf -- "$RECOVERY_WORK"
    exit "$rc"
  }
  trap cleanup_backup EXIT

  primary=$(k -n "$DATA_NS" get cluster kanz-testnet-postgres -o jsonpath='{.status.currentPrimary}')
  [[ "$primary" == kanz-testnet-postgres-* ]] || die 'CloudNativePG did not report a bounded primary'
  mkdir -p "$work/postgres" "$work/nats" "$work/redis"
  : >"$work/postgres/source.tsv"
  local database
  for database in "${DATABASES[@]}"; do
    k -n "$DATA_NS" exec "$primary" -c postgres -- bash -ceu '
      exec pg_dump --username postgres --dbname "$1" \
        --format=custom --compress=zstd:3 --no-owner --no-privileges
    ' _ "$database" >"$work/postgres/${database}.dump"
    source_postgres_inventory_row "$primary" "$database" >>"$work/postgres/source.tsv"
  done
  verify_postgres_inventory "$work/postgres" kanz-testnet-data-plane-v2 "${DATABASES[@]}"

  redis_backup_checkpoint >"$work/redis/source.txt" || die 'Redis did not confirm a current authenticated checkpoint'
  k -n "$MSG_NS" exec redis-0 -c redis -- cat /data/dump.rdb >"$work/redis/dump.rdb"
  [[ -s "$work/redis/dump.rdb" ]] || die 'Redis produced an empty RDB artifact'

  create_backup_runner
  k -n "$MSG_NS" wait --for=condition=Ready "pod/$NATS_RUNNER" --timeout=120s >/dev/null
  local runner
  runner=$(k -n "$MSG_NS" get pod "$NATS_RUNNER" -o json | backup_runner_uid)
  [[ "$runner" == "$RUNNER_UID" ]] || die 'backup runner identity changed before snapshot'
  k -n "$MSG_NS" exec "$NATS_RUNNER" -c bootstrap -- sh -ceu '
    rm -rf /tmp/nats-backup
    nats --server "$NATS_URL" --tlscert "$NATS_CERT" --tlskey "$NATS_KEY" --tlsca "$NATS_CA" \
      account backup --check --critical-warnings --force /tmp/nats-backup >/dev/null
    tar -C /tmp -czf - nats-backup
  ' >"$work/nats/account.tar.gz"
  [[ -s "$work/nats/account.tar.gz" ]] || die 'NATS produced an empty account backup'
  cleanup_backup_runner || die 'backup runner cleanup unverified'

  local pf
  timeout 30 /usr/local/bin/k3s kubectl -n "$MSG_NS" port-forward --address 127.0.0.1 pod/nats-0 18222:8222 >"$work/nats/port-forward.log" 2>&1 & pf=$!
  local ready=0
  for _ in $(seq 1 30); do
    if curl -fsS --max-time 2 'http://127.0.0.1:18222/jsz?streams=true&consumers=true&config=true' >"$work/nats/source.json" 2>/dev/null; then
      ready=1
      break
    fi
    kill -0 "$pf" 2>/dev/null || break
    sleep 1
  done
  [[ "$ready" == 1 ]] || die "NATS monitoring port-forward did not become ready: $(cat "$work/nats/port-forward.log")"
  kill "$pf" >/dev/null 2>&1 || true
  wait "$pf" 2>/dev/null || true
  rm -f "$work/nats/port-forward.log"
  nats_account_state __system__ <"$work/nats/source.json" >/dev/null || die 'NATS backup account inventory incomplete'

  jq -n \
    --arg backup_id "$backup_id" --arg created_at "$(date -u +%FT%TZ)" \
    --arg release_commit "$RELEASE_COMMIT" --arg source_region ap-northeast-1 \
    --arg source_instance i-0ee1235614219f089 --arg tv_mode "$TV_MODE" \
    '{format:"kanz-testnet-data-plane-v2",backup_id:$backup_id,created_at:$created_at,release_commit:$release_commit,source_region:$source_region,source_instance:$source_instance,quiesced:true} + (if $tv_mode=="qualify" then {tv_sync:{tenant:"__system__",retention_seconds:604800,history:"not-yet-qualified"}} else {} end)' \
    >"$work/metadata.json"
  (cd "$work" && find . -type f ! -name MANIFEST.sha256 -print0 | sort -z | xargs -0 sha256sum >MANIFEST.sha256)
  tar -C "$work" -czf "$archive" .
  archive_sha=$(sha256sum "$archive" | awk '{print $1}')

  local evidence elapsed
  evidence=$(upload_recovery_archive "$key" "$archive" "$archive_sha") || die 'backup object verification failed'
  elapsed=$(( $(date -u +%s) - started ))
  jq -n --arg backup_id "$backup_id" --arg key "$key" --arg sha256 "$archive_sha" \
    --argjson evidence "$evidence" --argjson elapsed_seconds "$elapsed" \
    '$evidence + {status:"BACKUP_VERIFIED",backup_id:$backup_id,key:$key,sha256:$sha256,elapsed_seconds:$elapsed_seconds}'
}

object_version() {
  jq -ser 'select(length==1) | .[0].VersionId | strings |
    select(length>0 and .!="null" and (test("[[:cntrl:]]")|not))'
}

upload_recovery_archive() {
  local key=$1 archive=$2 sha=$3 version
  version=$(aws s3api put-object --region "$RECOVERY_REGION" --bucket "$RECOVERY_BUCKET" --key "$key" \
    --body "$archive" --server-side-encryption aws:kms --ssekms-key-id "$RECOVERY_KMS_KEY_ARN" \
    --checksum-algorithm SHA256 --metadata "sha256=${sha},format=kanz-testnet-data-plane-v2" \
    --output json | object_version) || die 'upload did not identify an immutable object version'
  read_recovery_archive "$key" "$version" "${archive}.download" "$sha"
}

read_recovery_archive() {
  local key=$1 version=$2 destination=$3 expected_sha=${4:-} head retention sha until returned
  local -a object=(--region "$RECOVERY_REGION" --bucket "$RECOVERY_BUCKET" --key "$key" "--version-id=$version" --output json)
  head=$(aws s3api head-object "${object[@]}" --checksum-mode ENABLED) || die 'could not inspect recovery version'
  [[ "$(object_version <<<"$head")" == "$version" ]] || die 'recovery object version changed'
  sha=$(jq -er --arg kms "$RECOVERY_KMS_KEY_ARN" '
    select(.ServerSideEncryption=="aws:kms" and .SSEKMSKeyId==$kms and (.ContentLength|numbers)>0) |
    .Metadata.sha256 | strings | select(test("^[0-9a-f]{64}$"))' <<<"$head") || die 'invalid recovery object metadata'
  [[ -z "$expected_sha" || "$sha" == "$expected_sha" ]] || die 'recovery object differs from upload'
  retention=$(aws s3api get-object-retention "${object[@]}") || die 'could not inspect recovery retention'
  until=$(jq -er 'select(.Retention.Mode=="GOVERNANCE") | .Retention.RetainUntilDate | strings | select(length>0)' \
    <<<"$retention") || die 'artifact lacks Governance retention'
  local expires
  expires=$(date -d "$until" +%s) || die 'invalid recovery retention date'
  (( expires > $(date +%s) )) || die 'recovery retention expired'
  returned=$(aws s3api get-object "${object[@]}" --checksum-mode ENABLED "$destination" | object_version) || die 'could not download recovery version'
  [[ "$returned" == "$version" ]] || die 'downloaded version differs from locked metadata'
  [[ "$(sha256sum "$destination" | awk '{print $1}')" == "$sha" ]] || die 'artifact checksum differs from locked metadata'
  jq -n --arg version "$version" --arg sha "$sha" --arg until "$until" \
    '{version_id:$version,sha256:$sha,object_lock_retain_until:$until}'
}

redis_backup_checkpoint() {
  k -n "$MSG_NS" exec redis-0 -c redis -- sh -ceu '
    auth=$(cat /run/secrets/redis/password)
    [ -n "$auth" ] || exit 1
    redis() { redis-cli -a "$auth" --no-auth-warning --raw "$@" 2>/dev/null; }
    # redis-cli can exit successfully with an error reply. An older dump on
    # disk cannot stand in for an acknowledged synchronous SAVE.
    [ "$(redis PING)" = PONG ] && [ "$(redis SAVE)" = OK ] || exit 1
    keys=$(redis DBSIZE)
    last=$(redis LASTSAVE)
    for value in "$keys" "$last"; do
      case "$value" in ""|*[!0-9]*|0?*) exit 1;; esac
      [ "${#value}" -le 16 ] && [ "$value" -le 9007199254740991 ] || exit 1
    done
    [ "$last" -gt 0 ] || exit 1
    printf "%s\n%s\n" "$keys" "$last"
  '
}

download_restore_archive() {
  local destination=$1 selection=${2:-} key version expected_sha='' evidence
  if [[ -n "$selection" ]]; then
    selection=$(printf '%s' "$selection" | base64 -d | jq -cse '
      select(length==1) | .[0] |
      select((.key | strings | test("^testnet/data-plane/[0-9]{8}T[0-9]{6}Z-[0-9a-f]{12}/bundle\\.tar\\.gz\\z")) and
             (.version_id | strings | length>0) and .version_id!="null" and (.version_id | test("[[:cntrl:]]") | not) and
             (.sha256 | strings | test("^[0-9a-f]{64}\\z")))') || die 'invalid backup selection'
    key=$(jq -er .key <<<"$selection")
    version=$(jq -er .version_id <<<"$selection")
    expected_sha=$(jq -er .sha256 <<<"$selection")
  else
    key=$(aws s3api list-objects-v2 --region "$RECOVERY_REGION" --bucket "$RECOVERY_BUCKET" --prefix testnet/data-plane/ --query 'sort_by(Contents,&LastModified)[-1].Key' --output text) || die 'could not list recovery artifacts'
    [[ "$key" == testnet/data-plane/*/bundle.tar.gz ]] || die 'no bounded data-plane recovery artifact exists'
    version=$(aws s3api head-object --region "$RECOVERY_REGION" --bucket "$RECOVERY_BUCKET" --key "$key" \
      --output json | object_version) || die 'selected recovery key has no immutable version'
  fi
  evidence=$(read_recovery_archive "$key" "$version" "$destination" "$expected_sha") || die 'restore object verification failed'
  jq -c --arg key "$key" '. + {key:$key}' <<<"$evidence"
}

restore() {
  require_boundary
  local started work key archive archive_sha version object_evidence
  started=$SECONDS
  work=$(mktemp -d /var/tmp/kanz-restore.XXXXXX)
  archive="$work/bundle.tar.gz"
  RECOVERY_WORK=$work
  RECOVERY_ARCHIVE=$archive
  cleanup_restore() {
    local rc=$?
    if ! cleanup_tv_restore_namespace; then printf '%s\n' 'recovery-refused: owned drill cleanup unverified' >&2; rc=2; fi
    [[ "$RECOVERY_WORK" == /var/tmp/kanz-restore.* ]] && rm -rf -- "$RECOVERY_WORK"
    exit "$rc"
  }
  trap cleanup_restore EXIT

  object_evidence=$(download_restore_archive "$archive" "$BACKUP_SELECTION") || die 'could not select and verify recovery artifact'
  key=$(jq -er .key <<<"$object_evidence")
  version=$(jq -er .version_id <<<"$object_evidence")
  archive_sha=$(jq -er .sha256 <<<"$object_evidence")
  tar -tzf "$archive" | grep -Eq '(^/|(^|/)\.\.(/|$))' && die 'artifact contains an unsafe path'
  mkdir "$work/extracted"
  tar -C "$work/extracted" -xzf "$archive"
  (cd "$work/extracted" && sha256sum --check --strict MANIFEST.sha256 >/dev/null) || die 'artifact member checksum failed'
  local archive_format
  archive_format=$(jq -er 'select(.quiesced==true) | .format |
    select(.=="kanz-testnet-data-plane-v1" or .=="kanz-testnet-data-plane-v2")' "$work/extracted/metadata.json") || die 'artifact is incompatible or not quiesced'

  local owner_posture='ALTER ROLE kanz_restore CREATEDB NOSUPERUSER NOBYPASSRLS'
  local tv_logging=''
  verify_postgres_inventory "$work/extracted/postgres" "$archive_format" "${DATABASES[@]}"
  if [[ "$TV_MODE" == qualify ]]; then
    jq -e '.tv_sync=={tenant:"__system__",retention_seconds:604800,history:"not-yet-qualified"}' "$work/extracted/metadata.json" >/dev/null || die 'artifact lacks explicit TV history inventory'
    owner_posture='ALTER ROLE kanz_restore CREATEDB CREATEROLE NOSUPERUSER NOBYPASSRLS'
    # PostgreSQL 16 logging controls: no statement text, including error,
    # duration and sampled statements, during isolated role provisioning.
    tv_logging='  postgresql:
    parameters:
      log_statement: "none"
      log_min_error_statement: "panic"
      log_error_verbosity: "terse"
      log_min_duration_statement: "-1"
      log_min_duration_sample: "-1"
      log_transaction_sample_rate: "0"
      log_parameter_max_length: "0"
      log_parameter_max_length_on_error: "0"'

  fi
  begin_tv_restore_namespace
  cat <<EOF | k apply -f - >/dev/null
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: { name: default-deny, namespace: ${DRILL_NS} }
spec: { podSelector: {}, policyTypes: [Ingress, Egress] }
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: { name: bounded-drill, namespace: ${DRILL_NS} }
spec:
  podSelector: {}
  policyTypes: [Ingress, Egress]
  ingress:
    - from: [{ podSelector: {} }]
    - from: [{ namespaceSelector: { matchLabels: { kubernetes.io/metadata.name: cnpg-system } } }]
      ports: [{ protocol: TCP, port: 8000 }]
  egress:
    - to: [{ podSelector: {} }]
    - to: [{ namespaceSelector: { matchLabels: { kubernetes.io/metadata.name: kube-system } } }]
      ports: [{ protocol: UDP, port: 53 }, { protocol: TCP, port: 53 }]
    - to: [{ ipBlock: { cidr: 10.71.0.15/32 } }]
      ports: [{ protocol: TCP, port: 6443 }]
---
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: { name: kanz-postgres-drill, namespace: ${DRILL_NS}, labels: { kanz.io/ha: UNSUPPORTED } }
spec:
  instances: 1
  imageName: ${PG_IMAGE}
  enableSuperuserAccess: false
${tv_logging}
  bootstrap:
    initdb:
      database: restore_control
      owner: kanz_restore
      dataChecksums: true
      encoding: UTF8
      localeCollate: C
      localeCType: C
      postInitApplicationSQL: ["${owner_posture}"]
  resources: { requests: { cpu: 250m, memory: 512Mi }, limits: { cpu: "1", memory: 1Gi } }
  storage: { size: 10Gi, storageClass: local-path }
  walStorage: { size: 2Gi, storageClass: local-path }
  affinity: { enablePodAntiAffinity: false }
---
apiVersion: v1
kind: Pod
metadata: { name: postgres-restore, namespace: ${DRILL_NS}, labels: { app: postgres-restore } }
spec:
  restartPolicy: Never
  securityContext: { runAsNonRoot: true, runAsUser: 26, runAsGroup: 26, fsGroup: 26, seccompProfile: { type: RuntimeDefault } }
  containers:
    - name: restore
      image: ${PG_IMAGE}
      command: [sleep, "3600"]
      resources: { requests: { cpu: 25m, memory: 64Mi }, limits: { cpu: 250m, memory: 256Mi } }
      securityContext: { allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: { drop: [ALL] } }
      volumeMounts: [{ name: credentials, mountPath: /run/secrets/postgres, readOnly: true }, { name: tmp, mountPath: /tmp }]
  volumes:
    - name: credentials
      secret: { secretName: kanz-postgres-drill-app, defaultMode: 0440 }
    - name: tmp
      emptyDir: { sizeLimit: 1Gi }
---
apiVersion: v1
kind: Pod
metadata: { name: redis-restore, namespace: ${DRILL_NS}, labels: { app: redis-restore } }
spec:
  restartPolicy: Never
  securityContext: { runAsNonRoot: true, runAsUser: 999, runAsGroup: 999, fsGroup: 999, seccompProfile: { type: RuntimeDefault } }
  containers:
    - name: redis
      image: ${REDIS_IMAGE}
      command: [sh, -ceu, 'while [ ! -f /data/restore.ready ]; do sleep 1; done; exec redis-server --bind 127.0.0.1 --protected-mode yes --appendonly no --save "" --dir /data --dbfilename dump.rdb']
      resources: { requests: { cpu: 25m, memory: 32Mi }, limits: { cpu: 200m, memory: 256Mi } }
      securityContext: { allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: { drop: [ALL] } }
      volumeMounts: [{ name: data, mountPath: /data }]
  volumes: [{ name: data, emptyDir: { sizeLimit: 1Gi } }]
---
apiVersion: v1
kind: Pod
metadata: { name: nats-restore, namespace: ${DRILL_NS}, labels: { app: nats-restore } }
spec:
  restartPolicy: Never
  securityContext: { runAsNonRoot: true, runAsUser: 1000, runAsGroup: 1000, fsGroup: 1000, seccompProfile: { type: RuntimeDefault } }
  containers:
    - name: server
      image: ${NATS_IMAGE}
      args: [--jetstream, --store_dir=/data, --http_port=8222]
      resources: { requests: { cpu: 25m, memory: 32Mi }, limits: { cpu: 200m, memory: 256Mi } }
      securityContext: { allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: { drop: [ALL] } }
      volumeMounts: [{ name: data, mountPath: /data }]
    - name: restore
      image: ${NATS_BOX_IMAGE}
      command: [sleep, "3600"]
      resources: { requests: { cpu: 10m, memory: 16Mi }, limits: { cpu: 100m, memory: 128Mi } }
      securityContext: { allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: { drop: [ALL] } }
      volumeMounts: [{ name: transfer, mountPath: /restore }, { name: tmp, mountPath: /tmp }]
  volumes:
    - { name: data, emptyDir: { sizeLimit: 4Gi } }
    - { name: transfer, emptyDir: { sizeLimit: 1Gi } }
    - { name: tmp, emptyDir: { medium: Memory, sizeLimit: 32Mi } }
EOF
  wait_for_pod_selector "$DRILL_NS" cnpg.io/cluster=kanz-postgres-drill 120
  k -n "$DRILL_NS" wait --for=condition=Ready pod -l cnpg.io/cluster=kanz-postgres-drill --timeout=300s >/dev/null
  k -n "$DRILL_NS" wait --for=condition=Ready pod/postgres-restore --timeout=180s >/dev/null

  local postgres_ready=0
  for _ in $(seq 1 90); do
    if k -n "$DRILL_NS" exec postgres-restore -c restore -- bash -ceu '
      exec pg_isready --host "$(cat /run/secrets/postgres/host)" --username kanz_restore --dbname restore_control
    ' >/dev/null 2>&1; then
      postgres_ready=1
      break
    fi
    sleep 2
  done
  [[ "$postgres_ready" == 1 ]] || die 'restored PostgreSQL service did not become reachable'

  local database expected_migrations expected_tables expected_unsafe source_lsn expected_ledger restored
  while IFS='|' read -r database expected_migrations expected_tables expected_unsafe source_lsn expected_ledger; do
    [[ " ${DATABASES[*]} " == *" $database "* ]] || die "artifact names unexpected database $database"
    k -n "$DRILL_NS" exec -i postgres-restore -c restore -- bash -ceu '
      export PGPASSWORD="$(cat /run/secrets/postgres/password)"
      host=$(cat /run/secrets/postgres/host)
      createdb --host "$host" --username kanz_restore "$1"
      pg_restore --host "$host" --username kanz_restore --dbname "$1" --no-owner --no-privileges --exit-on-error
    ' _ "$database" <"$work/extracted/postgres/$database.dump"
    restored=$(k -n "$DRILL_NS" exec postgres-restore -c restore -- bash -ceu '
      export PGPASSWORD="$(cat /run/secrets/postgres/password)"
      psql --host "$(cat /run/secrets/postgres/host)" --username kanz_restore --dbname "$1" --no-psqlrc --tuples-only --no-align --field-separator="|" --command "select (select count(*) from schema_migrations), (select count(*) from pg_class where relkind='"'"'r'"'"' and relnamespace=(select oid from pg_namespace where nspname='"'"'public'"'"')), (select count(*) from pg_class where relkind='"'"'r'"'"' and relrowsecurity and not relforcerowsecurity)"
    ' _ "$database")
    [[ "$restored" == "$expected_migrations|$expected_tables|$expected_unsafe" && "$expected_unsafe" == 0 ]] || die "PostgreSQL invariant mismatch for $database"
    if [[ "$archive_format" == kanz-testnet-data-plane-v2 ]]; then
      restored=$(restored_migration_ledger "$database") || die 'cannot read restored PostgreSQL migration ledger'
      verify_migration_ledger "$expected_ledger" "$restored"
    fi
  done <"$work/extracted/postgres/source.tsv"

  k -n "$DRILL_NS" exec -i redis-restore -c redis -- sh -ceu 'cat > /data/dump.rdb; touch /data/restore.ready' <"$work/extracted/redis/dump.rdb"
  local expected_redis restored_redis=''
  expected_redis=$(sed -n '1p' "$work/extracted/redis/source.txt")
  for _ in $(seq 1 60); do
    restored_redis=$(k -n "$DRILL_NS" exec redis-restore -c redis -- redis-cli --raw DBSIZE 2>/dev/null || true)
    [[ "$restored_redis" =~ ^[0-9]+$ ]] && break
    sleep 1
  done
  [[ "$restored_redis" == "$expected_redis" ]] || die 'Redis restored key count differs from source'

  k -n "$DRILL_NS" exec -i nats-restore -c restore -- sh -ceu 'tar -C /restore -xzf -' <"$work/extracted/nats/account.tar.gz"
  k -n "$DRILL_NS" exec nats-restore -c restore -- nats --server nats://127.0.0.1:4222 account restore /restore/nats-backup >/dev/null
  local expected_state restored_state nats_evidence
  expected_state=$(nats_account_state __system__ <"$work/extracted/nats/source.json") || die 'NATS source account inventory incomplete'
  restored_state=$(k -n "$DRILL_NS" exec nats-restore -c restore -- wget -qO- 'http://127.0.0.1:8222/jsz?streams=true&consumers=true&config=true' |
    nats_account_state '$G') || die 'NATS restored account inventory incomplete'
  [[ "$restored_state" == "$expected_state" ]] || die 'NATS named streams or consumer positions differ from source'
  nats_evidence=$(nats_restore_evidence <<<"$expected_state")

  local metadata_release details
  metadata_release=$(jq -r .release_commit "$work/extracted/metadata.json")
  if [[ "$TV_MODE" == qualify ]]; then
    local tv_proof
    tv_proof=$(qualify_tv_restored_snapshot "$archive_sha")
    details=$(jq -n --argjson replay "$tv_proof" '{tv_sync:$replay,scope:"isolated restored archive only; not current target or genesis readiness"}')
  else
    details=$(jq -n --argjson keys "$restored_redis" '{redis_keys:$keys,isolation:"temporary replacement namespace on source-region node"}')
  fi
  cleanup_tv_restore_namespace || die 'drill cleanup did not complete'
  emit_restore_receipt "$key" "$archive_sha" "$metadata_release" "$(( SECONDS - started ))" "${#DATABASES[@]}" "$nats_evidence" "$(jq --argjson ledger "$(migration_ledger_evidence "$archive_format")" '.cleanup="verified" | .postgres_migration_ledger=$ledger' <<<"$details")" "$version"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  case "$OPERATION" in
    backup) backup ;;
    restore) restore ;;
    *) die 'usage: testnet-data-plane-recovery.sh backup|restore BUCKET KMS_KEY_ARN RELEASE_COMMIT' ;;
  esac
fi
