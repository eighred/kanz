# Dot-source only after exact origin/main provenance verification.
function Get-TvReplayEcrManifest {
    param([string]$Aws, [string]$Profile, [string]$Digest)
    if ($Digest -cnotmatch '^sha256:[0-9a-f]{64}$') { throw 'Invalid replay manifest digest.' }
    $raw = @(& $Aws ecr batch-get-image --profile $Profile --region ap-northeast-1 --registry-id 012619468098 --repository-name tv-sync --image-ids "imageDigest=$Digest" --output json --no-cli-pager 2>$null)
    if ($LASTEXITCODE -ne 0) { throw 'Replay manifest lookup failed.' }
    $result = ($raw -join "`n") | ConvertFrom-Json
    if (@($result.failures).Count -ne 0 -or @($result.images).Count -ne 1) { throw 'Replay manifest lookup is ambiguous.' }
    $image = $result.images[0]
    if ($image.registryId -cne '012619468098' -or $image.repositoryName -cne 'tv-sync' -or $image.imageId.imageDigest -cne $Digest -or $image.imageManifest -isnot [string]) { throw 'Replay manifest identity differs.' }
    $bytes = [Text.Encoding]::UTF8.GetBytes($image.imageManifest)
    if ($bytes.Length -eq 0 -or $bytes.Length -gt 1048576) { throw 'Replay manifest exceeds bounds.' }
    $sha = [Security.Cryptography.SHA256]::Create()
    try { $actual = 'sha256:' + ([BitConverter]::ToString($sha.ComputeHash($bytes))).Replace('-', '').ToLowerInvariant() }
    finally { $sha.Dispose() }
    if ($actual -cne $Digest) { throw 'Replay manifest bytes differ from digest.' }
    return ($image.imageManifest | ConvertFrom-Json)
}

function Resolve-TvReplayPlatform {
    param([string]$Aws, [string]$Profile, [string]$RootDigest)
    $index = Get-TvReplayEcrManifest -Aws $Aws -Profile $Profile -Digest $RootDigest
    if ($index.schemaVersion -ne 2 -or $index.mediaType -cnotin @('application/vnd.oci.image.index.v1+json', 'application/vnd.docker.distribution.manifest.list.v2+json')) { throw 'Reviewed replay root is not a platform index.' }
    $platforms = @($index.manifests | Where-Object { $_.platform.os -ceq 'linux' -and $_.platform.architecture -ceq 'amd64' })
    if ($platforms.Count -ne 1 -or $platforms[0].digest -cnotmatch '^sha256:[0-9a-f]{64}$') { throw 'Replay linux/amd64 platform is ambiguous.' }
    $digest = $platforms[0].digest
    $manifest = Get-TvReplayEcrManifest -Aws $Aws -Profile $Profile -Digest $digest
    if ($manifest.schemaVersion -ne 2 -or $manifest.mediaType -cnotin @('application/vnd.oci.image.manifest.v1+json', 'application/vnd.docker.distribution.manifest.v2+json') -or $manifest.config.digest -cnotmatch '^sha256:[0-9a-f]{64}$' -or @($manifest.layers).Count -eq 0) { throw 'Replay platform manifest is invalid.' }
    return $digest
}
