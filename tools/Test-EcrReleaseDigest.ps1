$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'Resolve-EcrReleaseDigest.ps1')
$expected = 'sha256:' + ('a' * 64)
$actual = ConvertFrom-EcrDigestResult -Output @("  $expected`n") -ExitCode 0 -Repository accounting -ReleaseTag old
if ($actual -cne $expected) { throw 'Valid digest was not preserved.' }
foreach ($case in @(
    @{ Output = @(); Exit = 0; Reason = 'no valid digest' },
    @{ Output = @('None'); Exit = 0; Reason = 'no valid digest' },
    @{ Output = @($expected, $expected); Exit = 0; Reason = 'no valid digest' },
    @{ Output = @(); Exit = 1; Reason = 'AWS CLI failure' },
    @{ Output = @('An error occurred (ImageNotFoundException)'); Exit = 254; Reason = 'ImageNotFoundException' },
    @{ Output = @('An error occurred (AccessDeniedException)'); Exit = 254; Reason = 'AccessDeniedException' },
    @{ Output = @($expected); Exit = 1; Reason = 'AWS CLI failure' }
)) {
    $failure = $null
    try { ConvertFrom-EcrDigestResult -Output $case.Output -ExitCode $case.Exit -Repository accounting -ReleaseTag old | Out-Null }
    catch { $failure = $_.Exception.Message }
    if (-not $failure -or -not $failure.Contains($case.Reason) -or -not $failure.Contains('accounting:old')) {
        throw "Incorrect fail-closed diagnostic: $failure"
    }
}
Write-Output 'ECR result validation: 8 cases passed.'
