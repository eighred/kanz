# Dot-source after verifying this file against merged origin/main.
function Read-ReviewedTvSyncRelease {
    param([Parameter(Mandatory = $true)][string]$LockPath)
    # Byte pinning rejects duplicate/unknown JSON fields before PowerShell's
    # parser can silently overwrite them. Changing this lock requires review.
    $bytes = [IO.File]::ReadAllBytes($LockPath)
    $sha = [Security.Cryptography.SHA256]::Create()
    try { $hash = ([BitConverter]::ToString($sha.ComputeHash($bytes))).Replace('-', '').ToLowerInvariant() }
    finally { $sha.Dispose() }
    if ($hash -cne 'edd507687c164cc362182b3d04fad7a2491a70dbfd8bee8210a6856acf29a4b1') {
        throw 'Signed release lock is not the exact reviewed bytes.'
    }
    $lock = [Text.Encoding]::UTF8.GetString($bytes) | ConvertFrom-Json
    $keys = @($lock.PSObject.Properties.Name | Sort-Object)
    if (($keys -join ',') -cne 'destination_tag,images,release_sha,release_tag,schema_version,signer_identity,signer_issuer' -or $lock.schema_version -ne 1) {
        throw 'Signed release lock schema is unsupported.'
    }
    if ((@($lock.images.PSObject.Properties.Name | Sort-Object) -join ',') -cne 'kanz-migrate,tv-sync') {
        throw 'Signed release lock must contain exactly the reviewed pair.'
    }
    return $lock
}

function Resolve-ReviewedTestnetReleaseTag {
    param(
        [Parameter(Mandatory = $true)][string]$LockPath,
        [Parameter(Mandatory = $true)][string]$Repository,
        [Parameter(Mandatory = $true)][string]$SourceCommit,
        [Parameter(Mandatory = $true)][string]$Digest
    )
    $lock = Read-ReviewedTvSyncRelease -LockPath $LockPath
    $member = $lock.images.PSObject.Properties | Where-Object { $_.Name -ceq $Repository }
    $knownDigest = @($lock.images.PSObject.Properties | Where-Object { $_.Value.destination.EndsWith("@$Digest", [StringComparison]::Ordinal) }).Count -gt 0
    if (($member -and $SourceCommit -ceq $lock.release_sha) -or $knownDigest) {
        if (-not $member -or $SourceCommit -cne $lock.release_sha -or
            $member.Value.destination -cne "012619468098.dkr.ecr.ap-northeast-1.amazonaws.com/$Repository@$Digest" -or
            $member.Value.source -cne "ghcr.io/eighred/$Repository@$Digest") {
            throw 'Signed release source, repository and digest do not match the reviewed pair.'
        }
        return $lock.destination_tag
    }
    # Ordinary main-built images retain the existing exact full-SHA tag check.
    return $SourceCommit
}

function Resolve-EcrReleaseDigest {
    param(
        [Parameter(Mandatory = $true)][string]$Aws,
        [Parameter(Mandatory = $true)][string]$Profile,
        [Parameter(Mandatory = $true)][string]$Region,
        [Parameter(Mandatory = $true)][string]$Repository,
        [Parameter(Mandatory = $true)][string]$ReleaseTag
    )
    $previousPreference = $ErrorActionPreference
    try {
        # Windows PowerShell represents native stderr as ErrorRecord objects.
        # Capture them without throwing before we inspect the native exit code.
        $ErrorActionPreference = 'Continue'
        $output = @(& $Aws ecr describe-images --profile $Profile --region $Region `
            --repository-name $Repository --image-ids "imageTag=$ReleaseTag" `
            --query 'imageDetails[0].imageDigest' --output text 2>&1)
        $exitCode = $LASTEXITCODE
    }
    finally { $ErrorActionPreference = $previousPreference }
    return ConvertFrom-EcrDigestResult -Output $output -ExitCode $exitCode -Repository $Repository -ReleaseTag $ReleaseTag
}

function ConvertFrom-EcrDigestResult {
    param([AllowEmptyCollection()][object[]]$Output, [int]$ExitCode, [string]$Repository, [string]$ReleaseTag)
    $text = ($Output | ForEach-Object { [string]$_ }) -join "`n"
    if ($ExitCode -ne 0) {
        $reason = 'AWS CLI failure'
        if ($text -match '\(([A-Za-z][A-Za-z0-9]*Exception)\)') { $reason = $Matches[1] }
        throw "ECR lookup failed for ${Repository}:$ReleaseTag ($reason; exit $ExitCode). Deployment refused."
    }
    $digest = $text.Trim()
    if ($digest -cnotmatch '^sha256:[0-9a-f]{64}$') {
        throw "ECR returned no valid digest for ${Repository}:$ReleaseTag. Deployment refused."
    }
    return $digest
}
