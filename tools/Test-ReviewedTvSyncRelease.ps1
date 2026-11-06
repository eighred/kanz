param([Parameter(Mandatory = $true)][string]$FixtureDirectory)
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'Resolve-EcrReleaseDigest.ps1')
$lockPath = Join-Path $PSScriptRoot '../kanz/infra/overlays/testnet-tokyo/tv-sync-release.json'
$lock = Read-ReviewedTvSyncRelease -LockPath $lockPath
$digest = $lock.images.'tv-sync'.destination.Split('@')[1]
if ((Resolve-ReviewedTestnetReleaseTag -LockPath $lockPath -Repository 'tv-sync' -SourceCommit $lock.release_sha -Digest $digest) -cne 'release-v2026.4.0') { throw 'signed member not resolved' }
$ordinary = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
if ((Resolve-ReviewedTestnetReleaseTag -LockPath $lockPath -Repository 'oms' -SourceCommit $ordinary -Digest ('sha256:' + ('b' * 64))) -cne $ordinary) { throw 'ordinary full-SHA resolution changed' }
foreach ($tuple in @(
    @{ Repository = 'tv-sync'; SourceCommit = $ordinary; Digest = $digest },
    @{ Repository = 'oms'; SourceCommit = $lock.release_sha; Digest = $digest },
    @{ Repository = 'tv-sync'; SourceCommit = $lock.release_sha; Digest = ('sha256:' + ('b' * 64)) }
)) {
    $refused = $false
    try { Resolve-ReviewedTestnetReleaseTag -LockPath $lockPath @tuple | Out-Null } catch { $refused = $true }
    if (-not $refused) { throw 'signed member mismatch fell back to an ordinary tag' }
}
$raw = [IO.File]::ReadAllText($lockPath)
$mutations = @(
    $raw.Replace('"schema_version": 1,', '"schema_version": 1, "schema_version": 1,'),
    $raw.Replace('"schema_version": 1,', '"schema_version": 1, "unknown": true,'),
    $raw.Replace('v2026.4.0', 'v2026.4.1'),
    $raw.Replace($lock.release_sha, $ordinary),
    $raw.Replace('token.actions.githubusercontent.com', 'invalid.example'),
    $raw.Replace('amazonaws.com/tv-sync@', 'amazonaws.com/oms@'),
    $raw.Replace($digest, ('sha256:' + ('b' * 64))),
    $raw.Replace('"tv-sync": {', '"unreviewed": {')
)
$fixture = Join-Path $FixtureDirectory 'mutated-release.json'
foreach ($mutation in $mutations) {
    [IO.File]::WriteAllBytes($fixture, [Text.Encoding]::UTF8.GetBytes($mutation))
    $refused = $false
    try { Read-ReviewedTvSyncRelease -LockPath $fixture | Out-Null } catch { $refused = $true }
    if (-not $refused) { throw 'tampered release lock accepted' }
}
Write-Output 'reviewed signed-pair tag binding and mutation probes passed'
