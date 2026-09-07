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
$manifestDir = Join-Path $PSScriptRoot '..\kanz\infra\controllers\argo-rollouts'
$lockPath = Join-Path $manifestDir 'release-lock.json'
$lock = Get-Content -Raw -LiteralPath $lockPath | ConvertFrom-Json

if ($lock.version -notmatch '^v[0-9]+\.[0-9]+\.[0-9]+$' -or
    $lock.sourceCommit -notmatch '^[0-9a-f]{40}$' -or
    $lock.manifestSHA256 -notmatch '^[0-9a-f]{64}$' -or
    $lock.controllerImage -notmatch '^quay\.io/argoproj/argo-rollouts@sha256:[0-9a-f]{64}$' -or
    $lock.manifestURL -ne "https://github.com/argoproj/argo-rollouts/releases/download/$($lock.version)/install.yaml") {
    throw 'The Argo Rollouts release lock is malformed or internally inconsistent.'
}

function ConvertTo-Base64File([string]$Path) {
    $bytes = [System.IO.File]::ReadAllBytes($Path)
    return [Convert]::ToBase64String($bytes)
}

$fileCommands = @()
foreach ($name in @('kustomization.yaml', 'namespace.yaml', 'pdb.yaml', 'network-policy.yaml', 'deployment-patch.yaml')) {
    $encoded = ConvertTo-Base64File (Join-Path $manifestDir $name)
    $fileCommands += "printf '%s' '$encoded' | base64 -d > `"`${work}/$name`""
}

$applyFlag = if ($Apply) { '1' } else { '0' }
$commands = @(
    'set -Eeuo pipefail',
    'work=$(mktemp -d /var/tmp/kanz-argo-rollouts.XXXXXX)',
    'cleanup() { rm -rf "${work}"; }',
    'trap cleanup EXIT',
    "expected_manifest_sha='$($lock.manifestSHA256)'",
    "expected_image='$($lock.controllerImage)'",
    "manifest_url='$($lock.manifestURL)'",
    "apply='$applyFlag'"
) + $fileCommands + @(
    'curl --fail --silent --show-error --location --proto ''=https'' --tlsv1.2 "${manifest_url}" --output "${work}/upstream-install.yaml"',
    'printf ''%s  %s\n'' "${expected_manifest_sha}" "${work}/upstream-install.yaml" | sha256sum --check --status',
    '/usr/local/bin/k3s kubectl kustomize "${work}" > "${work}/rendered.yaml"',
    'test "$(grep -c ''^kind: CustomResourceDefinition$'' "${work}/rendered.yaml")" = 5',
    'grep -Fq "image: ${expected_image}" "${work}/rendered.yaml"',
    'if grep -Eq ''image: .*(latest|:v[0-9])'' "${work}/rendered.yaml"; then echo ''mutable controller image survived render'' >&2; exit 42; fi',
    'validation_manifest="${work}/rendered.yaml"',
    'if ! /usr/local/bin/k3s kubectl get namespace argo-rollouts >/dev/null 2>&1; then',
    '  if [[ "${apply}" = 1 ]]; then',
    '    /usr/local/bin/k3s kubectl apply --server-side --field-manager=kanz-bootstrap -f "${work}/namespace.yaml" >/dev/null',
    '    /usr/local/bin/k3s kubectl wait --for=jsonpath=''{.status.phase}''=Active namespace/argo-rollouts --timeout=60s >/dev/null',
    '  else',
    '    cp "${work}/kustomization.yaml" "${work}/kustomization.intended.yaml"',
    '    sed -i ''s/^namespace: argo-rollouts$/namespace: default/'' "${work}/kustomization.yaml"',
    '    /usr/local/bin/k3s kubectl kustomize "${work}" > "${work}/validation.yaml"',
    '    mv "${work}/kustomization.intended.yaml" "${work}/kustomization.yaml"',
    '    validation_manifest="${work}/validation.yaml"',
    '    echo argo-rollouts-server-dry-run-namespace-substitute=default',
    '  fi',
    'fi',
    'set +e',
    '/usr/local/bin/k3s kubectl apply --server-side --dry-run=server --field-manager=kanz-bootstrap -f "${validation_manifest}" > "${work}/dry-run.out" 2>&1',
    'dry_run_rc=$?',
    'set -e',
    'if [[ "${dry_run_rc}" != 0 ]]; then tail -n 80 "${work}/dry-run.out" >&2; exit "${dry_run_rc}"; fi',
    'echo argo-rollouts-server-dry-run-ok',
    'if [[ "${apply}" = 0 ]]; then exit 0; fi',
    '/usr/local/bin/k3s kubectl apply --server-side --field-manager=kanz-bootstrap -f "${work}/rendered.yaml" >/dev/null',
    'for crd in analysisruns.argoproj.io analysistemplates.argoproj.io clusteranalysistemplates.argoproj.io experiments.argoproj.io rollouts.argoproj.io; do /usr/local/bin/k3s kubectl wait --for=condition=Established "crd/${crd}" --timeout=120s >/dev/null; done',
    '/usr/local/bin/k3s kubectl -n argo-rollouts rollout status deployment/argo-rollouts --timeout=180s >/dev/null',
    '/usr/local/bin/k3s kubectl api-resources --api-group=argoproj.io -o name | grep -qx ''rollouts.argoproj.io''',
    'test "$(/usr/local/bin/k3s kubectl -n argo-rollouts get deployment argo-rollouts -o jsonpath=''{.spec.template.spec.containers[?(@.name=="argo-rollouts")].image}'')" = "${expected_image}"',
    'echo argo-rollouts-controller-available',
    'echo argo-rollouts-api-discoverable',
    'echo argo-rollouts-image-digest-locked'
)

$parameters = @{ commands = $commands } | ConvertTo-Json -Compress -Depth 4
$commandId = (& $aws ssm send-command `
    --profile $Profile `
    --region $Region `
    --instance-ids $InstanceId `
    --document-name AWS-RunShellScript `
    --comment 'Verify or install pinned Argo Rollouts for Kanz testnet' `
    --parameters $parameters `
    --query 'Command.CommandId' `
    --output text).Trim()
if ($LASTEXITCODE -ne 0 -or -not $commandId) {
    throw 'AWS rejected the SSM Argo Rollouts command.'
}

$result = $null
for ($attempt = 0; $attempt -lt 120; $attempt++) {
    Start-Sleep -Seconds 5
    $result = & $aws ssm get-command-invocation `
        --profile $Profile `
        --region $Region `
        --command-id $commandId `
        --instance-id $InstanceId `
        --query '{Status:Status,Output:StandardOutputContent,Error:StandardErrorContent}' `
        --output json | ConvertFrom-Json
    if ($LASTEXITCODE -eq 0 -and $result.Status -notin @('Pending', 'InProgress', 'Delayed')) {
        break
    }
}
if (-not $result -or $result.Status -in @('Pending', 'InProgress', 'Delayed')) {
    throw "SSM command $commandId remained non-terminal for 10 minutes."
}
if ($LASTEXITCODE -ne 0 -or $result.Status -ne 'Success') {
    throw "Argo Rollouts verification/installation failed: $($result.Error)"
}

[pscustomobject]@{
    CommandId = $commandId
    Mode      = if ($Apply) { 'Apply' } else { 'ServerDryRun' }
    Status    = $result.Status
    Evidence  = $result.Output.Trim()
}
