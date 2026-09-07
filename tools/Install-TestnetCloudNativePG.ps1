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
$manifestDir = Join-Path $PSScriptRoot '..\kanz\infra\controllers\cloudnative-pg'
$lock = Get-Content -Raw -LiteralPath (Join-Path $manifestDir 'release-lock.json') | ConvertFrom-Json

if ($lock.version -notmatch '^v[0-9]+\.[0-9]+\.[0-9]+$' -or
    $lock.manifestSHA256 -notmatch '^[0-9a-f]{64}$' -or
    $lock.controllerImage -notmatch '^ghcr\.io/cloudnative-pg/cloudnative-pg@sha256:[0-9a-f]{64}$' -or
    $lock.manifestURL -ne "https://github.com/cloudnative-pg/cloudnative-pg/releases/download/$($lock.version)/cnpg-$($lock.version.TrimStart('v')).yaml") {
    throw 'The CloudNativePG release lock is malformed or inconsistent.'
}

function ConvertTo-Base64File([string]$Path) {
    return [Convert]::ToBase64String([IO.File]::ReadAllBytes($Path))
}

$fileCommands = @()
foreach ($name in @('kustomization.yaml', 'network-policy.yaml', 'pdb.yaml', 'deployment-patch.yaml')) {
    $encoded = ConvertTo-Base64File (Join-Path $manifestDir $name)
    $fileCommands += "printf '%s' '$encoded' | base64 -d > `"`${work}/$name`""
}

$applyFlag = if ($Apply) { '1' } else { '0' }
$commands = @(
    'set -Eeuo pipefail',
    'work=$(mktemp -d /var/tmp/kanz-cnpg.XXXXXX)',
    'cleanup() { rm -rf "${work}"; }',
    'trap cleanup EXIT',
    "manifest_url='$($lock.manifestURL)'",
    "expected_manifest_sha='$($lock.manifestSHA256)'",
    "expected_image='$($lock.controllerImage)'",
    "apply='$applyFlag'"
) + $fileCommands + @(
    'curl --fail --silent --show-error --location --proto ''=https'' --tlsv1.2 "${manifest_url}" --output "${work}/upstream-install.yaml"',
    'printf ''%s  %s\n'' "${expected_manifest_sha}" "${work}/upstream-install.yaml" | sha256sum --check --status',
    '/usr/local/bin/k3s kubectl kustomize "${work}" > "${work}/rendered.yaml"',
    'test "$(grep -c ''^kind: CustomResourceDefinition$'' "${work}/rendered.yaml")" = 11',
    'grep -Fq "image: ${expected_image}" "${work}/rendered.yaml"',
    'if grep -Eq ''image: .*(:latest|:v?[0-9]+\.[0-9]+)'' "${work}/rendered.yaml"; then echo ''mutable controller image survived render'' >&2; exit 42; fi',
    'validation_manifest="${work}/rendered.yaml"',
    'if ! /usr/local/bin/k3s kubectl get namespace cnpg-system >/dev/null 2>&1; then',
    '  if [[ "${apply}" = 1 ]]; then',
    '    /usr/local/bin/k3s kubectl create namespace cnpg-system --dry-run=client -o yaml | /usr/local/bin/k3s kubectl apply --server-side --field-manager=kanz-bootstrap -f - >/dev/null',
    '    /usr/local/bin/k3s kubectl wait --for=jsonpath=''{.status.phase}''=Active namespace/cnpg-system --timeout=60s >/dev/null',
    '  else',
    '    sed ''s/namespace: cnpg-system/namespace: default/g'' "${work}/rendered.yaml" > "${work}/validation.yaml"',
    '    validation_manifest="${work}/validation.yaml"',
    '    echo cloudnative-pg-server-dry-run-namespace-substitute=default',
    '  fi',
    'fi',
    '/usr/local/bin/k3s kubectl apply --server-side --dry-run=server --field-manager=kanz-bootstrap -f "${validation_manifest}" >/dev/null',
    'echo cloudnative-pg-server-dry-run-ok',
    'if [[ "${apply}" = 0 ]]; then exit 0; fi',
    '/usr/local/bin/k3s kubectl apply --server-side --field-manager=kanz-bootstrap -f "${work}/rendered.yaml" >/dev/null',
    'for crd in clusters.postgresql.cnpg.io backups.postgresql.cnpg.io scheduledbackups.postgresql.cnpg.io databases.postgresql.cnpg.io databaseroles.postgresql.cnpg.io; do /usr/local/bin/k3s kubectl wait --for=condition=Established "crd/${crd}" --timeout=180s >/dev/null; done',
    '/usr/local/bin/k3s kubectl -n cnpg-system rollout status deployment/cnpg-controller-manager --timeout=240s >/dev/null',
    'test "$(/usr/local/bin/k3s kubectl -n cnpg-system get deployment cnpg-controller-manager -o jsonpath=''{.spec.template.spec.containers[?(@.name=="manager")].image}'')" = "${expected_image}"',
    'echo cloudnative-pg-controller-available',
    'echo cloudnative-pg-image-digest-locked'
)

$parameters = @{ commands = $commands } | ConvertTo-Json -Compress -Depth 4
$commandId = (& $aws ssm send-command --profile $Profile --region $Region --instance-ids $InstanceId `
    --document-name AWS-RunShellScript --comment 'Verify or install pinned CloudNativePG for Kanz testnet' `
    --parameters $parameters --query 'Command.CommandId' --output text).Trim()
if ($LASTEXITCODE -ne 0 -or -not $commandId) { throw 'AWS rejected the SSM CloudNativePG command.' }

$result = $null
for ($attempt = 0; $attempt -lt 120; $attempt++) {
    Start-Sleep -Seconds 5
    $result = & $aws ssm get-command-invocation --profile $Profile --region $Region `
        --command-id $commandId --instance-id $InstanceId `
        --query '{Status:Status,Output:StandardOutputContent,Error:StandardErrorContent}' --output json | ConvertFrom-Json
    if ($LASTEXITCODE -eq 0 -and $result.Status -notin @('Pending', 'InProgress', 'Delayed')) { break }
}
if (-not $result -or $result.Status -in @('Pending', 'InProgress', 'Delayed')) {
    throw "SSM command $commandId remained non-terminal for 10 minutes."
}
if ($result.Status -ne 'Success') { throw "CloudNativePG verification/installation failed: $($result.Error)" }

[pscustomobject]@{
    CommandId = $commandId
    Mode      = if ($Apply) { 'Apply' } else { 'ServerDryRun' }
    Status    = $result.Status
    Evidence  = $result.Output.Trim()
}
