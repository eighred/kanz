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
$verifierPath = Join-Path $repoRoot 'tools\Verify-TestnetOmsGoLive.jq'

& git -C $repoRoot fetch origin main --quiet
if ($LASTEXITCODE -ne 0) { throw 'Could not fetch origin/main.' }
$releaseCommit = (& git -C $repoRoot rev-parse origin/main).Trim()
$headCommit = (& git -C $repoRoot rev-parse HEAD).Trim()
if ($headCommit -ne $releaseCommit) {
    throw "OMS go-live verification is refused: HEAD $headCommit is not exact origin/main $releaseCommit."
}
& git -C $repoRoot diff --quiet $releaseCommit -- tools/Test-TestnetOmsGoLive.ps1 tools/Verify-TestnetOmsGoLive.jq
if ($LASTEXITCODE -ne 0) {
    throw 'OMS go-live verification is refused: verifier inputs differ from merged origin/main.'
}

$verifier = Get-Content -LiteralPath $verifierPath -Raw
if ([string]::IsNullOrWhiteSpace($verifier)) { throw 'The OMS go-live verifier is empty.' }
$payload = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($verifier))
$remote = "/var/tmp/kanz-oms-go-live-$([Guid]::NewGuid().ToString('N'))"
$parameters = @{ commands = @(
    'set -eu',
    "base='$remote'",
    'finish() { rc=$?; rm -f "${base}.jq" "${base}-deployment.json" "${base}-pods.json" "${base}-logs.txt"; exit "${rc}"; }',
    'trap finish EXIT',
    ("printf '%s' '$payload' | base64 -d > " + '"${base}.jq"'),
    '/usr/local/bin/k3s kubectl -n kanz-services get deployment oms -o json > "${base}-deployment.json"',
    '/usr/local/bin/k3s kubectl -n kanz-services get pods -l app=oms -o json > "${base}-pods.json"',
    '/usr/local/bin/k3s kubectl -n kanz-services logs deployment/oms -c oms --since=2m --tail=1000 > "${base}-logs.txt" 2>&1',
    'pgpod=$(/usr/local/bin/k3s kubectl -n kanz-data get pod -l cnpg.io/cluster=kanz-testnet-postgres,role=primary -o jsonpath=''{.items[0].metadata.name}'')',
    'portfolio_count=$(/usr/local/bin/k3s kubectl -n kanz-data exec "${pgpod}" -c postgres -- psql -U postgres -d risk_engine -Atqc ''select count(*) from portfolios'')',
    'nats_ip=$(/usr/local/bin/k3s kubectl -n kanz-messaging get pod nats-0 -o jsonpath=''{.status.podIP}'')',
    'mandate_count=$(curl -fsS --connect-timeout 3 --max-time 5 "http://${nats_ip}:8222/jsz?streams=true&accounts=true" | jq ''[.account_details[]?.stream_detail[]? | select(.name == "MANDATE") | .state.messages] | if length == 1 then .[0] else error("expected one MANDATE stream") end'')',
    'oms_metrics=$(/usr/local/bin/k3s kubectl get --raw /api/v1/namespaces/kanz-services/services/http:oms:8090/proxy/metrics)',
    'report=$(jq -n --slurpfile deployment "${base}-deployment.json" --slurpfile pods "${base}-pods.json" --rawfile recent_logs "${base}-logs.txt" --argjson portfolio_count "${portfolio_count}" --argjson mandate_count "${mandate_count}" --arg oms_metrics "${oms_metrics}" ''{deployment: $deployment[0], pods: $pods[0], recent_logs: $recent_logs, portfolio_count: $portfolio_count, mandate_count: $mandate_count, oms_metrics: $oms_metrics}'' | jq -c -f "${base}.jq")',
    'printf ''%s\n'' "${report}"',
    'printf ''%s'' "${report}" | jq -e ''.verdict == "PASS"'' >/dev/null'
) } | ConvertTo-Json -Compress -Depth 4

$commandId = (& $aws ssm send-command --profile $Profile --region $Region `
    --instance-ids $InstanceId --document-name AWS-RunShellScript `
    --comment 'Verify Kanz OMS controls before testnet resume' --parameters $parameters `
    --query 'Command.CommandId' --output text).Trim()
if ($LASTEXITCODE -ne 0 -or -not $commandId) { throw 'AWS rejected the OMS go-live verification command.' }

$deadline = [DateTime]::UtcNow.AddMinutes(3)
do {
    Start-Sleep -Milliseconds 750
    $result = & $aws ssm get-command-invocation --profile $Profile --region $Region `
        --command-id $commandId --instance-id $InstanceId `
        --query '{Status:Status,Output:StandardOutputContent,Error:StandardErrorContent}' `
        --output json 2>$null | ConvertFrom-Json
    if ($result.Status -in @('Success', 'Cancelled', 'Failed', 'TimedOut')) { break }
} while ([DateTime]::UtcNow -lt $deadline)

if (-not $result) {
    throw "OMS go-live verification $commandId returned no terminal SSM result."
}
Write-Output "SSM command: $commandId"
if (-not [string]::IsNullOrWhiteSpace($result.Output)) {
    Write-Output $result.Output.Trim()
}
if ($result.Status -ne 'Success') {
    $detail = $result.Error.Trim()
    throw "OMS go-live verification $commandId refused resume: $detail"
}
