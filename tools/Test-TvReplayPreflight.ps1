$ErrorActionPreference = 'Stop'
$nativeGit = (Get-Command git -CommandType Application | Select-Object -First 1).Source
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$canonicalLock = 'kanz/infra/overlays/testnet_tokyo/tv-sync-release.json'
$fixtureMain = 'f' * 40
$fixtureState = [pscustomobject]@{ pair = $null; mode = ''; lockReads = 0; readLock = $false; checkedVerifier = $false }

# Execute the real wrapper through its preflight. Only the remote/source
# identity responses are fixtures; each owned input must exist and be tracked.
# No AWS request, git fetch, credential read or target mutation is permitted.
function aws.exe { throw 'Unexpected AWS call in offline preflight fixture.' }
function git {
    $argv = @($args)
    if ($argv.Count -lt 3 -or $argv[0] -cne '-C' -or $argv[1] -cne $repoRoot) {
        throw 'Unexpected Git invocation in preflight fixture.'
    }
    $op = $argv[2]
    $global:LASTEXITCODE = 0
    if ($op -ceq 'fetch' -and ($argv[3..5] -join ' ') -ceq 'origin main --quiet') { return }
    if ($op -ceq 'rev-parse' -and $argv[3] -in @('HEAD', 'origin/main')) { return $fixtureMain }
    if ($op -ceq 'merge-base' -and $argv[3] -ceq '--is-ancestor' -and $argv[4] -ceq $fixtureState.pair.release_sha -and $argv[5] -ceq $fixtureMain) {
        $fixtureState.readLock = $true
        return
    }
    if ($op -ceq 'cat-file' -and $argv[3] -ceq '-e' -and $argv[4] -ceq "$($fixtureState.pair.release_sha):kanz/services/tv_sync/cmd/tv_sync_replay/main.go") {
        $fixtureState.checkedVerifier = $true
        $global:LASTEXITCODE = 1
        return
    }
    $file = $null
    if ($op -ceq 'hash-object') { $file = [IO.Path]::GetRelativePath($repoRoot, $argv[3]).Replace('\', '/') }
    if ($op -ceq 'rev-parse' -and $argv[3].StartsWith($fixtureMain + ':', [StringComparison]::Ordinal)) {
        $file = $argv[3].Substring(41)
    }
    if (-not $file) { throw 'Unexpected Git operation in preflight fixture.' }
    & $nativeGit -C $repoRoot ls-files --error-unmatch -- $file 1>$null 2>$null
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath (Join-Path $repoRoot $file) -PathType Leaf)) {
        throw 'Preflight references an absent or untracked source input.'
    }
    if ($file -ceq $canonicalLock) { $fixtureState.lockReads++ }
    if ($fixtureState.mode -ceq 'changed-helper' -and $op -ceq 'rev-parse' -and $file -ceq 'tools/tv-sync-restored-proof.sh') { return ('0' * 40) }
    if ($fixtureState.mode -ceq 'missing-lock' -and $file -ceq $canonicalLock) { $global:LASTEXITCODE = 1; return '' }
    if ($fixtureState.mode -ceq 'changed-source' -and $op -ceq 'rev-parse' -and $file -ceq $canonicalLock) { return ('0' * 40) }
    & $nativeGit -C $repoRoot hash-object -- (Join-Path $repoRoot $file)
}

