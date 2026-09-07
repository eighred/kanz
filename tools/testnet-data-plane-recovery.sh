#!/usr/bin/env bash
set -Eeuo pipefail

# This script runs only on the SSM-controlled K3s host. AWS credentials remain
# in the host instance profile; database and Redis credentials remain inside
# their CSI/Secret-mounted containers. Neither crosses kubectl stdout.

readonly OPERATION=${1:-}
readonly RECOVERY_BUCKET=${2:-}
readonly RECOVERY_KMS_KEY_ARN=${3:-}
readonly RELEASE_COMMIT=${4:-unknown}
readonly RECOVERY_REGION=ap-northeast-3
readonly DATA_NS=kanz-data
readonly MSG_NS=kanz-messaging
readonly DRILL_NS=kanz-recovery-drill
readonly PG_HOST=kanz-testnet-postgres-rw.kanz-data.svc.cluster.local
readonly -a DATABASES=(accounting audit identity oms regulatory risk_engine venue_binance venue_okx)
readonly PG_IMAGE='ghcr.io/cloudnative-pg/postgresql:16.10-standard-bookworm@sha256:533e45e0a00c156bdab7c4d9a84fffa87259594725a3909c898e8645727d9555'
readonly REDIS_IMAGE='redis@sha256:bb186d083732f669da90be8b0f975a37812b15e913465bb14d845db72a4e3e08'
readonly NATS_IMAGE='nats@sha256:d4ac35882ac65aff236cd65b9d3fa4d24332c681e1a85f94eedccd3cdd65b1da'
readonly NATS_BOX_IMAGE='natsio/nats-box@sha256:ffce8bd103383f179f8c7f11cf645726acf5d17280706c530c3b342dbe16334c'

k() { k3s kubectl "$@"; }
die() { printf 'recovery-refused: %s\n' "$*" >&2; exit 2; }

