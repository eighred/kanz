$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'Resolve-TvReplayPlatform.ps1')
$rootDigest = 'sha256:' + ('a' * 64)
$leafDigest = 'sha256:' + ('b' * 64)
$reader = ${function:Get-TvReplayEcrManifest}
$script:manifestBytes = '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}'
$sha = [Security.Cryptography.SHA256]::Create()
try { $script:manifestDigest = 'sha256:' + ([BitConverter]::ToString($sha.ComputeHash([Text.Encoding]::UTF8.GetBytes($script:manifestBytes)))).Replace('-', '').ToLowerInvariant() } finally { $sha.Dispose() }
$script:readerMode = 'valid'
function Invoke-FixtureAws {
    $global:LASTEXITCODE = 0
    $bytes = $script:manifestBytes
    $repository = 'tv-sync'
    if ($script:readerMode -eq 'changed-bytes') { $bytes += ' ' }
    if ($script:readerMode -eq 'wrong-repository') { $repository = 'other' }
    if ($script:readerMode -eq 'command-failed') { $global:LASTEXITCODE = 1 }
    return (@{ failures = @(); images = @(@{registryId='012619468098'; repositoryName=$repository; imageId=@{imageDigest=$script:manifestDigest}; imageManifest=$bytes}) } | ConvertTo-Json -Compress -Depth 8)
}
$read = & $reader -Aws Invoke-FixtureAws -Profile fixture -Digest $script:manifestDigest
if ($read.schemaVersion -ne 2) { throw 'valid manifest bytes refused' }
foreach ($mode in @('changed-bytes','wrong-repository','command-failed')) {
    $script:readerMode = $mode
    $refused = $false
    try { $null = & $reader -Aws Invoke-FixtureAws -Profile fixture -Digest $script:manifestDigest } catch { $refused = $true }
    if (-not $refused) { throw "manifest fixture was not refused: $mode" }
}
$script:fixtureMode = 'valid' 
function Get-TvReplayEcrManifest {
    param([string]$Aws, [string]$Profile, [string]$Digest)
    if ($script:fixtureMode -eq 'lookup-failed') { throw 'fixture lookup failure' }
    if ($Digest -ceq $rootDigest) {
        $descriptors = @([pscustomobject]@{ digest = $leafDigest; platform = @{ os = 'linux'; architecture = 'amd64' } })
        if ($script:fixtureMode -eq 'duplicate') { $descriptors += $descriptors[0] }
        if ($script:fixtureMode -eq 'wrong-platform') { $descriptors[0].platform.architecture = 'arm64' }
        return @{ schemaVersion = 2; mediaType = 'application/vnd.oci.image.index.v1+json'; manifests = $descriptors }
    }
    if ($Digest -cne $leafDigest) { throw 'unexpected digest' }
    $version = 2
    if ($script:fixtureMode -eq 'invalid-child') { $version = 1 }
    return @{ schemaVersion = $version; mediaType = 'application/vnd.oci.image.manifest.v1+json'; config = @{digest = 'sha256:' + ('c' * 64)}; layers = @(@{digest = 'sha256:' + ('d' * 64)}) }
}
$actual = Resolve-TvReplayPlatform -Aws fixture -Profile fixture -RootDigest $rootDigest
if ($actual -cne $leafDigest) { throw 'valid platform fixture failed' }
foreach ($mode in @('lookup-failed','duplicate','wrong-platform','invalid-child')) {
    $script:fixtureMode = $mode
    $refused = $false
    try { $null = Resolve-TvReplayPlatform -Aws fixture -Profile fixture -RootDigest $rootDigest } catch { $refused = $true }
    if (-not $refused) { throw "platform fixture was not refused: $mode" }
}
# The failed-native-command fixture sets this global deliberately. All refusal
# assertions above have completed; do not leak its simulated status to pwsh CI.
$global:LASTEXITCODE = 0
Write-Output 'replay-platform-fixtures=9 PASS'
