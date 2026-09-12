[CmdletBinding()]
param(
    [ValidateSet('Bootstrap', 'Rotate')]
    [string]$Mode = 'Rotate',

    [ValidateSet('Venue', 'DataPlane', 'AccountProof', 'WebEdge')]
    [string]$Target = 'Venue',

    [switch]$StageOnly
)

$ErrorActionPreference = 'Stop'
$profileName = if ($env:KANZ_AWS_PROFILE) { $env:KANZ_AWS_PROFILE } else { 'kanz-platform' }
$region = 'ap-northeast-1'
$scriptRoot = $PSScriptRoot
$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $scriptRoot '..'))
$terraformRoot = Join-Path $repositoryRoot 'kanz\infra\terraform-testnet'
$scriptName = if ($Target -eq 'DataPlane') { 'bootstrap-testnet-data-plane.sh' } elseif ($Target -eq 'WebEdge') { 'bootstrap-testnet-web-edge.sh' } else { 'bootstrap-testnet-venues.sh' }
$bootstrapScript = Join-Path $scriptRoot $scriptName
$remoteName = switch ($Target) {
    'DataPlane' { 'testnet-data-plane-secrets' }
    'AccountProof' { 'testnet-venue-account-proof' }
    'WebEdge' { 'testnet-web-edge' }
    default { 'testnet-venue-secrets' }
}
$completionMarker = "/vault/run/$remoteName.complete"
$aws = (Get-Command aws.exe -ErrorAction Stop).Source
$terraformCommand = Get-Command terraform.exe -ErrorAction SilentlyContinue
if (-not $terraformCommand) {
    $terraformPath = Get-ChildItem "$env:LOCALAPPDATA\Microsoft\WinGet\Packages\Hashicorp.Terraform_*" `
        -Recurse -Filter terraform.exe -ErrorAction SilentlyContinue |
        Select-Object -First 1 -ExpandProperty FullName
}
else {
    $terraformPath = $terraformCommand.Source
}

if (-not (Test-Path -LiteralPath $bootstrapScript -PathType Leaf)) {
    throw "Vault bootstrap script not found: $bootstrapScript"
}
if ($Target -eq 'DataPlane' -and $Mode -eq 'Rotate') {
    throw 'Data-plane bootstrap is idempotent; use Bootstrap mode.'
}
if (-not (Get-Command session-manager-plugin.exe -ErrorAction SilentlyContinue)) {
    $pluginDirectory = 'C:\Program Files\Amazon\SessionManagerPlugin\bin'
    if (-not (Test-Path -LiteralPath (Join-Path $pluginDirectory 'session-manager-plugin.exe'))) {
        throw 'AWS Session Manager Plugin is not installed.'
    }
    $env:Path = "$pluginDirectory;$env:Path"
}

if ($env:KANZ_TESTNET_INSTANCE_ID) {
    $instanceId = $env:KANZ_TESTNET_INSTANCE_ID
}
elseif ($terraformPath) {
    $instanceOutput = & $terraformPath "-chdir=$terraformRoot" output -raw instance_id 2>$null
    if ($LASTEXITCODE -ne 0 -or -not $instanceOutput) {
        throw 'Terraform could not resolve the testnet EC2 instance from local state.'
    }
    $instanceId = $instanceOutput.Trim()
}
else {
    throw 'Terraform is unavailable and KANZ_TESTNET_INSTANCE_ID is not set.'
}
if ($instanceId -notmatch '^i-[0-9a-f]+$') {
    throw 'Unable to resolve the testnet EC2 instance from Terraform state.'
}

function Send-SsmShellCommand {
    param(
        [Parameter(Mandatory = $true)]
        [string[]]$Commands,

        [Parameter(Mandatory = $true)]
        [string]$Comment
    )

    # Windows PowerShell 5.1 strips the JSON quotes when a structured value is
    # passed directly to a native executable. Let AWS CLI load the JSON from a
    # short-lived, secret-free file so the same launcher works in 5.1 and 7+.
    $parameterFile = Join-Path ([IO.Path]::GetTempPath()) ("kanz-ssm-parameters-{0}.json" -f [Guid]::NewGuid())
    try {
        $parameterJson = @{ commands = $Commands } | ConvertTo-Json -Compress
        [IO.File]::WriteAllText($parameterFile, $parameterJson, (New-Object Text.UTF8Encoding($false)))
        $id = (& $aws ssm send-command `
            --profile $profileName `
            --region $region `
            --instance-ids $instanceId `
            --document-name AWS-RunShellScript `
            --comment $Comment `
            --parameters "file://$parameterFile" `
            --query 'Command.CommandId' `
            --output text).Trim()
        if ($LASTEXITCODE -ne 0 -or -not $id) {
            throw 'Failed to send the SSM shell command.'
        }
        return $id
    }
    finally {
        Remove-Item -LiteralPath $parameterFile -Force -ErrorAction SilentlyContinue
    }
}

$payload = [Convert]::ToBase64String([IO.File]::ReadAllBytes($bootstrapScript))
$remoteScript = "/vault/run/bootstrap-$remoteName.sh"
$stageCommands = @(
    "printf '%s' '$payload' | base64 -d > /var/tmp/kanz-bootstrap-$remoteName.sh",
    "chmod 0700 /var/tmp/kanz-bootstrap-$remoteName.sh",
    "k3s kubectl -n vault cp /var/tmp/kanz-bootstrap-$remoteName.sh vault-0:$remoteScript -c vault",
    "k3s kubectl -n vault exec vault-0 -c vault -- chmod 0700 $remoteScript",
    "k3s kubectl -n vault exec vault-0 -c vault -- /bin/sh -n $remoteScript",
    "k3s kubectl -n vault exec vault-0 -c vault -- rm -f $completionMarker",
    "rm -f /var/tmp/kanz-bootstrap-$remoteName.sh"
)
$commandId = Send-SsmShellCommand -Commands $stageCommands -Comment "Stage the secret-free Kanz $remoteName operator script"

$deadline = [DateTime]::UtcNow.AddMinutes(2)
do {
    Start-Sleep -Milliseconds 500
    $invocation = & $aws ssm get-command-invocation `
        --profile $profileName `
        --region $region `
        --command-id $commandId `
        --instance-id $instanceId `
        --output json 2>$null | ConvertFrom-Json
    if ($invocation.Status -in @('Success', 'Cancelled', 'Failed', 'TimedOut')) { break }
} while ([DateTime]::UtcNow -lt $deadline)
if ($invocation.Status -ne 'Success') {
    throw "Vault operator script staging failed with status '$($invocation.Status)': $($invocation.StandardErrorContent)"
}
if ($StageOnly) {
    Write-Host "$Target Vault operator script staged and syntax-checked successfully."
    return
}

