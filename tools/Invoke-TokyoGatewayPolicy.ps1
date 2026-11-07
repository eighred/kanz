[CmdletBinding()]
param([switch]$Apply)

$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$rolloutProfile = 'kanz-platform'
$region = 'ap-northeast-1'
$account = '012619468098'
$instance = 'i-0ee1235614219f089'
$aws = (Get-Command aws.exe -ErrorAction Stop).Source
$python = (Get-Command python.exe -ErrorAction Stop).Source
$kubectl = (Get-Command kubectl.exe -ErrorAction Stop).Source
$inputs = @('tools/Invoke-TokyoGatewayPolicy.ps1', 'tools/gateway-policy-proof', '.github/scripts/release_ci.py', '.github/workflows', 'kanz/infra')
& git -C $repo fetch origin main --quiet
if ($LASTEXITCODE -ne 0) { throw 'Could not refresh main.' }
$source = (& git -C $repo rev-parse origin/main).Trim()
$head = (& git -C $repo rev-parse HEAD).Trim()
if ($source -cnotmatch '^[0-9a-f]{40}$' -or $head -cne $source) { throw 'Rollout requires exact merged origin/main.' }
& git -C $repo diff --quiet $source -- @inputs
if ($LASTEXITCODE -ne 0) { throw 'Rollout source differs from main.' }
$untracked = @(& git -C $repo ls-files --others --exclude-standard -- @inputs)
if ($LASTEXITCODE -ne 0 -or $untracked.Count -ne 0) { throw 'Rollout inputs contain untracked files.' }
$proofRoot = Join-Path $PSScriptRoot 'gateway-policy-proof'
$sourceLoader = [IO.File]::ReadAllText((Join-Path $proofRoot 'source_loader.py'))
if ([Text.Encoding]::UTF8.GetByteCount($sourceLoader) -gt 65536) { throw 'Source loader exceeds bound.' }
$acceptance = & $python -I -B -c $sourceLoader $proofRoot preflight --source $source 2>$null
if ($LASTEXITCODE -ne 0 -or -not (($acceptance -join "`n") | ConvertFrom-Json).accepted) { throw 'Exact-main canonical workflow acceptance is incomplete.' }
$role = (& $aws configure get role_arn --profile $rolloutProfile).Trim()
$parentProfile = (& $aws configure get source_profile --profile $rolloutProfile).Trim()
if ($LASTEXITCODE -ne 0 -or $role -cne "arn:aws:iam::${account}:role/KanzPlatformProvisioner" -or $parentProfile -cne 'kanz-operator-process') { throw 'Unexpected operator profile chain.' }
$parentCaller = & $aws sts get-caller-identity --profile $parentProfile --region $region --output json 2>$null | ConvertFrom-Json
if ($LASTEXITCODE -ne 0 -or $parentCaller.Account -cne $account -or $parentCaller.Arn -cne "arn:aws:iam::${account}:user/kanz-operator") { throw 'Unexpected source operator principal.' }
$caller = & $aws sts get-caller-identity --profile $rolloutProfile --region $region --output json 2>$null | ConvertFrom-Json
if ($LASTEXITCODE -ne 0 -or $caller.Account -cne $account -or $caller.Arn -cne "arn:aws:sts::${account}:assumed-role/KanzPlatformProvisioner/kanz-platform-cli") { throw 'Unexpected rollout principal.' }
$node = & $aws ec2 describe-instances --profile $rolloutProfile --region $region --instance-ids $instance --query 'Reservations[0].Instances[0].{Id:InstanceId,State:State.Name}' --output json 2>$null | ConvertFrom-Json
if ($LASTEXITCODE -ne 0 -or $node.Id -cne $instance -or $node.State -cne 'running') { throw 'Fixed Tokyo instance differs.' }

# Only verified public digest/platform fields leave this resolver; anonymous
# registry tokens and OCI config bodies remain in its subprocess memory.
$identityRaw = & $python -I -B -c $sourceLoader $proofRoot image_identity 2>$null
if ($LASTEXITCODE -ne 0) { throw 'Public probe image identity could not be qualified.' }
$identity = ($identityRaw -join "`n") | ConvertFrom-Json
$render = & $kubectl kustomize --load-restrictor=LoadRestrictionsNone (Join-Path $repo 'kanz/infra/overlays/testnet-tokyo') 2>$null
if ($LASTEXITCODE -ne 0) { throw 'Canonical policy render failed.' }
$projector = @'
import sys,json,yaml
raw=sys.stdin.read(4194305)
assert len(raw)<=4194304
d=list(yaml.safe_load_all(raw))
p=[x for x in d if x and x.get("kind")=="NetworkPolicy"]
names={"accounting","audit","compliance","api-gateway","web-bff"}
w={}
for x in d:
 if x and x.get("kind")=="Deployment" and x["metadata"]["name"] in names:
  assert x["metadata"]["namespace"]=="kanz-services" and x["metadata"]["name"] not in w
  t=x["spec"]["template"]
  w[x["metadata"]["name"]]={"labels":t["metadata"]["labels"],"selector":x["spec"]["selector"],"images":{k:{c["name"]:c["image"] for c in t["spec"].get(k,[])} for k in ("containers","initContainers")}}
