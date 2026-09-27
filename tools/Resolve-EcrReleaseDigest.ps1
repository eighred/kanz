# Dot-source after verifying this file against merged origin/main.
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
