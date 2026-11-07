[CmdletBinding()]
param([string]$Profile = 'kanz-platform')

$ErrorActionPreference = 'Stop'
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$aws = (Get-Command aws.exe -ErrorAction Stop).Source
$python = (Get-Command python.exe -ErrorAction Stop).Source
$verifierPath = Join-Path $PSScriptRoot 'cni-proof/proof.py'
$networkPath = Join-Path $PSScriptRoot 'gateway-policy-proof/network_observation.py'
$Region = 'ap-northeast-1'
$InstanceId = 'i-0ee1235614219f089'
$AccountId = '012619468098'

& git -C $repoRoot fetch origin main --quiet
if ($LASTEXITCODE -ne 0) { throw 'Could not fetch origin/main.' }
$releaseCommit = (& git -C $repoRoot rev-parse origin/main).Trim()
$headCommit = (& git -C $repoRoot rev-parse HEAD).Trim()
if ($headCommit -ne $releaseCommit) {
    throw 'CNI proof is refused: HEAD is not exact origin/main.'
}
& git -C $repoRoot ls-files --error-unmatch tools/Test-TestnetCNI.ps1 tools/cni-proof/proof.py tools/gateway-policy-proof/network_observation.py *> $null
if ($LASTEXITCODE -ne 0) { throw 'CNI verifier inputs must be tracked.' }
& git -C $repoRoot diff --quiet $releaseCommit -- tools/Test-TestnetCNI.ps1 tools/cni-proof/proof.py tools/gateway-policy-proof/network_observation.py
if ($LASTEXITCODE -ne 0) { throw 'CNI verifier inputs differ from merged origin/main.' }

# No SSM command is issued before source and fixed estate identity checks.
$identity = & $aws sts get-caller-identity --profile $Profile --region $Region --output json 2>$null | ConvertFrom-Json
if ($LASTEXITCODE -ne 0 -or $identity.Account -ne $AccountId) { throw 'Unexpected AWS account.' }
$instance = & $aws ec2 describe-instances --profile $Profile --region $Region --instance-ids $InstanceId `
    --query 'Reservations[0].Instances[0].{Id:InstanceId,State:State.Name}' --output json 2>$null | ConvertFrom-Json
if ($LASTEXITCODE -ne 0 -or $instance.Id -ne $InstanceId -or $instance.State -ne 'running') {
    throw 'The fixed Tokyo instance is not running in the expected account and region.'
}
foreach ($path in @($verifierPath, $networkPath)) {
    $info = Get-Item -LiteralPath $path
    if ($info.LinkType -or $info.PSIsContainer -or $info.Length -gt 24576) { throw 'CNI source must be bounded regular bytes.' }
}
$verifierBytes = [IO.File]::ReadAllBytes($verifierPath)
$networkBytes = [IO.File]::ReadAllBytes($networkPath)
$utf8 = [Text.UTF8Encoding]::new($false, $true)
$verifier = $utf8.GetString($verifierBytes)
$null = $utf8.GetString($networkBytes)
if (($verifierBytes.Length + $networkBytes.Length) -gt 24576) { throw 'CNI verifier exceeds the transfer bound.' }
$expectedImage = [regex]::Match($verifier, '(?m)^IMAGE = "([^"]+)"\r?$').Groups[1].Value
if ($expectedImage -notmatch '^python:[a-zA-Z0-9.-]+@sha256:[0-9a-f]{64}$') { throw 'CNI image pin is absent.' }
$payload = [Convert]::ToBase64String($verifierBytes)
$networkPayload = [Convert]::ToBase64String($networkBytes)
$parameters = @{ executionTimeout = @('540'); commands = @(
    'set -eu',
    'umask 077',
    'work=$(mktemp -d /var/tmp/kanz-cni-verifier.XXXXXXXX)',
    'trap ''rm -f "$work/cni-proof/proof.py" "$work/gateway-policy-proof/network_observation.py"; rmdir "$work/cni-proof" "$work/gateway-policy-proof" "$work"'' EXIT',
    'mkdir "$work/cni-proof" "$work/gateway-policy-proof"',
    'engine="$work/cni-proof/proof.py"',
    'set -C',
    ('printf ''%s'' ''' + $networkPayload + ''' | base64 -d > "$work/gateway-policy-proof/network_observation.py"'),
    ('printf ''%s'' ''' + $payload + ''' | base64 -d > "$engine"'),
    ('timeout --signal=TERM --kill-after=70s 400s python3 -I -B "$engine" --tokyo --source ' + $releaseCommit)
) } | ConvertTo-Json -Compress -Depth 4
if ([Text.Encoding]::UTF8.GetByteCount($parameters) -gt 30000 -or $parameters.Length -gt 30000) { throw 'CNI complete command exceeds native bound.' }
$commandId = (& $aws ssm send-command --profile $Profile --region $Region --instance-ids $InstanceId `
    --document-name AWS-RunShellScript --comment "Kanz isolated CNI proof $releaseCommit" `
    --parameters $parameters --query Command.CommandId --output text 2>$null).Trim()
if ($LASTEXITCODE -ne 0 -or $commandId -notmatch '^[0-9a-f-]{36}$') { throw 'SSM refused CNI verification.' }
$deadline = [DateTime]::UtcNow.AddSeconds(560)
$result = $null
do {
    Start-Sleep -Seconds 2
    $raw = & $aws ssm get-command-invocation --profile $Profile --region $Region `
        --command-id $commandId --instance-id $InstanceId `
        --query '{Status:Status,Output:StandardOutputContent}' --output json 2>$null
    if ($LASTEXITCODE -eq 0) { $result = $raw | ConvertFrom-Json }
    if ($result -and $result.Status -in @('Success', 'Cancelled', 'Failed', 'TimedOut')) { break }
} while ([DateTime]::UtcNow -lt $deadline)
if (-not $result -or $result.Status -ne 'Success') {
    throw 'CNI proof did not complete successfully; cleanup is not certified. Inspect only the generated fixture namespace before retrying.'
}
$result.Output | & $python -I -B $verifierPath --verify-report --source $releaseCommit 2>$null
if ($LASTEXITCODE -ne 0) { throw 'CNI evidence violates the canonical case/image contract.' }
try { $report = $result.Output.Trim() | ConvertFrom-Json } catch { throw 'CNI proof returned no valid evidence.' }
if ($report.format -ne 'kanz-cni-proof-v1' -or $report.source -ne $releaseCommit -or
    $report.image -ne $expectedImage -or $report.namespace -notmatch '^kanz-cni-proof-[0-9a-f]{32}$' -or
    $report.verdict -ne 'PASS' -or $report.cleanup -ne 'verified_absent' -or
    @($report.cases).Count -ne 16 -or @($report.cases | Where-Object verdict -ne 'PASS').Count -ne 0) {
    throw 'CNI evidence is incomplete or failed; no policy coverage is certified.'
}
# Emit only the source, immutable fixture image, namespace and measured verdicts.
[ordered]@{ source = $report.source; image = $report.image; namespace = $report.namespace
    images = $report.images
    cases = @($report.cases | Select-Object case, verdict, observation)
    cleanup = $report.cleanup; verdict = $report.verdict } | ConvertTo-Json -Depth 5 -Compress
