[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^i-[0-9a-f]+$')]
    [string]$InstanceId,

    [ValidateSet('ap-northeast-1')]
    [string]$Region = 'ap-northeast-1',

    [string]$Profile = 'kanz-platform',

    [switch]$Apply,

    [switch]$RecreateCompletedJobs
)

$ErrorActionPreference = 'Stop'
if ($RecreateCompletedJobs -and -not $Apply) { throw 'Completed Job recreation requires an explicit apply.' }
$aws = (Get-Command aws.exe -ErrorAction Stop).Source
$kubectl = (Get-Command kubectl.exe -ErrorAction Stop).Source
$overlay = Join-Path $PSScriptRoot '..\kanz\infra\overlays\testnet-tokyo\data'

$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
& git -C $repoRoot fetch origin main --quiet
if ($LASTEXITCODE -ne 0) { throw 'Could not fetch origin/main.' }
$releaseCommit = (& git -C $repoRoot rev-parse origin/main).Trim()
$headCommit = (& git -C $repoRoot rev-parse HEAD).Trim()
if ($headCommit -ne $releaseCommit) { throw "Data-plane installation refused: HEAD $headCommit is not exact origin/main $releaseCommit." }
& git -C $repoRoot diff --quiet $releaseCommit -- 'tools/Install-TestnetDataPlane.ps1' 'tools/Resolve-EcrReleaseDigest.ps1' 'kanz/infra'
if ($LASTEXITCODE -ne 0) { throw 'Data-plane installation refused: reviewed source inputs differ.' }
. (Join-Path $PSScriptRoot 'Resolve-EcrReleaseDigest.ps1')
$tvReleaseLock = Join-Path $repoRoot 'kanz/infra/overlays/testnet-tokyo/tv-sync-release.json'
$tvRelease = Read-ReviewedTvSyncRelease -LockPath $tvReleaseLock
$tvMigrationDigest = $tvRelease.images.'kanz-migrate'.destination.Split('@')[1]
$tvMigrationTag = Resolve-ReviewedTestnetReleaseTag -LockPath $tvReleaseLock -Repository 'kanz-migrate' -SourceCommit $tvRelease.release_sha -Digest $tvMigrationDigest
if ((Resolve-EcrReleaseDigest -Aws $aws -Profile $Profile -Region $Region -Repository 'kanz-migrate' -ReleaseTag $tvMigrationTag) -cne $tvMigrationDigest) { throw 'Paired tv-sync migration image is absent from the reviewed release tag.' }

function Invoke-SsmCommands {
    param(
        [Parameter(Mandatory = $true)]
        [string[]]$Commands,
        [Parameter(Mandatory = $true)]
        [string]$Comment,
        [int]$TimeoutSeconds = 600
    )

    $parameters = @{ commands = $Commands } | ConvertTo-Json -Compress -Depth 4
    $commandId = (& $aws ssm send-command --profile $Profile --region $Region `
        --instance-ids $InstanceId --document-name AWS-RunShellScript `
        --comment $Comment --parameters $parameters `
        --query 'Command.CommandId' --output text).Trim()
    if ($LASTEXITCODE -ne 0 -or -not $commandId) {
        throw "AWS rejected SSM command: $Comment"
    }

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        Start-Sleep -Milliseconds 750
        $result = & $aws ssm get-command-invocation --profile $Profile --region $Region `
            --command-id $commandId --instance-id $InstanceId `
            --query '{Status:Status,Output:StandardOutputContent,Error:StandardErrorContent}' `
            --output json 2>$null | ConvertFrom-Json
        if ($result.Status -in @('Success', 'Cancelled', 'Failed', 'TimedOut')) { break }
    } while ([DateTime]::UtcNow -lt $deadline)

    if (-not $result -or $result.Status -in @('Pending', 'InProgress', 'Delayed')) {
        throw "SSM command $commandId did not finish within $TimeoutSeconds seconds: $Comment"
    }
    if ($result.Status -ne 'Success') {
        throw "SSM command $commandId failed ($Comment): $($result.Error)"
    }
    return [pscustomobject]@{
        CommandId = $commandId
        Output = $result.Output.Trim()
    }
}

