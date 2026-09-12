[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^i-[0-9a-f]+$')]
    [string]$InstanceId,

    [ValidateSet('ap-northeast-1')]
    [string]$Region = 'ap-northeast-1',

    [string]$Profile = 'kanz-platform'
)

$ErrorActionPreference = 'Stop'
$aws = (Get-Command aws.exe -ErrorAction Stop).Source
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$templatePath = Join-Path $PSScriptRoot 'TestnetPortfolioBootstrapJob.yaml'
$expectedBindings = '__system__/PF1@XBIN=binance-main,__system__/PF1@XOKX=okx-sub-1'

& git -C $repoRoot fetch origin main --quiet
if ($LASTEXITCODE -ne 0) { throw 'Could not fetch origin/main.' }
$releaseCommit = (& git -C $repoRoot rev-parse origin/main).Trim()
$headCommit = (& git -C $repoRoot rev-parse HEAD).Trim()
if ($headCommit -ne $releaseCommit) {
    throw "Portfolio bootstrap is refused: HEAD $headCommit is not exact origin/main $releaseCommit."
}
& git -C $repoRoot diff --quiet $releaseCommit -- tools/Initialize-TestnetPortfolio.ps1 tools/TestnetPortfolioBootstrapJob.yaml kanz/test/live/capitalpath kanz/infra/nats/tenancy.yaml kanz/infra/overlays/testnet-tokyo
if ($LASTEXITCODE -ne 0) { throw 'Portfolio bootstrap inputs differ from merged origin/main.' }

$digest = (& $aws ecr describe-images --profile $Profile --region $Region `
    --repository-name kanz-capitalpath --image-ids "imageTag=$releaseCommit" `
    --query 'imageDetails[0].imageDigest' --output text 2>$null).Trim()
if ($LASTEXITCODE -ne 0 -or $digest -notmatch '^sha256:[0-9a-f]{64}$') {
    throw "The signed kanz-capitalpath image for $releaseCommit is not available in Tokyo ECR."
}
$image = "012619468098.dkr.ecr.ap-northeast-1.amazonaws.com/kanz-capitalpath@$digest"
$manifest = (Get-Content -LiteralPath $templatePath -Raw).Replace('KANZ_CAPITALPATH_IMAGE', $image)
if ($manifest.Contains('KANZ_CAPITALPATH_IMAGE')) { throw 'The bootstrap image placeholder was not resolved.' }
$payload = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($manifest))
$remote = "/var/tmp/kanz-portfolio-bootstrap-$([Guid]::NewGuid().ToString('N')).yaml"

$parameters = @{ commands = @(
    'set -eu',
    "manifest='$remote'",
    'cleanup() { /usr/local/bin/k3s kubectl -n kanz-services delete job kanz-portfolio-bootstrap --ignore-not-found --wait=false >/dev/null 2>&1 || true; rm -f "${manifest}"; }',
    'trap cleanup EXIT',
    ("printf '%s' '$payload' | base64 -d > " + '"${manifest}"'),
    '/usr/local/bin/k3s kubectl -n kanz-services get serviceaccount kanz-portfolio-bootstrap >/dev/null',
    'live_oms=$(/usr/local/bin/k3s kubectl -n kanz-services get deployment oms -o json | jq -r ''[.spec.template.spec.containers[] | select(.name=="oms") | .env[]? | select(.name=="OMS_VENUE_ACCOUNTS") | .value][0] // ""'')',
    'live_risk=$(/usr/local/bin/k3s kubectl -n kanz-services get rollout risk-engine -o json | jq -r ''[.spec.template.spec.containers[] | select(.name=="risk-engine") | .env[]? | select(.name=="RISK_ENGINE_VENUE_ACCOUNTS") | .value][0] // ""'')',
    ('test "$live_oms" = ''{0}'' || {{ echo ''REFUSED: live OMS binding does not match exact main'' >&2; exit 2; }}' -f $expectedBindings),
    ('test "$live_risk" = ''{0}'' || {{ echo ''REFUSED: live risk binding does not match exact main'' >&2; exit 2; }}' -f $expectedBindings),
    'pgpod=$(/usr/local/bin/k3s kubectl -n kanz-data get pod -l cnpg.io/cluster=kanz-testnet-postgres,role=primary -o jsonpath=''{.items[0].metadata.name}'')',
    'existing=$(/usr/local/bin/k3s kubectl -n kanz-data exec "${pgpod}" -c postgres -- psql -U postgres -d risk_engine -Atqc "select count(*) from portfolios where tenant_id=''''__system__'''' and portfolio_id=''''PF1''''")',
    'total=$(/usr/local/bin/k3s kubectl -n kanz-data exec "${pgpod}" -c postgres -- psql -U postgres -d risk_engine -Atqc ''select count(*) from portfolios'')',
    'if [ "${existing}" = 1 ] && [ "${total}" = 1 ]; then echo ''portfolio-bootstrap: PF1 already materialized; no event republished''; exit 0; fi',
    'test "${existing}" = 0 -a "${total}" = 0 || { echo "REFUSED: portfolio inventory is not empty and is not the reviewed singleton PF1" >&2; exit 2; }',
    '/usr/local/bin/k3s kubectl apply --server-side --field-manager=kanz-portfolio-bootstrap -f "${manifest}" >/dev/null',
    '/usr/local/bin/k3s kubectl -n kanz-services wait --for=condition=complete job/kanz-portfolio-bootstrap --timeout=180s >/dev/null || { /usr/local/bin/k3s kubectl -n kanz-services logs job/kanz-portfolio-bootstrap >&2 || true; exit 1; }',
    '/usr/local/bin/k3s kubectl -n kanz-services logs job/kanz-portfolio-bootstrap',
    'i=0; while [ "${i}" -lt 30 ]; do materialized=$(/usr/local/bin/k3s kubectl -n kanz-data exec "${pgpod}" -c postgres -- psql -U postgres -d risk_engine -Atqc "select count(*) from portfolios where tenant_id=''''__system__'''' and portfolio_id=''''PF1''''"); [ "${materialized}" = 1 ] && break; i=$((i+1)); sleep 1; done',
    'test "${materialized:-0}" = 1 || { echo ''portfolio-bootstrap: typed event was not materialized'' >&2; exit 1; }',
    'echo ''portfolio-bootstrap: VERIFIED typed event materialized as __system__/PF1; no order, mandate, or platform-mode subject was published'''
) } | ConvertTo-Json -Compress -Depth 4

$commandId = (& $aws ssm send-command --profile $Profile --region $Region `
    --instance-ids $InstanceId --document-name AWS-RunShellScript `
    --comment 'Bootstrap reviewed Kanz testnet portfolio' --parameters $parameters `
    --query 'Command.CommandId' --output text).Trim()
if ($LASTEXITCODE -ne 0 -or -not $commandId) { throw 'AWS rejected the portfolio bootstrap command.' }
$deadline = [DateTime]::UtcNow.AddMinutes(5)
do {
    Start-Sleep -Milliseconds 750
    $result = & $aws ssm get-command-invocation --profile $Profile --region $Region `
        --command-id $commandId --instance-id $InstanceId `
        --query '{Status:Status,Output:StandardOutputContent,Error:StandardErrorContent}' --output json 2>$null | ConvertFrom-Json
    if ($result.Status -in @('Success','Cancelled','Failed','TimedOut')) { break }
} while ([DateTime]::UtcNow -lt $deadline)
Write-Output "SSM command: $commandId"
if ($result.Output) { Write-Output $result.Output.Trim() }
if ($result.Status -ne 'Success') { throw "Portfolio bootstrap failed: $($result.Error.Trim())" }