. (Join-Path $PSScriptRoot 'Resolve-EcrReleaseDigest.ps1')
$fixtureState.pair = Read-ReviewedTvSyncRelease -LockPath (Join-Path $repoRoot $canonicalLock)
foreach ($case in @(
    @{ Mode = 'old-release'; Expected = 'Reviewed release lacks the exact-state replay verifier; a new signed pair is required before SSM.' },
    @{ Mode = 'missing-lock'; Expected = 'TV recovery provenance unavailable.' },
    @{ Mode = 'changed-source'; Expected = 'TV recovery source differs from origin/main.' }
)) {
    $fixtureState.mode = $case.Mode
    $fixtureState.lockReads = 0
    $fixtureState.readLock = $false
    $fixtureState.checkedVerifier = $false
    $message = ''
    try { & (Join-Path $PSScriptRoot 'Invoke-TestnetDataPlaneRecovery.ps1') -Operation Restore -QualifyTVSync }
    catch { $message = $_.Exception.Message }
    if ($message -cne $case.Expected) { throw "Preflight fixture $($case.Mode) refused at the wrong boundary: $message" }
    if ($case.Mode -ceq 'old-release' -and ($fixtureState.lockReads -ne 2 -or -not $fixtureState.readLock -or -not $fixtureState.checkedVerifier)) {
        throw 'Canonical source lock and old-verifier boundary were not both exercised.'
    }
    if ($case.Mode -cne 'old-release' -and ($fixtureState.readLock -or $fixtureState.checkedVerifier)) { throw 'Invalid provenance reached release acceptance.' }
}
$fixtureState.mode = 'changed-helper'
foreach ($tv in @($false, $true)) {
    $message = ''
    try { & (Join-Path $PSScriptRoot 'Invoke-TestnetDataPlaneRecovery.ps1') -Operation Restore -QualifyTVSync:$tv }
    catch { $message = $_.Exception.Message }
    if ($message -cne 'Recovery source tv-sync-restored-proof.sh is not the exact blob merged into origin/main.') {
        throw "Unreviewed restore helper reached the wrong boundary: $message"
    }
}
$fixtureState.mode = 'transport'
$fixtureState | Add-Member -NotePropertyName parameterPath -NotePropertyValue ''
$fixtureState | Add-Member -NotePropertyName payloadSize -NotePropertyValue 0
# Intercept the native executable resolution and its two expected calls. No
# real KMS/SSM process or network connection is started in this fixture.
function Get-Command {
    param([string]$Name, [string]$CommandType)
    if ($Name -ceq 'aws.exe') { return [pscustomobject]@{ Source = 'Invoke-OfflineRecoveryAws' } }
    Microsoft.PowerShell.Core\Get-Command @PSBoundParameters
}
function Invoke-OfflineRecoveryAws {
    if ($args[0] -ceq 'kms' -and $args[1] -ceq 'describe-key') {
        return 'arn:aws:kms:ap-northeast-3:012619468098:key/fixture'
    }
    if ($fixtureState.mode -ceq 'backup-result' -and $args[0] -ceq 'ssm') {
        if ($args[1] -ceq 'send-command') { return 'fixture-backup-command' }
        if ($args[1] -ceq 'get-command-invocation' -and $args -ccontains 'fixture-backup-command') {
            return (@{ Status='Success'; Output=($receiptFixture | ConvertTo-Json); Error='' } | ConvertTo-Json)
        }
        throw 'Unexpected offline backup command.'
    }
    if ($args[0] -cne 'ssm' -or $args[1] -cne 'send-command') { throw 'Unexpected offline AWS operation.' }
    $reference = $args[[Array]::IndexOf($args, '--parameters') + 1]
    if (-not $reference.StartsWith('file://')) { throw 'SSM parameters not file-backed.' }
    $fixtureState.parameterPath = $reference.Substring(7)
    $fixtureState.payloadSize = [IO.FileInfo]::new($fixtureState.parameterPath).Length
    $received = Get-Content -LiteralPath $fixtureState.parameterPath -Raw | ConvertFrom-Json
    if ($fixtureState.mode -ceq 'receipt-transport') {
        if ($received.commands[-1] -cnotmatch " '([A-Za-z0-9+/=]+)'$") { throw 'Missing encoded restore selection.' }
        $selected = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($Matches[1])) | ConvertFrom-Json
        if ($selected.key -cne $receiptFixture.key -or $selected.version_id -cne $receiptFixture.version_id -or $selected.sha256 -cne $receiptFixture.sha256) {
            throw 'Restore selection changed in transit.'
        }
    }
    foreach ($name in @('testnet-data-plane-recovery.sh', 'tv-sync-restored-proof.sh')) {
        $stage = @($received.commands | Where-Object { $_.StartsWith("printf '%s' '") -and $_.EndsWith(('/' + $name + '"')) })
        if ($stage.Count -ne 1 -or $stage[0] -cnotmatch "^printf '%s' '([A-Za-z0-9+/=]+)' \| base64 -d \| gzip -dc > ") {
            throw 'Missing source decompression stage.'
        }
        $encoded = [IO.MemoryStream]::new([Convert]::FromBase64String($Matches[1]))
        $gzip = [IO.Compression.GZipStream]::new($encoded, [IO.Compression.CompressionMode]::Decompress)
        $decoded = [IO.MemoryStream]::new()
        try {
            $gzip.CopyTo($decoded)
            if ([Convert]::ToBase64String($decoded.ToArray()) -cne [Convert]::ToBase64String([IO.File]::ReadAllBytes((Join-Path $PSScriptRoot $name)))) {
                throw 'Decoded recovery source differs from the checked bytes.'
            }
        } finally { $decoded.Dispose(); $gzip.Dispose(); $encoded.Dispose() }
    }
    throw 'Verified offline SSM payload.'
}
$message = ''
try { & (Join-Path $PSScriptRoot 'Invoke-TestnetDataPlaneRecovery.ps1') -Operation Restore }
catch { $message = $_.Exception.Message }
if ($message -cne 'Verified offline SSM payload.' -or (Test-Path -LiteralPath $fixtureState.parameterPath)) {
    throw "Offline source transport or parameter cleanup failed: $message"
}
$backupId = '20261118T010000Z-123456789abc'
$boundaryVersion = ("$([char]0xE9)" * 508) + "old+/='1"
$receiptFixture = @{ status='BACKUP_VERIFIED'; backup_id=$backupId; key="testnet/data-plane/$backupId/bundle.tar.gz"; version_id=$boundaryVersion; sha256='a' * 64 }
$fixtureState.mode = 'backup-result'
$backup = & (Join-Path $PSScriptRoot 'Invoke-TestnetDataPlaneRecovery.ps1') -Operation Backup
if ($backup.CommandId -cne 'fixture-backup-command' -or $backup.Operation -cne 'Backup') { throw 'Expected the completed backup result.' }
$fixtureState.mode = 'receipt-transport'
$message = ''
try { & (Join-Path $PSScriptRoot 'Invoke-TestnetDataPlaneRecovery.ps1') -Operation Restore -BackupReceipt $backup.Evidence }
catch { $message = $_.Exception.Message }
if ($message -cne 'Verified offline SSM payload.' -or (Test-Path -LiteralPath $fixtureState.parameterPath)) { throw "Receipt transport failed: $message" }
foreach ($change in @(@{status='RESTORE_VERIFIED'}, @{backup_id='mismatched'}, @{version_id='null'}, @{version_id=$boundaryVersion + 'a'}, @{sha256='short'})) {
    $invalid = $receiptFixture.Clone()
    foreach ($field in $change.Keys) { $invalid[$field] = $change[$field] }
    $message = ''
    try { & (Join-Path $PSScriptRoot 'Invoke-TestnetDataPlaneRecovery.ps1') -Operation Restore -BackupReceipt ($invalid | ConvertTo-Json) }
    catch { $message = $_.Exception.Message }
    if ($message -notlike 'Expected a verified backup receipt*') { throw "Invalid backup receipt reached the wrong boundary: $message" }
}
$message = ''
try { & (Join-Path $PSScriptRoot 'Invoke-TestnetDataPlaneRecovery.ps1') -Operation Backup -BackupReceipt $backup.Evidence }
catch { $message = $_.Exception.Message }
if ($message -cne 'A backup receipt can select a restore only.') { throw 'Backup accepted restore selection.' }
$global:LASTEXITCODE = 0
Write-Output "replay-preflight-fixtures=13 PASS (offline source, transport and receipt selection)"
