[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^i-[0-9a-f]+$')]
    [string]$InstanceId,

    [ValidateSet('ap-northeast-1')]
    [string]$Region = 'ap-northeast-1',

    [string]$Profile = 'kanz-platform',

    [switch]$Apply,

    # Deliberately advances one harmless pod-template environment variable after the
    # exact-main apply, then exits non-zero. The EXIT trap must restore the
    # complete pre-apply controller state. This is a testnet transaction proof,
    # never a production deployment option.
    [switch]$VerifyRollback
)

$ErrorActionPreference = 'Stop'
if ($VerifyRollback -and -not $Apply) {
    throw '-VerifyRollback requires -Apply because rollback cannot be proved without a mutation.'
}
$aws = (Get-Command aws.exe -ErrorAction Stop).Source
$kubectl = (Get-Command kubectl.exe -ErrorAction Stop).Source
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$overlay = Join-Path $repoRoot 'kanz\infra\overlays\testnet-tokyo'
$podVerifierPath = Join-Path $repoRoot 'tools\Verify-TestnetWorkloadPods.jq'

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

& git -C $repoRoot fetch origin main --quiet
if ($LASTEXITCODE -ne 0) { throw 'Could not fetch origin/main.' }
$releaseCommit = (& git -C $repoRoot rev-parse origin/main).Trim()
$headCommit = (& git -C $repoRoot rev-parse HEAD).Trim()
if ($headCommit -ne $releaseCommit) {
    throw "Workload installation is refused: HEAD $headCommit is not exact origin/main $releaseCommit."
}
$provenancePaths = @(
    'tools/Install-TestnetWorkloads.ps1',
    'tools/Verify-TestnetWorkloadPods.jq',
    'kanz/infra/overlays/testnet-tokyo',
    'kanz/infra/deploy/analysis-template.yaml',
    'kanz/infra/deploy/accounting-deploy.yaml',
    'kanz/infra/deploy/api-gateway-deploy.yaml',
    'kanz/infra/deploy/audit-deploy.yaml',
    'kanz/infra/deploy/compliance-deploy.yaml',
    'kanz/infra/deploy/identity-deploy.yaml',
    'kanz/infra/deploy/oms-deploy.yaml',
    'kanz/infra/deploy/risk-engine-rollout.yaml',
    'kanz/infra/deploy/venue-binance-deploy.yaml',
    'kanz/infra/deploy/venue-okx-deploy.yaml',
    'kanz/infra/deploy/web-bff-deploy.yaml',
    'kanz/infra/security/secrets/secretproviderclass.yaml',
    'tools/Invoke-VenueSecrets.ps1',
    'tools/bootstrap-testnet-web-edge.sh'
)
& git -C $repoRoot diff --quiet $releaseCommit -- @provenancePaths
if ($LASTEXITCODE -ne 0) {
    throw 'Workload installation is refused: a rendered input differs from merged origin/main.'
}