$renderedLines = & $kubectl kustomize --load-restrictor=LoadRestrictionsNone $overlay
if ($LASTEXITCODE -ne 0 -or -not $renderedLines) {
    throw 'The Tokyo data-plane overlay did not render.'
}
$rendered = ($renderedLines -join "`n") + "`n"
$resourceCount = ([regex]::Matches($rendered, '(?m)^kind: ')).Count
if ($resourceCount -ne 38) {
    throw "Expected 38 rendered resources; found $resourceCount. Review the transfer and readiness contract."
}
foreach ($required in @(
    'sync_interval: always', '--appendfsync always',
    'kanz-testnet-postgres', 'name: nats', 'name: redis'
)) {
    if (-not $rendered.Contains($required)) {
        throw "Rendered data plane is missing required marker: $required"
    }
}
if ($rendered -match '(?m)^\s*image:\s+(?!\S+@sha256:[0-9a-f]{64}\s*$)\S+\s*$') {
    throw 'A mutable image tag survived the Tokyo data-plane render.'
}

if (-not $rendered.Contains($tvRelease.images.'kanz-migrate'.destination)) { throw 'Rendered data-plane job lost the paired tv-sync migration image.' }

$bytes = [Text.Encoding]::UTF8.GetBytes($rendered)
$sha256 = [Security.Cryptography.SHA256]::Create()
try {
    $manifestHash = ([BitConverter]::ToString($sha256.ComputeHash($bytes))).Replace('-', '').ToLowerInvariant()
}
finally {
    $sha256.Dispose()
}
$compressed = New-Object IO.MemoryStream
$gzip = New-Object IO.Compression.GzipStream($compressed, [IO.Compression.CompressionLevel]::Optimal, $true)
try {
    $gzip.Write($bytes, 0, $bytes.Length)
}
finally {
    $gzip.Dispose()
}
$payload = [Convert]::ToBase64String($compressed.ToArray())
$compressed.Dispose()

$transferId = [Guid]::NewGuid().ToString('N')
$remoteBase = "/var/tmp/kanz-data-plane-$transferId"
$remotePayload = "$remoteBase.b64"
$remoteManifest = "$remoteBase.yaml"

Invoke-SsmCommands -Comment 'Initialize Kanz testnet data-plane transfer' -TimeoutSeconds 60 -Commands @(
    'set -eu',
    "umask 077; : > '$remotePayload'"
) | Out-Null