assert 3<=len(p)<=64 and set(w)==names
print(json.dumps({"policies":p,"workloads":w}))
'@
$projectionRaw = ($render -join "`n") | & $python -I -B -c $projector
if ($LASTEXITCODE -ne 0) { throw 'Rendered policy/workload projection failed.' }
$projection = ($projectionRaw -join "`n") | ConvertFrom-Json
$policies = $projection.policies
$files = [ordered]@{}
foreach ($name in @('transaction.py', 'policy_client.py', 'rollout_probe.py', 'rollout_engine.py', 'bounded_process.py', 'source_loader.py')) {
    $relative = "tools/gateway-policy-proof/$name"
    & git -C $repo ls-files --error-unmatch $relative *> $null
    if ($LASTEXITCODE -ne 0) { throw 'Rollout engine is not tracked.' }
    $text = [IO.File]::ReadAllText((Join-Path $repo $relative))
    if ([Text.Encoding]::UTF8.GetByteCount($text) -gt 65536) { throw 'Rollout source file exceeds bound.' }
    $files[$name] = $text
}
$bundle = [ordered]@{ files = $files; payload = [ordered]@{ source = $source; policies = $policies; identity = $identity; workloads = $projection.workloads } } | ConvertTo-Json -Depth 60 -Compress
$bytes = [Text.Encoding]::UTF8.GetBytes($bundle)
if ($bytes.Length -gt 524288) { throw 'Rollout bundle exceeds bound.' }
$memory = New-Object IO.MemoryStream
$gzip = New-Object IO.Compression.GZipStream($memory, [IO.Compression.CompressionMode]::Compress, $true)
try { $gzip.Write($bytes, 0, $bytes.Length) } finally { $gzip.Dispose() }
$encoded = [Convert]::ToBase64String($memory.ToArray())
$memory.Dispose()
if ($encoded.Length -gt 20000) { throw 'Rollout transfer exceeds bounded SSM payload.' }
$bootstrap = @'
import base64,gzip,io,json,os,pathlib,sys
raw=gzip.GzipFile(fileobj=io.BytesIO(base64.b64decode(sys.argv[1],validate=True))).read(524289)
if len(raw)>524288: raise ValueError('bundle bound')
b=json.loads(raw); names={'transaction.py','policy_client.py','rollout_probe.py','rollout_engine.py','bounded_process.py','source_loader.py'}
if set(b)!={'files','payload'} or set(b['files'])!=names: raise ValueError('bundle shape')
root=pathlib.Path(sys.argv[2])
for name,text in b['files'].items():
 data=text.encode()
 if len(data)>65536: raise ValueError('file bound')
 with (root/name).open('xb') as f:f.write(data)
with (root/'input.json').open('x') as f:json.dump(b['payload'],f)
'@
$bootstrapEncoded = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($bootstrap))
$applyArg = if ($Apply) { ' --apply' } else { '' }
$commands = @('set -eu', 'umask 077', 'work=$(mktemp -d /var/tmp/kanz-gateway-source.XXXXXXXX)',
    'trap ''rm -f "$work/transaction.py" "$work/policy_client.py" "$work/rollout_probe.py" "$work/rollout_engine.py" "$work/bounded_process.py" "$work/source_loader.py" "$work/input.json" "$work/bootstrap.py"; rmdir "$work"'' EXIT',
    ("printf '%s' '$bootstrapEncoded' | base64 -d > `"`$work/bootstrap.py`""),
    ("python3 -I -B `"`$work/bootstrap.py`" '$encoded' `"`$work`""),
    'chmod 400 "$work"/*.py "$work/input.json"',
    ("timeout --signal=TERM --kill-after=180s 1200s env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin HOME=`"`$work`" TMPDIR=`"`$work`" LANG=C.UTF-8 python3 -I -B `"`$work/source_loader.py`" `"`$work`" rollout_engine --tokyo --source $source$applyArg < `"`$work/input.json`""))
$parameters = @{ executionTimeout = @('1500'); commands = $commands } | ConvertTo-Json -Compress -Depth 5
$latest = @(& git -C $repo ls-remote origin refs/heads/main)
if ($LASTEXITCODE -ne 0 -or $latest.Count -ne 1 -or ($latest[0] -split '\s+')[0] -cne $source) { throw 'Canonical main advanced before SSM; repeat source acceptance.' }
$command = (& $aws ssm send-command --profile $rolloutProfile --region $region --instance-ids $instance --document-name AWS-RunShellScript --comment "Kanz four-policy qualification $source" --parameters $parameters --query Command.CommandId --output text 2>$null).Trim()
if ($LASTEXITCODE -ne 0 -or $command -cnotmatch '^[0-9a-f-]{36}$') { throw 'SSM rollout request refused.' }
$end = [DateTime]::UtcNow.AddSeconds(1520)
do {
    Start-Sleep -Seconds 3
    $raw = & $aws ssm get-command-invocation --profile $rolloutProfile --region $region --command-id $command --instance-id $instance --query '{Status:Status,Output:StandardOutputContent}' --output json 2>$null
    if ($LASTEXITCODE -eq 0) { $result = ($raw -join "`n") | ConvertFrom-Json }
    if ($result -and $result.Status -in @('Success', 'Failed', 'TimedOut', 'Cancelled')) { break }
} while ([DateTime]::UtcNow -lt $end)
if (-not $result -or $result.Status -ne 'Success') { throw "Policy qualification did not succeed. Retain the node's private transaction receipts; do not retry or force rollback. SSM command: $command" }
$report = $result.Output.Trim() | ConvertFrom-Json
$mode = if ($Apply) { 'apply' } else { 'dry_run' }
$phase = if ($Apply) { 'verified' } else { 'dry_run_passed' }
if ($report.format -cne 'kanz-tokyo-gateway-policy-v1' -or $report.source -cne $source -or $report.mode -cne $mode -or $report.phase -cne $phase -or ($Apply -and $report.receipt -cnotmatch '^/var/tmp/kanz-gateway-policy-[a-zA-Z0-9_]+$') -or (-not $Apply -and $null -ne $report.receipt) -or ($Apply -and $report.cleanup -cne 'verified')) { throw 'Rollout result violates the exact source/cleanup contract.' }
$report | ConvertTo-Json -Compress
