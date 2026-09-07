[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidateSet('Backup', 'Restore')]
    [string]$Operation,
    [string]$InstanceId = 'i-0ee1235614219f089',
    [ValidateSet('ap-northeast-1')]
    [string]$Region = 'ap-northeast-1',
    [string]$Profile = 'kanz-platform',
    [string]$RecoveryBucket = 'kanz-testnet-tokyo-recovery-012619468098'
)

$ErrorActionPreference = 'Stop'
$aws = (Get-Command aws.exe -ErrorAction Stop).Source
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$scriptPath = Join-Path $PSScriptRoot 'testnet-data-plane-recovery.sh'

& git -C $repoRoot fetch origin main --quiet
if ($LASTEXITCODE -ne 0) { throw 'Could not fetch origin/main.' }
$releaseCommit = (& git -C $repoRoot rev-parse origin/main).Trim()
$workingBlob = (& git -C $repoRoot hash-object $scriptPath).Trim()
$mergedBlob = (& git -C $repoRoot rev-parse "${releaseCommit}:tools/testnet-data-plane-recovery.sh" 2>$null).Trim()
if ($LASTEXITCODE -ne 0 -or $workingBlob -ne $mergedBlob) {
    throw 'Recovery is refused: the local recovery script is not the exact blob merged into origin/main.'
}

$kmsArn = (& $aws kms describe-key --profile $Profile --region ap-northeast-3 --key-id alias/kanz-testnet-tokyo-recovery --query KeyMetadata.Arn --output text).Trim()
if ($LASTEXITCODE -ne 0 -or $kmsArn -notmatch '^arn:aws:kms:ap-northeast-3:012619468098:key/') {
    throw 'Could not resolve the bounded Osaka recovery KMS key.'
}

$raw = [IO.File]::ReadAllText($scriptPath).Replace([string]([char]13) + [char]10, [string][char]10)
$encoded = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($raw))
$remote = '/var/tmp/kanz-testnet-data-plane-recovery.sh'
$commands = @(
    'set -Eeuo pipefail',
    "printf '%s' '$encoded' | base64 -d > '$remote'",
    "chmod 0700 '$remote'",
    "'$remote' '$($Operation.ToLowerInvariant())' '$RecoveryBucket' '$kmsArn' '$releaseCommit'"
)
$parameters = @{ commands = $commands } | ConvertTo-Json -Compress -Depth 4
$commandId = (& $aws ssm send-command --profile $Profile --region $Region --instance-ids $InstanceId --document-name AWS-RunShellScript --comment "Kanz data-plane $Operation from merged $releaseCommit" --parameters $parameters --query Command.CommandId --output text).Trim()
if ($LASTEXITCODE -ne 0 -or -not $commandId) { throw 'AWS rejected the recovery command.' }

$deadline = [DateTime]::UtcNow.AddMinutes(20)
do {
    Start-Sleep -Seconds 2
    $result = & $aws ssm get-command-invocation --profile $Profile --region $Region --command-id $commandId --instance-id $InstanceId --query '{Status:Status,Output:StandardOutputContent,Error:StandardErrorContent}' --output json 2>$null | ConvertFrom-Json
    if ($result.Status -in @('Success', 'Cancelled', 'Failed', 'TimedOut')) { break }
} while ([DateTime]::UtcNow -lt $deadline)

if (-not $result -or $result.Status -ne 'Success') {
    throw "Recovery command $commandId failed or timed out: $($result.Error)"
}
[pscustomobject]@{
    CommandId = $commandId
    Operation = $Operation
    ReleaseCommit = $releaseCommit
    Evidence = $result.Output.Trim()
}
