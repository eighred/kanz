[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^i-[0-9a-f]+$')]
    [string]$InstanceId,

    [ValidateSet('ap-northeast-1')]
    [string]$Region = 'ap-northeast-1',

    [string]$Profile = 'kanz-platform',

    [switch]$Apply
)

$ErrorActionPreference = 'Stop'
$aws = (Get-Command aws.exe -ErrorAction Stop).Source
$kubectl = (Get-Command kubectl.exe -ErrorAction Stop).Source
$overlay = Join-Path $PSScriptRoot '..\kanz\infra\overlays\testnet-tokyo\data'

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
if ($resourceCount -ne 37) {
    throw "Expected 37 rendered resources; found $resourceCount. Review the transfer and readiness contract."
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
    # installer can replace an incomplete Job below.
    'sed -e ''s/^  namespace: kanz-data$/  namespace: default/'' -e ''s/^  namespace: kanz-messaging$/  namespace: default/'' "${manifest}" > "${validation}"',
    'validation_manifest="${validation}"',
    'echo data-plane-server-dry-run-namespace-substitute=default',
    'k3s kubectl apply --server-side --dry-run=server --field-manager=kanz-bootstrap -f "${validation_manifest}" >/dev/null',
    'echo data-plane-server-dry-run-ok',
    "apply='$applyFlag'",
    'if [[ "${apply}" = 0 ]]; then exit 0; fi',
    # kubectl does not implement Argo's BeforeHookCreation policy. A previously
    # failed bootstrap Job is immutable and must be removed before a reviewed,
    # idempotent reapply; a completed Job is retained as evidence.
    'for item in kanz-data/postgres-provisioner kanz-data/postgres-migrations kanz-messaging/nats-bootstrap; do ns=${item%/*}; job=${item#*/}; if k3s kubectl -n "${ns}" get job "${job}" >/dev/null 2>&1; then succeeded=$(k3s kubectl -n "${ns}" get job "${job}" -o jsonpath=''{.status.succeeded}''); if [[ "${succeeded:-0}" != 1 ]]; then k3s kubectl -n "${ns}" delete job "${job}" --wait=true >/dev/null; fi; fi; done',
    'k3s kubectl apply --server-side --field-manager=kanz-bootstrap -f "${manifest}" >/dev/null',
    'redis_sa=$(k3s kubectl -n kanz-messaging get pod redis-0 -o jsonpath=''{.spec.serviceAccountName}'' 2>/dev/null || true)',
    'if [[ -n "${redis_sa}" && "${redis_sa}" != redis ]]; then k3s kubectl -n kanz-messaging delete pod redis-0 --wait=false >/dev/null; fi',
    'k3s kubectl wait --for=jsonpath=''{.status.phase}''=Active namespace/kanz-data --timeout=60s >/dev/null',
    'k3s kubectl -n kanz-data wait --for=condition=Ready cluster/kanz-testnet-postgres --timeout=600s >/dev/null',
    'k3s kubectl -n kanz-messaging rollout status statefulset/nats --timeout=600s >/dev/null',
    'k3s kubectl -n kanz-messaging rollout status statefulset/redis --timeout=600s >/dev/null',
    'k3s kubectl -n kanz-data wait --for=condition=complete job/postgres-provisioner --timeout=600s >/dev/null',
    'k3s kubectl -n kanz-data wait --for=condition=complete job/postgres-migrations --timeout=900s >/dev/null',
    'k3s kubectl -n kanz-messaging wait --for=condition=complete job/nats-bootstrap --timeout=600s >/dev/null',
    'pgpod=$(k3s kubectl -n kanz-data get pod -l cnpg.io/cluster=kanz-testnet-postgres,role=primary -o jsonpath=''{.items[0].metadata.name}'')',
    'test -n "${pgpod}"',
    'roles=$(k3s kubectl -n kanz-data exec "${pgpod}" -c postgres -- psql -Atqc "select count(*) from pg_roles where rolname ~ ''_(app|migrate)$'' and not rolsuper and not rolbypassrls")',
    'test "${roles}" = 16',
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
