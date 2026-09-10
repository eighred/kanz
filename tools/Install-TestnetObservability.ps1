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
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$overlay = Join-Path $repoRoot 'kanz\infra\overlays\testnet-tokyo-observability'

function Invoke-SsmCommands {
    param(
        [Parameter(Mandatory = $true)][string[]]$Commands,
        [Parameter(Mandatory = $true)][string]$Comment,
        [int]$TimeoutSeconds = 600
    )
    $parameters = @{ commands = $Commands } | ConvertTo-Json -Compress -Depth 4
    $commandId = (& $aws ssm send-command --profile $Profile --region $Region `
        --instance-ids $InstanceId --document-name AWS-RunShellScript `
        --comment $Comment --parameters $parameters `
        --query 'Command.CommandId' --output text).Trim()
    if ($LASTEXITCODE -ne 0 -or -not $commandId) { throw "AWS rejected SSM command: $Comment" }

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
    return [pscustomobject]@{ CommandId = $commandId; Output = $result.Output.Trim() }
}

& git -C $repoRoot fetch origin main --quiet
if ($LASTEXITCODE -ne 0) { throw 'Could not fetch origin/main.' }
$releaseCommit = (& git -C $repoRoot rev-parse origin/main).Trim()
$headCommit = (& git -C $repoRoot rev-parse HEAD).Trim()
if ($headCommit -ne $releaseCommit) {
    throw "Observability installation is refused: HEAD $headCommit is not exact origin/main $releaseCommit."
}
$provenancePaths = @(
    'tools/Install-TestnetObservability.ps1',
    'kanz/infra/overlays/testnet-tokyo-observability',
    'kanz/infra/observability/node-exporter.yaml',
    'kanz/infra/observability/prometheus.yaml',
    'kanz/infra/observability/rules-configmap.yaml'
)
& git -C $repoRoot diff --quiet $releaseCommit -- @provenancePaths
if ($LASTEXITCODE -ne 0) {
    throw 'Observability installation is refused: a rendered input differs from merged origin/main.'
}

$renderedLines = & $kubectl kustomize --load-restrictor=LoadRestrictionsNone $overlay
if ($LASTEXITCODE -ne 0 -or -not $renderedLines) { throw 'The Tokyo observability overlay did not render.' }
$rendered = ($renderedLines -join "`n") + "`n"
$resourceCount = ([regex]::Matches($rendered, '(?m)^kind: ')).Count
if ($resourceCount -ne 9) { throw "Expected 9 rendered resources; found $resourceCount." }
$images = [regex]::Matches($rendered, '(?m)^\s*image:\s+(\S+)\s*$')
if ($images.Count -ne 1) { throw "Expected one rendered container image; found $($images.Count)." }
$imageRef = $images[0].Groups[1].Value
if ($imageRef -notmatch '^012619468098\.dkr\.ecr\.ap-northeast-1\.amazonaws\.com/prometheus@(?<digest>sha256:[0-9a-f]{64})$') {
    throw "Rendered Prometheus image is outside the immutable Tokyo ECR boundary: $imageRef"
}
$digest = $Matches.digest
$retainedDigest = (& $aws ecr describe-images --profile $Profile --region $Region `
    --repository-name prometheus --image-ids imageTag=v3.13.3-amd64 `
    --query 'imageDetails[0].imageDigest' --output text 2>$null).Trim()
if ($LASTEXITCODE -ne 0 -or $retainedDigest -ne $digest) {
    throw "Tokyo ECR does not retain prometheus@$digest under immutable tag v3.13.3-amd64."
}
foreach ($required in @('kind: Namespace', 'name: kanz-observability', 'kind: Deployment',
    'name: prometheus', 'name: prometheus-rules', 'claimName: prometheus-data')) {
    if (-not $rendered.Contains($required)) { throw "Rendered observability is missing prerequisite: $required" }
}
foreach ($forbidden in @('quay.io', 'imagePullSecrets:', 'kubernetes.io/dockerconfigjson', 'kind: DaemonSet')) {
    if ($rendered.Contains($forbidden)) { throw "Rendered observability contains forbidden marker: $forbidden" }
}

$bytes = [Text.Encoding]::UTF8.GetBytes($rendered)
$sha256 = [Security.Cryptography.SHA256]::Create()
try { $manifestHash = ([BitConverter]::ToString($sha256.ComputeHash($bytes))).Replace('-', '').ToLowerInvariant() }
finally { $sha256.Dispose() }
$compressed = New-Object IO.MemoryStream
$gzip = New-Object IO.Compression.GzipStream($compressed, [IO.Compression.CompressionLevel]::Optimal, $true)
try { $gzip.Write($bytes, 0, $bytes.Length) }
finally { $gzip.Dispose() }
$payload = [Convert]::ToBase64String($compressed.ToArray())
$compressed.Dispose()

$transferId = [Guid]::NewGuid().ToString('N')
$remoteBase = "/var/tmp/kanz-observability-$transferId"
$remotePayload = "$remoteBase.b64"
$remoteManifest = "$remoteBase.yaml"
$remoteDryRunManifest = "$remoteBase.dry-run.yaml"
Invoke-SsmCommands -Comment 'Initialize Kanz observability transfer' -TimeoutSeconds 60 -Commands @(
    'set -eu', "umask 077; : > '$remotePayload'"
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
    Invoke-SsmCommands -Comment "Stage Kanz observability manifest $($batchStart + 1)-$batchEnd/$($chunks.Count)" `
        -TimeoutSeconds 60 -Commands $commands | Out-Null
}

$applyFlag = if ($Apply) { '1' } else { '0' }
$verification = Invoke-SsmCommands -Comment 'Verify or install Kanz Tokyo observability' -TimeoutSeconds 900 -Commands @(
    'set -Eeuo pipefail',
    "payload='$remotePayload'", "manifest='$remoteManifest'", "dry_run_manifest='$remoteDryRunManifest'", 'fresh=0',
    'finish() { rc=$?; set +e; if [[ "${rc}" != 0 && "${fresh}" = 1 ]]; then echo fresh-observability-rollback-started >&2; /usr/local/bin/k3s kubectl delete --ignore-not-found=true --wait=true -f "${manifest}" >&2 || echo fresh-observability-rollback-failed >&2; fi; rm -f "${payload}" "${manifest}" "${dry_run_manifest}"; exit "${rc}"; }',
    'trap finish EXIT',
    'base64 -d "${payload}" | gzip -d > "${manifest}"',
    ("printf '%s  %s\n' '$manifestHash' " + '"${manifest}" | sha256sum --check --status'),
    ('test "$(grep -c ''^kind:'' "${manifest}")" = ''' + $resourceCount + "'"),
    'test "$(grep -Ec ''^[[:space:]]*image: 012619468098\.dkr\.ecr\.ap-northeast-1\.amazonaws\.com/prometheus@sha256:[0-9a-f]{64}$'' "${manifest}")" = 1',
    '! grep -Eq ''quay\.io|imagePullSecrets:|kubernetes\.io/dockerconfigjson|kind: DaemonSet'' "${manifest}"',
    'if /usr/local/bin/k3s kubectl get namespace kanz-observability >/dev/null 2>&1; then cp "${manifest}" "${dry_run_manifest}"; else sed ''s/namespace: kanz-observability/namespace: default/g'' "${manifest}" > "${dry_run_manifest}"; fi',
    '/usr/local/bin/k3s kubectl apply --server-side --dry-run=server --field-manager=kanz-bootstrap -f "${dry_run_manifest}" >/dev/null',
    'echo observability-server-dry-run-ok',
    "apply='$applyFlag'", 'if [[ "${apply}" = 0 ]]; then exit 0; fi',
    'if ! /usr/local/bin/k3s kubectl get namespace kanz-observability >/dev/null 2>&1; then fresh=1; fi',
    '/usr/local/bin/k3s kubectl apply --server-side --field-manager=kanz-bootstrap -f "${manifest}" >/dev/null',
    '/usr/local/bin/k3s kubectl -n kanz-observability wait --for=jsonpath=''{.status.phase}''=Bound pvc/prometheus-data --timeout=180s >/dev/null',
    '/usr/local/bin/k3s kubectl -n kanz-observability rollout status deployment/prometheus --timeout=300s >/dev/null',
    '/usr/local/bin/k3s kubectl -n kanz-observability get endpointslice -l kubernetes.io/service-name=prometheus -o json | jq -e ''[.items[].endpoints[]? | select(.conditions.ready != false) | .addresses[]?] | length > 0'' >/dev/null',
    'prometheus_cluster_ip=$(/usr/local/bin/k3s kubectl -n kanz-observability get service/prometheus -o jsonpath=''{.spec.clusterIP}'')',
    'curl -fsS --max-time 5 "http://${prometheus_cluster_ip}:9090/-/ready" >/dev/null',
    'curl -fsS --get --data-urlencode ''query=vector(1)'' "http://${prometheus_cluster_ip}:9090/api/v1/query" | jq -e ''.status == "success" and .data.result[0].value[1] == "1"'' >/dev/null',
    'echo observability-prometheus-ready',
    'echo observability-query-api-verified',
    'echo observability-image-matches-reviewed-ecr-lock'
)

[pscustomobject]@{
    CommandId = $verification.CommandId
    Mode = if ($Apply) { 'Apply' } else { 'ServerDryRun' }
    ReleaseCommit = $releaseCommit
    ManifestHash = $manifestHash
    Resources = $resourceCount
    Images = $images.Count
    Evidence = $verification.Output
}
