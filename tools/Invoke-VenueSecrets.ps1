[CmdletBinding()]
param(
    [ValidateSet('Bootstrap', 'Rotate')]
    [string]$Mode = 'Rotate',

    [switch]$StageOnly
)

$ErrorActionPreference = 'Stop'
$profileName = if ($env:KANZ_AWS_PROFILE) { $env:KANZ_AWS_PROFILE } else { 'kanz-platform' }
$region = 'ap-northeast-1'
$scriptRoot = $PSScriptRoot
$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $scriptRoot '..'))
$terraformRoot = Join-Path $repositoryRoot 'kanz\infra\terraform-testnet'
$bootstrapScript = Join-Path $scriptRoot 'bootstrap-testnet-venues.sh'
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
    throw "Venue-secret bootstrap script not found: $bootstrapScript"
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
    $instanceId = (& $terraformPath "-chdir=$terraformRoot" output -raw instance_id 2>$null).Trim()
    if ($LASTEXITCODE -ne 0) {
        throw 'Terraform could not resolve the testnet EC2 instance from local state.'
    }
}
else {
    throw 'Terraform is unavailable and KANZ_TESTNET_INSTANCE_ID is not set.'
}
if ($instanceId -notmatch '^i-[0-9a-f]+$') {
    throw 'Unable to resolve the testnet EC2 instance from Terraform state.'
}

$payload = [Convert]::ToBase64String([IO.File]::ReadAllBytes($bootstrapScript))
$remoteScript = '/vault/run/bootstrap-testnet-venues.sh'
$stageCommands = @(
    "printf '%s' '$payload' | base64 -d > /var/tmp/kanz-bootstrap-testnet-venues.sh",
    'chmod 0700 /var/tmp/kanz-bootstrap-testnet-venues.sh',
    "k3s kubectl -n vault cp /var/tmp/kanz-bootstrap-testnet-venues.sh vault-0:$remoteScript -c vault",
    "k3s kubectl -n vault exec vault-0 -c vault -- chmod 0700 $remoteScript",
    "k3s kubectl -n vault exec vault-0 -c vault -- /bin/sh -n $remoteScript",
    'k3s kubectl -n vault exec vault-0 -c vault -- rm -f /vault/run/testnet-venue-secrets.complete',
    'rm -f /var/tmp/kanz-bootstrap-testnet-venues.sh'
)
$parameters = @{ commands = $stageCommands } | ConvertTo-Json -Compress
$commandId = (& $aws ssm send-command `
    --profile $profileName `
    --region $region `
    --instance-ids $instanceId `
    --document-name AWS-RunShellScript `
    --parameters $parameters `
    --query 'Command.CommandId' `
    --output text).Trim()
if ($LASTEXITCODE -ne 0 -or -not $commandId) {
    throw 'Failed to stage the secret-free Vault operator script through SSM.'
}

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
    Write-Host 'Secret-free Vault operator script staged and syntax-checked successfully.'
    return
}

$remoteMode = $Mode.ToLowerInvariant()
$interactiveCommand = "sudo k3s kubectl -n vault exec -it vault-0 -c vault -- /bin/sh $remoteScript $remoteMode"
Write-Host 'Secrets are entered only in the remote Vault process. Hidden input is expected.'
& $aws ssm start-session `
    --profile $profileName `
    --region $region `
    --target $instanceId `
    --document-name AWS-StartInteractiveCommand `
    --parameters "command=$interactiveCommand"
if ($LASTEXITCODE -ne 0) {
    throw "The interactive SSM session failed with exit code $LASTEXITCODE."
}

$checkParameters = @{ commands = @(
    "k3s kubectl -n vault exec vault-0 -c vault -- test -f /vault/run/testnet-venue-secrets.complete"
) } | ConvertTo-Json -Compress
$checkCommandId = (& $aws ssm send-command `
    --profile $profileName `
    --region $region `
    --instance-ids $instanceId `
    --document-name AWS-RunShellScript `
    --parameters $checkParameters `
    --query 'Command.CommandId' `
    --output text).Trim()
if ($LASTEXITCODE -ne 0 -or -not $checkCommandId) {
    throw 'Unable to verify the Vault completion marker.'
}

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

Write-Host "$Mode completed and the remote completion marker was verified."