$renderedLines = & $kubectl kustomize --load-restrictor=LoadRestrictionsNone $overlay
if ($LASTEXITCODE -ne 0 -or -not $renderedLines) {
    throw 'The Tokyo workload overlay did not render.'
}
$rendered = ($renderedLines -join "`n") + "`n"
$resourceCount = ([regex]::Matches($rendered, '(?m)^kind: ')).Count
if ($resourceCount -ne 61) {
    throw "Expected 61 rendered resources; found $resourceCount."
}
$images = [regex]::Matches($rendered, '(?m)^\s*image:\s+(\S+)\s*$')
if ($images.Count -ne 18) { throw "Expected 18 rendered container images; found $($images.Count)." }
foreach ($image in $images) {
    if ($image.Groups[1].Value -notmatch '^012619468098\.dkr\.ecr\.ap-northeast-1\.amazonaws\.com/[a-z0-9-]+@sha256:[0-9a-f]{64}$') {
        throw "Rendered workload image is outside the immutable Tokyo ECR boundary: $($image.Groups[1].Value)"
    }
}
$releaseAnnotations = [regex]::Matches($rendered, '(?m)^\s*kanz\.io/release-commit:\s+([0-9a-f]{40})\s*$')
if ($releaseAnnotations.Count -eq 0) {
    throw 'Rendered workloads do not declare a release commit.'
}
$imageReleaseCommit = $releaseAnnotations[0].Groups[1].Value
if ($releaseAnnotations | Where-Object { $_.Groups[1].Value -ne $imageReleaseCommit }) {
    throw 'Rendered workloads contain inconsistent release commits.'
}
$uniqueImages = $images | ForEach-Object { $_.Groups[1].Value } | Sort-Object -Unique
foreach ($imageRef in $uniqueImages) {
    if ($imageRef -notmatch '^012619468098\.dkr\.ecr\.ap-northeast-1\.amazonaws\.com/(?<repository>[a-z0-9-]+)@(?<digest>sha256:[0-9a-f]{64})$') {
        throw "Cannot inspect malformed Tokyo ECR image: $imageRef"
    }
    $repository = $Matches.repository
    $digest = $Matches.digest
    $releaseDigest = (& $aws ecr describe-images --profile $Profile --region $Region `
        --repository-name $repository --image-ids "imageTag=$imageReleaseCommit" `
        --query 'imageDetails[0].imageDigest' --output text 2>$null).Trim()
    if ($LASTEXITCODE -ne 0 -or $releaseDigest -ne $digest) {
        throw "Tokyo ECR does not retain $repository@$digest under release $imageReleaseCommit."
    }
}
foreach ($forbidden in @('ghcr.io', 'ghcr-pull', 'imagePullSecrets:', 'kubernetes.io/dockerconfigjson')) {
    if ($rendered.Contains($forbidden)) { throw "Rendered workload contains forbidden marker: $forbidden" }
}
foreach ($required in @(
    'kind: AnalysisTemplate', 'name: risk-engine-canary',
    'address: http://prometheus.kanz-observability.svc:9090',
    'name: accounting-db', 'name: api-gateway-redis', 'name: api-gateway-secrets',
    'name: audit-db', 'name: identity-db', 'name: identity-signing-key',
    'name: oms-db', 'name: risk-engine-db', 'name: venue-binance-db', 'name: web-bff-secrets',
    'name: venue-binance-keys', 'name: venue-okx-db', 'name: venue-okx-keys'
)) {
    if (-not $rendered.Contains($required)) { throw "Rendered workload is missing prerequisite: $required" }
}