require_boundary() {
  [[ "$RECOVERY_BUCKET" == kanz-testnet-tokyo-recovery-* ]] || die 'unexpected recovery bucket'
  [[ "$RECOVERY_KMS_KEY_ARN" == arn:aws:kms:ap-northeast-3:*:key/* ]] || die 'unexpected recovery KMS key'
  aws sts get-caller-identity --query Account --output text | grep -qx '012619468098' || die 'wrong AWS account'
  k get namespace "$DATA_NS" >/dev/null
  k get namespace "$MSG_NS" >/dev/null
}

refuse_active_writers() {
  local ns count
  for ns in kanz-services kanz-venues; do
    if k get namespace "$ns" >/dev/null 2>&1; then
      count=$(k -n "$ns" get pods -o json | jq '[.items[] | select(.status.phase != "Succeeded" and .status.phase != "Failed")] | length')
      [[ "$count" == 0 ]] || die "$ns has $count non-terminal pod(s); quiesce every writer before a cross-database backup"
    fi
  done
  [[ "$(k -n "$DATA_NS" get job postgres-provisioner -o jsonpath='{.status.succeeded}')" == 1 ]] || die 'PostgreSQL roles are not provisioned'
  [[ "$(k -n "$DATA_NS" get job postgres-migrations -o jsonpath='{.status.succeeded}')" == 1 ]] || die 'PostgreSQL migrations are not complete'
  [[ "$(k -n "$MSG_NS" get job nats-bootstrap -o jsonpath='{.status.succeeded}')" == 1 ]] || die 'NATS topology is not provisioned'
}

clone_job_as_runner() {
  local namespace=$1 job=$2 pod=$3 container=$4
  k -n "$namespace" delete pod "$pod" --ignore-not-found=true --wait=true >/dev/null
  local job_json
  job_json=$(k -n "$namespace" get job "$job" -o json)
  printf '%s' "$job_json" | jq \
    --arg pod "$pod" --arg container "$container" '
      .kind="Pod" | .apiVersion="v1" | .metadata.name=$pod |
      del(.metadata.uid,.metadata.resourceVersion,.metadata.creationTimestamp,
          .metadata.generation,.metadata.annotations,.metadata.ownerReferences,.status) |
      .spec=.spec.template.spec | del(.spec.template) |
      .spec.restartPolicy="Never" |
      (.spec.containers[] | select(.name==$container) | .command)=["sleep","1800"] |
      (.spec.containers[] | select(.name==$container) | .args)=[]' |
    k create -f - >/dev/null
  k -n "$namespace" wait --for=condition=Ready "pod/$pod" --timeout=120s >/dev/null
}

backup() {
  require_boundary
  refuse_active_writers

  local started backup_id work archive key archive_sha retain_until pg_runner nats_runner
  started=$(date -u +%s)
  backup_id="$(date -u +%Y%m%dT%H%M%SZ)-$(printf '%s' "$RELEASE_COMMIT" | cut -c1-12)"
  work=$(mktemp -d "/var/tmp/kanz-backup-${backup_id}.XXXXXX")
  archive="/var/tmp/kanz-data-plane-${backup_id}.tar.gz"
  key="testnet/data-plane/${backup_id}/bundle.tar.gz"
  pg_runner=postgres-backup-runner
  nats_runner=nats-backup-runner

  cleanup_backup() {
    k -n "$DATA_NS" delete pod "$pg_runner" --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
    k -n "$MSG_NS" delete pod "$nats_runner" --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
    rm -rf -- "$work"
    rm -f -- "$archive" "${archive}.download"
  }
  trap cleanup_backup EXIT

  mkdir -p "$work/postgres" "$work/nats" "$work/redis"
  clone_job_as_runner "$DATA_NS" postgres-provisioner "$pg_runner" provisioner

  : >"$work/postgres/source.tsv"
  local database
  for database in "${DATABASES[@]}"; do
    k -n "$DATA_NS" exec "$pg_runner" -c provisioner -- bash -ceu '
      export PGPASSWORD="$(cat /run/secrets/bootstrap/password)"
      exec pg_dump --host "$1" --username kanz_bootstrap --dbname "$2" \
        --format=custom --compress=zstd:3 --no-owner --no-privileges
    ' _ "$PG_HOST" "$database" >"$work/postgres/${database}.dump"
    k -n "$DATA_NS" exec "$pg_runner" -c provisioner -- bash -ceu '
      export PGPASSWORD="$(cat /run/secrets/bootstrap/password)"
      psql --host "$1" --username kanz_bootstrap --dbname "$2" --no-psqlrc --tuples-only --no-align \
        --command "select current_database(), (select count(*) from schema_migrations), (select count(*) from pg_class where relkind='"'"'r'"'"' and relnamespace=(select oid from pg_namespace where nspname='"'"'public'"'"')), (select count(*) from pg_class where relkind='"'"'r'"'"' and relrowsecurity and not relforcerowsecurity), pg_current_wal_lsn()"
    ' _ "$PG_HOST" "$database" >>"$work/postgres/source.tsv"
  done

  k -n "$MSG_NS" exec redis-0 -c redis -- sh -ceu '
    auth=$(cat /run/secrets/redis/password)
    redis-cli -a "$auth" --no-auth-warning SAVE >/dev/null
    redis-cli -a "$auth" --no-auth-warning --raw DBSIZE
    redis-cli -a "$auth" --no-auth-warning --raw LASTSAVE
  ' >"$work/redis/source.txt"
  k -n "$MSG_NS" exec redis-0 -c redis -- cat /data/dump.rdb >"$work/redis/dump.rdb"
  [[ -s "$work/redis/dump.rdb" ]] || die 'Redis produced an empty RDB artifact'

  clone_job_as_runner "$MSG_NS" nats-bootstrap "$nats_runner" bootstrap
  k -n "$MSG_NS" exec "$nats_runner" -c bootstrap -- sh -ceu '
    rm -rf /tmp/nats-backup
    nats --server "$NATS_URL" --tlscert "$NATS_CERT" --tlskey "$NATS_KEY" --tlsca "$NATS_CA" \
      account backup --check --critical-warnings --force /tmp/nats-backup >/dev/null
    tar -C /tmp -czf - nats-backup
  ' >"$work/nats/account.tar.gz"
  [[ -s "$work/nats/account.tar.gz" ]] || die 'NATS produced an empty account backup'

  local pf
  timeout 30 k -n "$MSG_NS" port-forward --address 127.0.0.1 pod/nats-0 18222:8222 >"$work/nats/port-forward.log" 2>&1 & pf=$!
  sleep 3
  curl -fsS --max-time 5 'http://127.0.0.1:18222/jsz?streams=true&consumers=true&config=true' >"$work/nats/source.json"
  kill "$pf" >/dev/null 2>&1 || true
  wait "$pf" 2>/dev/null || true
  rm -f "$work/nats/port-forward.log"

  jq -n \
    --arg backup_id "$backup_id" --arg created_at "$(date -u +%FT%TZ)" \
    --arg release_commit "$RELEASE_COMMIT" --arg source_region ap-northeast-1 \
    --arg source_instance i-0ee1235614219f089 \
    '{format:"kanz-testnet-data-plane-v1",backup_id:$backup_id,created_at:$created_at,release_commit:$release_commit,source_region:$source_region,source_instance:$source_instance,quiesced:true}' \
    >"$work/metadata.json"
  (cd "$work" && find . -type f ! -name MANIFEST.sha256 -print0 | sort -z | xargs -0 sha256sum >MANIFEST.sha256)
  tar -C "$work" -czf "$archive" .
  archive_sha=$(sha256sum "$archive" | awk '{print $1}')

  aws s3api put-object --region "$RECOVERY_REGION" --bucket "$RECOVERY_BUCKET" --key "$key" \
    --body "$archive" --server-side-encryption aws:kms --ssekms-key-id "$RECOVERY_KMS_KEY_ARN" \
    --checksum-algorithm SHA256 --metadata "sha256=${archive_sha},format=kanz-testnet-data-plane-v1" >/dev/null
  aws s3api get-object --region "$RECOVERY_REGION" --bucket "$RECOVERY_BUCKET" --key "$key" \
    --checksum-mode ENABLED "${archive}.download" >/dev/null
  [[ "$(sha256sum "${archive}.download" | awk '{print $1}')" == "$archive_sha" ]] || die 'downloaded recovery artifact checksum differs from upload'

  local head elapsed
  head=$(aws s3api head-object --region "$RECOVERY_REGION" --bucket "$RECOVERY_BUCKET" --key "$key" --checksum-mode ENABLED)
  jq -e --arg kms "$RECOVERY_KMS_KEY_ARN" --arg sha "$archive_sha" '
    .ServerSideEncryption=="aws:kms" and .SSEKMSKeyId==$kms and
    .ObjectLockMode=="GOVERNANCE" and .Metadata.sha256==$sha and (.ContentLength > 0)
  ' <<<"$head" >/dev/null || die 'S3 object lacks the required KMS, Object Lock, checksum, or content boundary'
  retain_until=$(jq -r .ObjectLockRetainUntilDate <<<"$head")
  elapsed=$(( $(date -u +%s) - started ))
  jq -n --arg backup_id "$backup_id" --arg key "$key" --arg sha256 "$archive_sha" \
    --arg retain_until "$retain_until" --argjson elapsed_seconds "$elapsed" \
    '{status:"BACKUP_VERIFIED",backup_id:$backup_id,key:$key,sha256:$sha256,object_lock_retain_until:$retain_until,elapsed_seconds:$elapsed_seconds}'
}

restore() {
  require_boundary
  local started work key archive archive_sha head
  started=$(date -u +%s)
  work=$(mktemp -d /var/tmp/kanz-restore.XXXXXX)
  archive="$work/bundle.tar.gz"
  cleanup_restore() {
    [[ "$DRILL_NS" == kanz-recovery-drill ]] && k delete namespace "$DRILL_NS" --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
    [[ "$work" == /var/tmp/kanz-restore.* ]] && rm -rf -- "$work"
  }
  trap cleanup_restore EXIT

  key=$(aws s3api list-objects-v2 --region "$RECOVERY_REGION" --bucket "$RECOVERY_BUCKET" --prefix testnet/data-plane/ --query 'sort_by(Contents,&LastModified)[-1].Key' --output text)
  [[ "$key" == testnet/data-plane/*/bundle.tar.gz ]] || die 'no bounded data-plane recovery artifact exists'
  head=$(aws s3api head-object --region "$RECOVERY_REGION" --bucket "$RECOVERY_BUCKET" --key "$key" --checksum-mode ENABLED)
  archive_sha=$(jq -r '.Metadata.sha256 // empty' <<<"$head")
  jq -e --arg kms "$RECOVERY_KMS_KEY_ARN" '.ServerSideEncryption=="aws:kms" and .SSEKMSKeyId==$kms and .ObjectLockMode=="GOVERNANCE" and (.ContentLength > 0)' <<<"$head" >/dev/null || die 'artifact lacks the required KMS and Object Lock boundary'
  [[ "$archive_sha" =~ ^[0-9a-f]{64}$ ]] || die 'artifact lacks SHA-256 metadata'
  aws s3api get-object --region "$RECOVERY_REGION" --bucket "$RECOVERY_BUCKET" --key "$key" --checksum-mode ENABLED "$archive" >/dev/null
  [[ "$(sha256sum "$archive" | awk '{print $1}')" == "$archive_sha" ]] || die 'artifact checksum differs from locked metadata'
  tar -tzf "$archive" | grep -Eq '(^/|(^|/)\.\.(/|$))' && die 'artifact contains an unsafe path'
  mkdir "$work/extracted"
  tar -C "$work/extracted" -xzf "$archive"
  (cd "$work/extracted" && sha256sum --check --strict MANIFEST.sha256 >/dev/null) || die 'artifact member checksum failed'
  jq -e '.format=="kanz-testnet-data-plane-v1" and .quiesced==true' "$work/extracted/metadata.json" >/dev/null || die 'artifact is incompatible or not quiesced'

  k delete namespace "$DRILL_NS" --ignore-not-found=true --wait=true >/dev/null
  cat <<EOF | k apply -f - >/dev/null
apiVersion: v1
kind: Namespace
metadata: { name: ${DRILL_NS}, labels: { kanz.io/purpose: recovery-drill } }
---
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
  bootstrap:
    initdb:
      database: restore_control
      owner: kanz_restore
      dataChecksums: true
      encoding: UTF8
      localeCollate: C
      localeCType: C
      postInitApplicationSQL: ["ALTER ROLE kanz_restore CREATEDB NOSUPERUSER NOBYPASSRLS"]
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
  k -n "$DRILL_NS" wait --for=condition=Ready pod -l cnpg.io/cluster=kanz-postgres-drill --timeout=300s >/dev/null
  k -n "$DRILL_NS" wait --for=condition=Ready pod/postgres-restore --timeout=180s >/dev/null

  local database expected_migrations expected_tables expected_unsafe source_lsn restored
  while IFS='|' read -r database expected_migrations expected_tables expected_unsafe source_lsn; do
    [[ " ${DATABASES[*]} " == *" $database "* ]] || die "artifact names unexpected database $database"
    k -n "$DRILL_NS" exec postgres-restore -c restore -- bash -ceu '
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
  k -n "$DRILL_NS" exec nats-restore -c restore -- nats --server nats://127.0.0.1:4222 account restore --force /restore/nats-backup >/dev/null
  local expected_streams expected_consumers expected_messages restored_jsz
  expected_streams=$(jq '.streams // 0' "$work/extracted/nats/source.json")
  expected_consumers=$(jq '[.account_details[].stream_detail[].consumer_detail // [] | length] | add // 0' "$work/extracted/nats/source.json")
  expected_messages=$(jq '[.account_details[].stream_detail[].state.messages // 0] | add // 0' "$work/extracted/nats/source.json")
  restored_jsz=$(k -n "$DRILL_NS" exec nats-restore -c restore -- wget -qO- 'http://127.0.0.1:8222/jsz?streams=true&consumers=true')
  jq -e --argjson streams "$expected_streams" --argjson consumers "$expected_consumers" --argjson messages "$expected_messages" '(.streams // 0)==$streams and ([.account_details[].stream_detail[].consumer_detail // [] | length] | add // 0)==$consumers and ([.account_details[].stream_detail[].state.messages // 0] | add // 0)==$messages' <<<"$restored_jsz" >/dev/null || die 'NATS restored totals differ from source'

  local elapsed metadata_release
  elapsed=$(( $(date -u +%s) - started ))
  metadata_release=$(jq -r .release_commit "$work/extracted/metadata.json")
  jq -n --arg key "$key" --arg sha256 "$archive_sha" --arg source_release_commit "$metadata_release" --argjson databases "${#DATABASES[@]}" --argjson redis_keys "$restored_redis" --argjson nats_streams "$expected_streams" --argjson nats_consumers "$expected_consumers" --argjson nats_messages "$expected_messages" --argjson rpo_seconds 0 --argjson rto_seconds "$elapsed" '{status:"RESTORE_VERIFIED",key:$key,sha256:$sha256,source_release_commit:$source_release_commit,databases:$databases,redis_keys:$redis_keys,nats_streams:$nats_streams,nats_consumers:$nats_consumers,nats_messages:$nats_messages,rpo_seconds:$rpo_seconds,rto_seconds:$rto_seconds,isolation:"temporary replacement namespace on source-region node"}'
}

case "$OPERATION" in
  backup) backup ;;
  restore) restore ;;
  *) die 'usage: testnet-data-plane-recovery.sh backup|restore BUCKET KMS_KEY_ARN RELEASE_COMMIT' ;;
esac