$chunkSize = 3000
$chunks = for ($offset = 0; $offset -lt $payload.Length; $offset += $chunkSize) {
    $length = [Math]::Min($chunkSize, $payload.Length - $offset)
    $payload.Substring($offset, $length)
}
for ($batchStart = 0; $batchStart -lt $chunks.Count; $batchStart += 4) {
    $batchEnd = [Math]::Min($batchStart + 4, $chunks.Count)
    $commands = @('set -eu')
    for ($index = $batchStart; $index -lt $batchEnd; $index++) {
        $commands += "printf '%s' '$($chunks[$index])' >> '$remotePayload'"
    }
    Invoke-SsmCommands -Comment "Stage Kanz data-plane manifest $($batchStart + 1)-$batchEnd/$($chunks.Count)" `
        -TimeoutSeconds 60 -Commands $commands | Out-Null
}

$applyFlag = if ($Apply) { '1' } else { '0' }
$recreateFlag = if ($RecreateCompletedJobs) { '1' } else { '0' }
$verification = Invoke-SsmCommands -Comment 'Verify or install Kanz Tokyo data plane' -TimeoutSeconds 1200 -Commands @(
    'set -Eeuo pipefail',
    "payload='$remotePayload'",
    "manifest='$remoteManifest'",
    'validation="${manifest}.validation"',
    'cleanup() { rm -f "${payload}" "${manifest}" "${validation}"; }',
    'trap cleanup EXIT',
    'base64 -d "${payload}" | gzip -d > "${manifest}"',
    ("printf '%s  %s\n' '$manifestHash' " + '"${manifest}" | sha256sum --check --status'),
    ('test "$(grep -c ''^kind:'' "${manifest}")" = ''' + $resourceCount + "'"),
    # Validate against an isolated namespace name every time. Otherwise a
    # legitimate Job template update is rejected as immutable before the
    # installer can explicitly recreate a completed Job below.
    'sed -e ''s/^  namespace: kanz-data$/  namespace: default/'' -e ''s/^  namespace: kanz-messaging$/  namespace: default/'' "${manifest}" > "${validation}"',
    'validation_manifest="${validation}"',
    'echo data-plane-server-dry-run-namespace-substitute=default',
    'k3s kubectl apply --server-side --dry-run=server --field-manager=kanz-bootstrap -f "${validation_manifest}" >/dev/null',
    'echo data-plane-server-dry-run-ok',
    "apply='$applyFlag'",
    "recreate_completed='$recreateFlag'",
    'if [[ "${apply}" = 0 ]]; then exit 0; fi',
    'tenancy_before=absent; if k3s kubectl -n kanz-messaging get configmap/nats-tenants >/dev/null 2>&1; then tenancy_before=$(k3s kubectl -n kanz-messaging get configmap/nats-tenants -o jsonpath=''{.data.tenants\.conf}'' | sha256sum | awk ''{print $1}''); fi',
    # Explicit source-first recreation only for complete, UID-owned data jobs.
    # Preflight every fixed job before deleting any; active/pending work refuses.
    'recreate_targets=(); for item in kanz-data/postgres-provisioner kanz-data/postgres-migrations kanz-data/tv-sync-migration kanz-messaging/nats-bootstrap; do ns=${item%/*}; job=${item#*/}; existing=$(k3s kubectl -n "${ns}" get job "${job}" --ignore-not-found=true -o json); if [[ -z "${existing}" ]]; then continue; fi; uid=$(jq -er ''select((.status.active // 0)==0 and any(.status.conditions[]?; .type=="Complete" and .status=="True")) | .metadata.uid'' <<<"${existing}") || { echo job-recreation-refused-not-complete >&2; exit 1; }; [[ "${uid}" =~ ^[0-9a-f-]{36}$ ]]; k3s kubectl -n "${ns}" get pods -l "batch.kubernetes.io/controller-uid=${uid}" -o json | jq -e ''all(.items[]; .status.phase=="Succeeded" or .status.phase=="Failed")'' >/dev/null; if [[ "${recreate_completed}" = 1 && "${ns}" = kanz-data ]]; then recreate_targets+=("${item}:${uid}"); fi; done; for target in "${recreate_targets[@]}"; do item=${target%:*}; uid=${target##*:}; ns=${item%/*}; job=${item#*/}; jq -cn --arg uid "${uid}" ''{apiVersion:"v1",kind:"DeleteOptions",propagationPolicy:"Foreground",preconditions:{uid:$uid}}'' | k3s kubectl delete --raw "/apis/batch/v1/namespaces/${ns}/jobs/${job}" -f - >/dev/null; k3s kubectl -n "${ns}" wait --for=delete "job/${job}" --timeout=60s >/dev/null; echo "completed-job-recreated=${job}"; done',
    'k3s kubectl apply --server-side --field-manager=kanz-bootstrap -f "${manifest}" >/dev/null',
    'tenancy_after=$(k3s kubectl -n kanz-messaging get configmap/nats-tenants -o jsonpath=''{.data.tenants\.conf}'' | sha256sum | awk ''{print $1}'')',
    'if [[ "${tenancy_before}" != absent && "${tenancy_before}" != "${tenancy_after}" ]] && k3s kubectl -n kanz-messaging get pod/nats-0 >/dev/null 2>&1; then mounted=; for attempt in $(seq 1 120); do mounted=$(k3s kubectl -n kanz-messaging exec nats-0 -c nats -- cat /etc/nats/tenants.conf | sha256sum | awk ''{print $1}'') || true; [[ "${mounted}" = "${tenancy_after}" ]] && break; sleep 1; done; [[ "${mounted}" = "${tenancy_after}" ]]; k3s kubectl -n kanz-messaging exec nats-0 -c nats -- nats-server -t -c /etc/nats/nats.conf >/dev/null; k3s kubectl -n kanz-messaging exec nats-0 -c nats -- nats-server --signal reload >/dev/null; echo nats-tenancy-reloaded-after-config-change; fi',
    'redis_sa=$(k3s kubectl -n kanz-messaging get pod redis-0 -o jsonpath=''{.spec.serviceAccountName}'' 2>/dev/null || true)',
    'if [[ -n "${redis_sa}" && "${redis_sa}" != redis ]]; then k3s kubectl -n kanz-messaging delete pod redis-0 --wait=false >/dev/null; fi',
    'k3s kubectl wait --for=jsonpath=''{.status.phase}''=Active namespace/kanz-data --timeout=60s >/dev/null',
    'k3s kubectl -n kanz-data wait --for=condition=Ready cluster/kanz-testnet-postgres --timeout=600s >/dev/null',
    'k3s kubectl -n kanz-messaging rollout status statefulset/nats --timeout=600s >/dev/null',
    'k3s kubectl -n kanz-messaging rollout status statefulset/redis --timeout=600s >/dev/null',
    'k3s kubectl -n kanz-data wait --for=condition=complete job/postgres-provisioner --timeout=600s >/dev/null',
    'k3s kubectl -n kanz-data wait --for=condition=complete job/postgres-migrations --timeout=900s >/dev/null',
    'k3s kubectl -n kanz-data wait --for=condition=complete job/tv-sync-migration --timeout=300s >/dev/null',
    'k3s kubectl -n kanz-messaging wait --for=condition=complete job/nats-bootstrap --timeout=600s >/dev/null',
    'migration_image="012619468098.dkr.ecr.ap-northeast-1.amazonaws.com/kanz-migrate@sha256:147db5e4edbb6151f44f1b2c799d14d44437298dc00e19d8ad983d54ab333a24"; migration_uid=$(k3s kubectl -n kanz-data get job/tv-sync-migration -o json | jq -er --arg image "${migration_image}" ''select(any(.status.conditions[]?; .type=="Complete" and .status=="True")) | select(any(.spec.template.spec.containers[]; .name=="migrate-tv-sync" and .image==$image)) | .metadata.uid''); [[ "${migration_uid}" =~ ^[0-9a-f-]{36}$ ]]; k3s kubectl -n kanz-data get pods -l "batch.kubernetes.io/controller-uid=${migration_uid}" -o json | jq -e --arg uid "${migration_uid}" --arg image "${migration_image}" ''[.items[] | select(any(.metadata.ownerReferences[]?; .kind=="Job" and .name=="tv-sync-migration" and .uid==$uid and .controller==true)) | select(.status.phase=="Succeeded") | select(any(.spec.containers[]; .name=="migrate-tv-sync" and .image==$image)) | select(any(.status.containerStatuses[]; .name=="migrate-tv-sync" and .state.terminated.exitCode==0 and (.imageID|endswith("@"+($image|split("@")[1])))))] | length==1'' >/dev/null',
    'pgpod=$(k3s kubectl -n kanz-data get pod -l cnpg.io/cluster=kanz-testnet-postgres,role=primary -o jsonpath=''{.items[0].metadata.name}'')',
    'test -n "${pgpod}"',
    'roles=$(k3s kubectl -n kanz-data exec "${pgpod}" -c postgres -- psql -Atqc "select count(*) from pg_roles where rolname IN (''accounting_app'',''accounting_migrate'',''audit_app'',''audit_migrate'',''identity_app'',''identity_migrate'',''oms_app'',''oms_migrate'',''regulatory_app'',''regulatory_migrate'',''risk_engine_app'',''risk_engine_migrate'',''venue_binance_app'',''venue_binance_migrate'',''venue_okx_app'',''venue_okx_migrate'',''tv_sync_app'',''tv_sync_migrate'') and not rolsuper and not rolbypassrls")',
    'test "${roles}" = 18',
    'redis_sync=$(k3s kubectl -n kanz-messaging exec redis-0 -c redis -- sh -c ''redis-cli -a "$(cat /run/secrets/redis/password)" --no-auth-warning CONFIG GET appendfsync | tail -1'')',
    'test "${redis_sync}" = always',
    'grep -Fq ''sync_interval: always'' "${manifest}"',
    'echo postgres-ready-and-roles-constrained',
    'echo postgres-migrations-current-and-force-rls-verified',
    'echo nats-ready-and-fsync-before-ack',
    'echo redis-ready-and-appendfsync-always'
)

[pscustomobject]@{
    CommandId    = $verification.CommandId
    Mode         = if ($Apply) { 'Apply' } else { 'ServerDryRun' }
    ManifestHash = $manifestHash
    Resources    = $resourceCount
    Evidence     = $verification.Output
}