$podVerifier = Get-Content -LiteralPath $podVerifierPath -Raw
if ([string]::IsNullOrWhiteSpace($podVerifier)) {
    throw 'The workload Pod verifier is empty.'
}
$podVerifierBytes = [Text.Encoding]::UTF8.GetBytes($podVerifier)
$podVerifierPayload = [Convert]::ToBase64String($podVerifierBytes)

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
$remoteBase = "/var/tmp/kanz-workloads-$transferId"
$remotePayload = "$remoteBase.b64"
$remoteManifest = "$remoteBase.yaml"
$remotePodVerifier = "$remoteBase.jq"
$remoteRollback = "$remoteBase.rollback.json"
$remoteRollbackVerify = "$remoteBase.rollback-verify.json"
$remotePolicyRollback = "$remoteBase.policy-rollback.json"
Invoke-SsmCommands -Comment 'Initialize Kanz testnet workload transfer' -TimeoutSeconds 60 -Commands @(
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
    Invoke-SsmCommands -Comment "Stage Kanz workload manifest $($batchStart + 1)-$batchEnd/$($chunks.Count)" `
        -TimeoutSeconds 60 -Commands $commands | Out-Null
}
Invoke-SsmCommands -Comment 'Stage Kanz workload Pod verifier' -TimeoutSeconds 60 -Commands @(
    'set -eu',
    "umask 077; printf '%s' '$podVerifierPayload' | base64 -d > '$remotePodVerifier'"
) | Out-Null

$applyFlag = if ($Apply) { '1' } else { '0' }
$rollbackProbeFlag = if ($VerifyRollback) { '1' } else { '0' }
$verification = Invoke-SsmCommands -Comment 'Verify or install Kanz Tokyo workloads' -TimeoutSeconds 2400 -Commands @(
    'set -Eeuo pipefail',
    "payload='$remotePayload'",
    "manifest='$remoteManifest'",
    "pod_filter='$remotePodVerifier'",
    "rollback='$remoteRollback'",
    "rollback_verify='$remoteRollbackVerify'",
    "policy_rollback='$remotePolicyRollback'",
    "rollback_probe='$rollbackProbeFlag'",
    "rollback_probe_id='$transferId'",
    'applied=0',
    'fresh=0',
    'policy_preexisting=0',
    'web_bff_preexisting=0',
    'capture_workloads() { /usr/local/bin/k3s kubectl -n kanz-services get deployment/identity deployment/compliance deployment/accounting deployment/audit deployment/oms deployment/venue-binance deployment/venue-okx deployment/api-gateway deployment/web-bff rollout.argoproj.io/risk-engine --ignore-not-found=true -o json | jq -S -c ''{apiVersion:"v1",kind:"List",items:[.items[] | {apiVersion,kind,metadata:{name:.metadata.name,namespace:.metadata.namespace,annotations:((.metadata.annotations // {}) | with_entries(select(.key | startswith("kanz.io/"))))},spec:.spec}] | sort_by(.kind,.metadata.name)}''; }',
    'rollback_workloads() { rollback_apply_ok=1; while IFS=$''\t'' read -r kind name; do if ! current=$(/usr/local/bin/k3s kubectl -n kanz-services get "${kind}/${name}" -o json); then rollback_apply_ok=0; continue; fi; if ! desired=$(jq -ce --arg kind "${kind}" --arg name "${name}" ''.items[] | select(.kind == $kind and .metadata.name == $name)'' "${rollback}"); then rollback_apply_ok=0; continue; fi; jq -cn --argjson current "${current}" --argjson desired "${desired}" ''{apiVersion:$current.apiVersion,kind:$current.kind,metadata:(($current.metadata | {name,namespace,resourceVersion,labels,annotations,finalizers,ownerReferences}) | with_entries(select(.value != null))),spec:$desired.spec} | .metadata.annotations = (((.metadata.annotations // {}) | with_entries(select((.key | startswith("kanz.io/")) | not))) + ($desired.metadata.annotations // {}))'' | /usr/local/bin/k3s kubectl replace --field-manager=kanz-bootstrap -f - >&2 || rollback_apply_ok=0; done < <(jq -r ''.items[] | [.kind,.metadata.name] | @tsv'' "${rollback}"); [[ "${rollback_apply_ok}" = 1 ]]; }',
    'deployment_restored() { name=$1; deadline=$((SECONDS+600)); while (( SECONDS < deadline )); do if /usr/local/bin/k3s kubectl -n kanz-services get "deployment/${name}" -o json | jq -e ''.status.observedGeneration >= .metadata.generation and (.status.updatedReplicas // 0) == .spec.replicas and (.status.availableReplicas // 0) == .spec.replicas and (.status.readyReplicas // 0) == .spec.replicas'' >/dev/null; then echo "deployment/${name} restored" >&2; return 0; fi; sleep 2; done; echo "deployment/${name} did not restore its current generation" >&2; return 1; }',
    'finish() { rc=$?; set +e; if [[ "${rc}" != 0 && "${applied}" = 1 ]]; then if [[ "${fresh}" = 1 ]]; then echo fresh-install-rollback-started >&2; /usr/local/bin/k3s kubectl delete --ignore-not-found=true --wait=true -f "${manifest}" >&2 || echo fresh-install-rollback-failed >&2; elif [[ -s "${rollback}" ]]; then echo update-rollback-started >&2; rollback_ok=1; rollback_workloads || rollback_ok=0; if [[ "${web_bff_preexisting}" = 0 ]]; then /usr/local/bin/k3s kubectl -n kanz-services delete deployment/web-bff --ignore-not-found=true --wait=true >&2 || rollback_ok=0; fi; if [[ "${policy_preexisting}" = 1 ]]; then policy_restored=0; for attempt in 1 2 3 4 5; do /usr/local/bin/k3s kubectl apply --server-side --force-conflicts --field-manager=kanz-bootstrap -f "${policy_rollback}" >&2 && { policy_restored=1; break; }; sleep 1; done; [[ "${policy_restored}" = 1 ]] || rollback_ok=0; else /usr/local/bin/k3s kubectl -n kanz-services delete networkpolicy allow-gateway-to-oms-query --ignore-not-found=true >&2 || rollback_ok=0; fi; for deployment in identity compliance accounting audit oms venue-binance venue-okx api-gateway; do deployment_restored "${deployment}" || rollback_ok=0; done; if [[ "${web_bff_preexisting}" = 1 ]]; then deployment_restored web-bff || rollback_ok=0; fi; /usr/local/bin/k3s kubectl -n kanz-services wait --for=jsonpath=''{.status.phase}''=Healthy rollout/risk-engine --timeout=900s >&2 || rollback_ok=0; capture_workloads > "${rollback_verify}" || rollback_ok=0; if cmp -s "${rollback}" "${rollback_verify}"; then echo update-rollback-state-exact >&2; else echo update-rollback-state-mismatch >&2; rollback_ok=0; fi; if [[ "${rollback_ok}" = 1 ]]; then echo update-rollback-complete >&2; else echo update-rollback-failed >&2; fi; fi; fi; rm -f "${payload}" "${manifest}" "${pod_filter}" "${rollback}" "${rollback_verify}" "${policy_rollback}"; exit "${rc}"; }',
    'trap finish EXIT',
    'base64 -d "${payload}" | gzip -d > "${manifest}"',
    ("printf '%s  %s\n' '$manifestHash' " + '"${manifest}" | sha256sum --check --status'),
    ('test "$(grep -c ''^kind:'' "${manifest}")" = ''' + $resourceCount + "'"),
    'test "$(grep -Ec ''^[[:space:]]*image: 012619468098\.dkr\.ecr\.ap-northeast-1\.amazonaws\.com/[a-z0-9-]+@sha256:[0-9a-f]{64}$'' "${manifest}")" = 18',
    '! grep -Eq ''ghcr\.io|ghcr-pull|imagePullSecrets:|kubernetes\.io/dockerconfigjson'' "${manifest}"',
    '/usr/local/bin/k3s kubectl get namespace kanz-services >/dev/null',
    '/usr/local/bin/k3s kubectl -n kanz-data wait --for=condition=Ready cluster/kanz-testnet-postgres --timeout=60s >/dev/null',
    '/usr/local/bin/k3s kubectl -n kanz-data wait --for=condition=complete job/postgres-migrations --timeout=60s >/dev/null',
    '/usr/local/bin/k3s kubectl -n kanz-messaging rollout status statefulset/nats --timeout=60s >/dev/null',
    '/usr/local/bin/k3s kubectl -n kanz-messaging rollout status statefulset/redis --timeout=60s >/dev/null',
    '/usr/local/bin/k3s kubectl -n cnpg-system wait --for=condition=Available deployment/cnpg-controller-manager --timeout=60s >/dev/null',
    '/usr/local/bin/k3s kubectl -n argo-rollouts wait --for=condition=Available deployment/argo-rollouts --timeout=60s >/dev/null',
    '/usr/local/bin/k3s kubectl apply --server-side --force-conflicts --dry-run=server --field-manager=kanz-bootstrap -f "${manifest}" >/dev/null',
    'echo workload-server-dry-run-ok',
    "apply='$applyFlag'",
    'if [[ "${apply}" = 0 ]]; then exit 0; fi',
    '/usr/local/bin/k3s kubectl get namespace kanz-observability >/dev/null',
    '/usr/local/bin/k3s kubectl -n kanz-observability get service/prometheus >/dev/null',
    '/usr/local/bin/k3s kubectl -n kanz-observability get endpointslice -l kubernetes.io/service-name=prometheus -o json | jq -e ''[.items[].endpoints[]? | select(.conditions.ready != false) | .addresses[]?] | length > 0'' >/dev/null',
    'prometheus_cluster_ip=$(/usr/local/bin/k3s kubectl -n kanz-observability get service/prometheus -o jsonpath=''{.spec.clusterIP}'')',
    'curl -fsS --max-time 5 "http://${prometheus_cluster_ip}:9090/-/ready" >/dev/null',
    'echo workload-analysis-provider-ready',
    'accounting_exists=0; risk_exists=0',
    'if /usr/local/bin/k3s kubectl -n kanz-services get deployment/accounting >/dev/null 2>&1; then accounting_exists=1; fi',
    'if /usr/local/bin/k3s kubectl -n kanz-services get rollout/risk-engine >/dev/null 2>&1; then risk_exists=1; fi',
    'if [[ "${accounting_exists}" != "${risk_exists}" ]]; then echo workload installation refused: partial managed state >&2; exit 1; fi',
    'if [[ "${accounting_exists}" = 0 ]]; then fresh=1; fi',
    'if [[ "${fresh}" = 0 ]]; then /usr/local/bin/k3s kubectl -n kanz-services get deployment/identity deployment/compliance deployment/accounting deployment/audit deployment/oms deployment/venue-binance deployment/venue-okx deployment/api-gateway rollout.argoproj.io/risk-engine >/dev/null; if /usr/local/bin/k3s kubectl -n kanz-services get deployment/web-bff >/dev/null 2>&1; then web_bff_preexisting=1; fi; capture_workloads > "${rollback}"; test "$(jq ''.items | length'' "${rollback}")" -ge 9; fi',
    'if [[ "${rollback_probe}" = 1 ]] && { [[ "${fresh}" = 1 ]] || [[ "${web_bff_preexisting}" = 0 ]] || [[ "$(jq ''.items | length'' "${rollback}")" != 10 ]]; }; then echo workload rollback probe refused: complete preexisting estate required >&2; exit 1; fi',
    'if [[ "${fresh}" = 0 ]] && /usr/local/bin/k3s kubectl -n kanz-services get networkpolicy/allow-gateway-to-oms-query -o json > "${policy_rollback}" 2>/dev/null; then policy_preexisting=1; fi',
    'applied=1',
    '/usr/local/bin/k3s kubectl apply --server-side --force-conflicts --field-manager=kanz-bootstrap -f "${manifest}" >/dev/null',
    'if [[ "${rollback_probe}" = 1 ]]; then probe_generation_before=$(/usr/local/bin/k3s kubectl -n kanz-services get deployment/identity -o jsonpath=''{.metadata.generation}''); /usr/local/bin/k3s kubectl -n kanz-services set env deployment/identity KANZ_ROLLBACK_PROBE="${rollback_probe_id}" >/dev/null; probe_generation_after=$(/usr/local/bin/k3s kubectl -n kanz-services get deployment/identity -o jsonpath=''{.metadata.generation}''); test "${probe_generation_after}" -gt "${probe_generation_before}"; /usr/local/bin/k3s kubectl -n kanz-services rollout status deployment/identity --timeout=600s >/dev/null; echo workload-rollback-probe-advanced >&2; exit 86; fi',
    'for deployment in identity compliance accounting audit oms venue-binance venue-okx api-gateway web-bff; do /usr/local/bin/k3s kubectl -n kanz-services rollout status "deployment/${deployment}" --timeout=600s >/dev/null; done',
    '/usr/local/bin/k3s kubectl -n kanz-services wait --for=jsonpath=''{.status.phase}''=Healthy rollout/risk-engine --timeout=900s >/dev/null',
    'pods_ready=0; for attempt in $(seq 1 120); do pods=$(/usr/local/bin/k3s kubectl -n kanz-services get pods -l app.kubernetes.io/part-of=kanz -o json); if jq -e -f "${pod_filter}" <<<"${pods}" >/dev/null; then pods_ready=1; break; fi; sleep 1; done; test "${pods_ready}" = 1',
    'test "$(/usr/local/bin/k3s kubectl -n kanz-services get secret -o json | jq ''[.items[] | select(.type=="kubernetes.io/dockerconfigjson")] | length'')" = 0',
    'echo workload-rollouts-ready',
    'echo workload-images-match-reviewed-ecr-lock',
    'echo workload-registry-secrets-absent'
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
