[CmdletBinding()]
param(
    [Parameter(Mandatory = $true, Position = 0)]
    [ValidateSet('fmt', 'init', 'validate', 'test', 'plan', 'show', 'apply', 'destroy', 'output')]
    [string]$Command,

    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$TerraformArguments
)

$ErrorActionPreference = 'Stop'
$profileName = if ($env:KANZ_AWS_PROFILE) { $env:KANZ_AWS_PROFILE } else { 'kanz-platform' }
$aws = (Get-Command aws.exe -ErrorAction Stop).Source
$terraformCommand = Get-Command terraform.exe -ErrorAction SilentlyContinue
if ($terraformCommand) {
    $terraform = $terraformCommand.Source
}
else {
    # Winget updates PATH for future shells. Resolve its verified package in the
    # current shell as well so the first provisioning run is deterministic.
    $terraform = Get-ChildItem "$env:LOCALAPPDATA\Microsoft\WinGet\Packages\Hashicorp.Terraform_*" `
        -Recurse -Filter terraform.exe | Select-Object -First 1 -ExpandProperty FullName
    if (-not $terraform) {
        throw 'terraform.exe is unavailable. Install the official Hashicorp.Terraform package.'
    }
}

# AWS CLI v2's login_session is newer than the AWS SDK credential-chain support
# used by some Terraform provider releases. Export only the already-short-lived,
# MFA-backed role session into this process. Never print or persist it.
$credentials = (& $aws configure export-credentials --profile $profileName --format process | ConvertFrom-Json)
if ($LASTEXITCODE -ne 0 -or -not $credentials.AccessKeyId) {
    throw "Unable to obtain a short-lived session from AWS profile '$profileName'. Run aws login --profile kanz-operator."
}

$previous = @{
    AccessKey = $env:AWS_ACCESS_KEY_ID
    SecretKey = $env:AWS_SECRET_ACCESS_KEY
    Token     = $env:AWS_SESSION_TOKEN
    Region    = $env:AWS_REGION
}

try {
    $env:AWS_ACCESS_KEY_ID = $credentials.AccessKeyId
    $env:AWS_SECRET_ACCESS_KEY = $credentials.SecretAccessKey
    $env:AWS_SESSION_TOKEN = $credentials.SessionToken
    $env:AWS_REGION = 'ap-northeast-1'
    & $terraform $Command @TerraformArguments
    if ($LASTEXITCODE -ne 0) {
        throw "terraform $Command failed with exit code $LASTEXITCODE"
    }
}
finally {
    $env:AWS_ACCESS_KEY_ID = $previous.AccessKey
    $env:AWS_SECRET_ACCESS_KEY = $previous.SecretKey
    $env:AWS_SESSION_TOKEN = $previous.Token
    $env:AWS_REGION = $previous.Region
}
