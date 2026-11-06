$ErrorActionPreference = 'Stop'
$nativeGit = (Get-Command git -CommandType Application | Select-Object -First 1).Source
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$canonicalLock = 'kanz/infra/overlays/testnet-tokyo/tv-sync-release.json'
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
    if ($op -ceq 'cat-file' -and $argv[3] -ceq '-e' -and $argv[4] -ceq "$($fixtureState.pair.release_sha):kanz/services/tv-sync/cmd/tv-sync-replay/main.go") {
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
$global:LASTEXITCODE = 0
Write-Output 'replay-preflight-fixtures=3 PASS (offline boundaries only)'
