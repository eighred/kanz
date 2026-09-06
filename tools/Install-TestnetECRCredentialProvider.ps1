[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^i-[0-9a-f]+$')]
    [string]$InstanceId,

    [ValidateSet('ap-northeast-1')]
    [string]$Region = 'ap-northeast-1',

    [string]$Profile = 'kanz-platform',

    [ValidatePattern('^[0-9]{12}\.dkr\.ecr\.ap-northeast-1\.amazonaws\.com/.+@sha256:[0-9a-f]{64}$')]
    [string]$ProbeImage = ''
)

$ErrorActionPreference = 'Stop'
$aws = (Get-Command aws.exe -ErrorAction Stop).Source
$providerCommit = '7656c21bcc13700566830f6bc4d753063513e6f7'
$sourcePath = "/var/tmp/cloud-provider-aws-$providerCommit"
$configCommand = @'
printf '%s\n' 'apiVersion: kubelet.config.k8s.io/v1' 'kind: CredentialProviderConfig' 'providers:' '  - name: ecr-credential-provider' '    matchImages:' '      - "*.dkr.ecr.*.amazonaws.com"' '      - "*.dkr.ecr.*.amazonaws.com.cn"' '    defaultCacheDuration: 12h' '    apiVersion: credentialprovider.kubelet.k8s.io/v1' > /var/lib/rancher/credentialprovider/config.yaml
'@

$commands = @(
    'set -Eeuo pipefail',
    'dnf install -y git golang >/dev/null',
    "test ! -e '$sourcePath'",
    "git clone --quiet --filter=blob:none --no-checkout https://github.com/kubernetes/cloud-provider-aws.git '$sourcePath'",
    "git -C '$sourcePath' checkout --quiet --detach '$providerCommit'",
    "test `$(git -C '$sourcePath' rev-parse HEAD) = '$providerCommit'",
    'install -d -m 0755 /var/lib/rancher/credentialprovider/bin',
    "cd '$sourcePath'",
    'CGO_ENABLED=0 GOCACHE=/var/tmp/kanz-ecr-provider-cache GOMODCACHE=/var/tmp/kanz-ecr-provider-modcache go build -trimpath -ldflags=''-s -w -buildid='' -o /var/lib/rancher/credentialprovider/bin/ecr-credential-provider ./cmd/ecr-credential-provider',
    'chmod 0755 /var/lib/rancher/credentialprovider/bin/ecr-credential-provider',
    $configCommand,
    "rm -rf '$sourcePath' /var/tmp/kanz-ecr-provider-cache /var/tmp/kanz-ecr-provider-modcache",
    'dnf remove -y git golang >/dev/null',
    'systemctl restart k3s',
    'systemctl is-active --quiet k3s',
    '/usr/local/bin/k3s kubectl wait --for=condition=Ready node --all --timeout=180s'
)

if ($ProbeImage) {
    $commands += @(
        "/usr/local/bin/k3s crictl rmi '$ProbeImage' >/dev/null 2>&1 || true",
        "/usr/local/bin/k3s crictl pull '$ProbeImage' >/dev/null",
        "echo 'cold-ecr-pull-ok'"
    )
}
else {
    $commands += "echo 'ecr-credential-provider-installed'"
}

$parameters = @{ commands = $commands } | ConvertTo-Json -Compress -Depth 4
$commandId = (& $aws ssm send-command `
    --profile $Profile `
    --region $Region `
    --instance-ids $InstanceId `
    --document-name AWS-RunShellScript `
    --comment 'Install pinned kubelet ECR credential provider for Kanz testnet' `
    --parameters $parameters `
    --query 'Command.CommandId' `
    --output text).Trim()
if ($LASTEXITCODE -ne 0 -or -not $commandId) {
    throw 'AWS rejected the SSM installation command.'
}

& $aws ssm wait command-executed `
    --profile $Profile `
    --region $Region `
    --command-id $commandId `
    --instance-id $InstanceId
if ($LASTEXITCODE -ne 0) {
    throw "SSM command $commandId did not reach a successful terminal state."
}

$result = & $aws ssm get-command-invocation `
    --profile $Profile `
    --region $Region `
    --command-id $commandId `
    --instance-id $InstanceId `
    --query '{Status:Status,Output:StandardOutputContent,Error:StandardErrorContent}' `
    --output json | ConvertFrom-Json
if ($LASTEXITCODE -ne 0 -or $result.Status -ne 'Success') {
    throw "Credential-provider installation failed: $($result.Error)"
}

[pscustomobject]@{
    CommandId = $commandId
    Status    = $result.Status
    Evidence  = $result.Output.Trim()
}
