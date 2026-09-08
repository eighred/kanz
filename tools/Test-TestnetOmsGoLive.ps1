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
    'jq -n --slurpfile deployment "${base}-deployment.json" --slurpfile pods "${base}-pods.json" --rawfile recent_logs "${base}-logs.txt" ''{deployment: $deployment[0], pods: $pods[0], recent_logs: $recent_logs}'' | jq -e -f "${base}.jq"'
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

if (-not $result -or $result.Status -ne 'Success') {
    $detail = if ($result) { $result.Error.Trim() } else { 'no terminal SSM result' }
    throw "OMS go-live verification $commandId refused resume: $detail"
}
Write-Output "SSM command: $commandId"
Write-Output $result.Output.Trim()
