[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidateSet('Backup', 'Restore')]
    [string]$Operation,
    [string]$InstanceId = 'i-0ee1235614219f089',
    [ValidateSet('ap-northeast-1')]
    [string]$Region = 'ap-northeast-1',
    [string]$Profile = 'kanz-platform',
    [string]$RecoveryBucket = 'kanz-testnet-tokyo-recovery-012619468098',
    [switch]$QualifyTVSync
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

$tvMode = 'off'
$pairEncoded = ''
$helperCommands = @()
if ($QualifyTVSync) {
    if ($InstanceId -cne 'i-0ee1235614219f089' -or $Region -cne 'ap-northeast-1' -or $RecoveryBucket -cne 'kanz-testnet-tokyo-recovery-012619468098') { throw 'TV recovery target is not the reviewed Tokyo boundary.' }
    $head = (& git -C $repoRoot rev-parse HEAD).Trim()
    if ($LASTEXITCODE -ne 0 -or $head -cne $releaseCommit) { throw 'TV recovery requires exact origin/main HEAD.' }
    $owned = @('tools/Invoke-TestnetDataPlaneRecovery.ps1', 'tools/testnet-data-plane-recovery.sh', 'tools/tv-sync-restored-proof.sh', 'tools/Resolve-EcrReleaseDigest.ps1', 'tools/Resolve-TvReplayPlatform.ps1', 'kanz/infra/overlays/testnet-tokyo/tv-sync-release.json')
    foreach ($file in $owned) {
        $local = (& git -C $repoRoot hash-object (Join-Path $repoRoot $file)).Trim()
        if ($LASTEXITCODE -ne 0) { throw 'TV recovery provenance unavailable.' }
        $merged = (& git -C $repoRoot rev-parse "${releaseCommit}:$file" 2>$null).Trim()
        if ($LASTEXITCODE -ne 0 -or $local -cne $merged) { throw 'TV recovery source differs from origin/main.' }
    }
    . (Join-Path $PSScriptRoot 'Resolve-EcrReleaseDigest.ps1')
    . (Join-Path $PSScriptRoot 'Resolve-TvReplayPlatform.ps1')
    $lockPath = Join-Path $repoRoot 'kanz/infra/overlays/testnet-tokyo/tv-sync-release.json'
    $pair = Read-ReviewedTvSyncRelease -LockPath $lockPath
    & git -C $repoRoot merge-base --is-ancestor $pair.release_sha $releaseCommit
    if ($LASTEXITCODE -ne 0) { throw 'Replay release source is not an ancestor of reviewed main.' }
    foreach ($file in @('kanz/services/tv-sync/cmd/tv-sync-replay/main.go', 'kanz/services/tv-sync/internal/projection/verify_replay_exact.go')) {
        & git -C $repoRoot cat-file -e "$($pair.release_sha):$file" 2>$null
        if ($LASTEXITCODE -ne 0) { throw 'Reviewed release lacks the exact-state replay verifier; a new signed pair is required before SSM.' }
    }
    # Public identity/manifest readback only. No secret values are retrieved.
    $identity = (& $aws sts get-caller-identity --profile $Profile --region $Region --output json 2>$null) | ConvertFrom-Json
    if ($LASTEXITCODE -ne 0 -or $identity.Account -cne '012619468098') { throw 'TV recovery account mismatch.' }
    $instance = (& $aws ec2 describe-instances --profile $Profile --region $Region --instance-ids $InstanceId --query 'Reservations[].Instances[].InstanceId' --output json 2>$null) | ConvertFrom-Json
    if ($LASTEXITCODE -ne 0 -or @($instance).Count -ne 1 -or @($instance)[0] -cne $InstanceId) { throw 'TV recovery instance mismatch.' }
    foreach ($name in @('tv-sync','kanz-migrate')) {
        $member = $pair.images.PSObject.Properties[$name].Value
        $digest = ($member.destination -split '@')[1]
        $tag = Resolve-ReviewedTestnetReleaseTag -LockPath $lockPath -Repository $name -SourceCommit $pair.release_sha -Digest $digest
        $actual = Resolve-EcrReleaseDigest -Aws $aws -Profile $Profile -Region $Region -Repository $name -ReleaseTag $tag
        if ($actual -cne $digest) { throw 'TV recovery paired image custody differs.' }
    }
    $rootDigest = ($pair.images.'tv-sync'.destination -split '@')[1]
    $platformDigest = Resolve-TvReplayPlatform -Aws $aws -Profile $Profile -RootDigest $rootDigest
    $pair | Add-Member -NotePropertyName runtime_platform_digest -NotePropertyValue $platformDigest
    $pairEncoded = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes(($pair | ConvertTo-Json -Compress -Depth 8)))
    $helperRaw = [IO.File]::ReadAllText((Join-Path $PSScriptRoot 'tv-sync-restored-proof.sh')).Replace("`r`n", "`n")
    $helperEncoded = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($helperRaw))
    $helperCommands = @("printf '%s' '$helperEncoded' | base64 -d > `"`$work/tv-sync-restored-proof.sh`"", "chmod 0700 `"`$work/tv-sync-restored-proof.sh`"")
    $tvMode = 'qualify'
}

$kmsArn = (& $aws kms describe-key --profile $Profile --region ap-northeast-3 --key-id alias/kanz-testnet-tokyo-recovery --query KeyMetadata.Arn --output text).Trim()
if ($LASTEXITCODE -ne 0 -or $kmsArn -notmatch '^arn:aws:kms:ap-northeast-3:012619468098:key/') {
    throw 'Could not resolve the bounded Osaka recovery KMS key.'
}

$raw = [IO.File]::ReadAllText($scriptPath).Replace([string]([char]13) + [char]10, [string][char]10)
$encoded = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($raw))
$commands = @(
    'set -Eeuo pipefail',
    'umask 077',
    'work=$(mktemp -d /var/tmp/kanz-recovery-source.XXXXXX)',
    'readonly work',
    'chmod 0700 "$work"',
    'cleanup_source() { rc=$?; rm -f -- "$work/testnet-data-plane-recovery.sh" "$work/tv-sync-restored-proof.sh" || rc=2; rmdir -- "$work" || rc=2; exit "$rc"; }',
    'trap cleanup_source EXIT'
) + $helperCommands + @(
    "printf '%s' '$encoded' | base64 -d > `"`$work/testnet-data-plane-recovery.sh`"",
    "chmod 0700 `"`$work/testnet-data-plane-recovery.sh`"",
    "`"`$work/testnet-data-plane-recovery.sh`" '$($Operation.ToLowerInvariant())' '$RecoveryBucket' '$kmsArn' '$releaseCommit' '$tvMode' '$pairEncoded'"
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
    if ($QualifyTVSync) { throw "TV recovery command $commandId failed or timed out; no restored-state qualification." }
    throw "Recovery command $commandId failed or timed out: $($result.Error)"
}
[pscustomobject]@{
    CommandId = $commandId
    Operation = $Operation
    ReleaseCommit = $releaseCommit
    Evidence = $result.Output.Trim()
}