$remoteArgument = switch ($Target) {
    'Venue' { ' ' + $Mode.ToLowerInvariant() }
    'AccountProof' { ' account-proof' }
    'WebEdge' { '' }
    default { '' }
}
$interactiveCommand = "sudo k3s kubectl -n vault exec -it vault-0 -c vault -- /bin/sh $remoteScript$remoteArgument"
Write-Host 'Sensitive values are entered only in the remote Vault process. Hidden input is expected.'
& $aws ssm start-session `
    --profile $profileName `
    --region $region `
    --target $instanceId `
    --document-name AWS-StartInteractiveCommand `
    --parameters "command=$interactiveCommand"
if ($LASTEXITCODE -ne 0) {
    throw "The interactive SSM session failed with exit code $LASTEXITCODE."
}

$checkCommands = @(
    "k3s kubectl -n vault exec vault-0 -c vault -- test -f $completionMarker"
)
$checkCommandId = Send-SsmShellCommand -Commands $checkCommands -Comment "Verify the Kanz $remoteName completion marker"

$deadline = [DateTime]::UtcNow.AddMinutes(1)
do {
    Start-Sleep -Milliseconds 500
    $check = & $aws ssm get-command-invocation `
        --profile $profileName `
        --region $region `
        --command-id $checkCommandId `
        --instance-id $instanceId `
        --output json 2>$null | ConvertFrom-Json
    if ($check.Status -in @('Success', 'Cancelled', 'Failed', 'TimedOut')) { break }
} while ([DateTime]::UtcNow -lt $deadline)
if ($check.Status -ne 'Success') {
    throw 'Vault did not emit its completion marker; credential rotation is not verified.'
}

Write-Host "$Target $Mode completed and the remote completion marker was verified."
