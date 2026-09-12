[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^i-[0-9a-f]+$')]
    [string]$InstanceId,

    [ValidateSet('ap-northeast-1')]
    [string]$Region = 'ap-northeast-1',

    [string]$Profile = 'kanz-platform',

    # Persist the exact result for the authenticated web status page. This only
    # updates a non-secret evidence ConfigMap; it never publishes a resume FACT,
    # changes OMS state, or submits an order. Without the switch the preflight
    # remains completely read-only, as before.
    [switch]$PublishWebEvidence
)

$ErrorActionPreference = 'Stop'
$aws = (Get-Command aws.exe -ErrorAction Stop).Source
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$verifierPath = Join-Path $repoRoot 'tools\Verify-TestnetOmsGoLive.jq'

& git -C $repoRoot fetch origin main --quiet
if ($LASTEXITCODE -ne 0) { throw 'Could not fetch origin/main.' }
$releaseCommit = (& git -C $repoRoot rev-parse origin/main).Trim()
$headCommit = (& git -C $repoRoot rev-parse HEAD).Trim()
if ($headCommit -ne $releaseCommit) {
    throw "OMS go-live verification is refused: HEAD $headCommit is not exact origin/main $releaseCommit."
}
& git -C $repoRoot diff --quiet $releaseCommit -- tools/Test-TestnetOmsGoLive.ps1 tools/Verify-TestnetOmsGoLive.jq
if ($LASTEXITCODE -ne 0) {
    throw 'OMS go-live verification is refused: verifier inputs differ from merged origin/main.'
}

$verifier = Get-Content -LiteralPath $verifierPath -Raw
if ([string]::IsNullOrWhiteSpace($verifier)) { throw 'The OMS go-live verifier is empty.' }
$payload = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($verifier))
$remote = "/var/tmp/kanz-oms-go-live-$([Guid]::NewGuid().ToString('N'))"
$parameters = @{ commands = @(
    'set -eu',
    "base='$remote'",
    'finish() { rc=$?; rm -f "${base}.jq" "${base}-deployment.json" "${base}-gateway.json" "${base}-binance.json" "${base}-okx.json" "${base}-pods.json" "${base}-logs.txt"; exit "${rc}"; }',
    'trap finish EXIT',
    ("printf '%s' '$payload' | base64 -d > " + '"${base}.jq"'),
    '/usr/local/bin/k3s kubectl -n kanz-services get deployment oms -o json > "${base}-deployment.json"',
    '/usr/local/bin/k3s kubectl -n kanz-services get deployment api-gateway -o json > "${base}-gateway.json"',
    '/usr/local/bin/k3s kubectl -n kanz-services get deployment venue-binance -o json > "${base}-binance.json"',
    '/usr/local/bin/k3s kubectl -n kanz-services get deployment venue-okx -o json > "${base}-okx.json"',
    '/usr/local/bin/k3s kubectl -n kanz-services get pods -l app=oms -o json > "${base}-pods.json"',
    '/usr/local/bin/k3s kubectl -n kanz-services logs deployment/oms -c oms --since=2m --tail=1000 > "${base}-logs.txt" 2>&1',
    'pgpod=$(/usr/local/bin/k3s kubectl -n kanz-data get pod -l cnpg.io/cluster=kanz-testnet-postgres,role=primary -o jsonpath=''{.items[0].metadata.name}'')',
    'portfolio_count=$(/usr/local/bin/k3s kubectl -n kanz-data exec "${pgpod}" -c postgres -- psql -U postgres -d risk_engine -Atqc ''select count(*) from portfolios'')',
    'trade_role=$(jq -r ''[.spec.template.spec.containers[] | select(.name == "api-gateway") | .env[]? | select(.name == "API_GATEWAY_TRADE_ROLE") | .value][0] // ""'' "${base}-gateway.json")',
    'approve_role=$(jq -r ''[.spec.template.spec.containers[] | select(.name == "api-gateway") | .env[]? | select(.name == "API_GATEWAY_APPROVE_ROLE") | .value][0] // ""'' "${base}-gateway.json")',
    'mandate_role=$(jq -r ''[.spec.template.spec.containers[] | select(.name == "api-gateway") | .env[]? | select(.name == "API_GATEWAY_MANDATE_ROLE") | .value][0] // ""'' "${base}-gateway.json")',
    'trade_query_role=${trade_role:-__absent__}; approve_query_role=${approve_role:-__absent__}; mandate_query_role=${mandate_role:-__absent__}',
    'for role in "${trade_query_role}" "${approve_query_role}" "${mandate_query_role}"; do printf ''%s\n'' "${role}" | grep -Eq ''^[a-zA-Z0-9_-]+$'' || { printf ''invalid gateway role\n'' >&2; exit 1; }; done',
    'identity=$(/usr/local/bin/k3s kubectl -n kanz-data exec "${pgpod}" -c postgres -- psql -U postgres -d identity -Atqc "select json_build_object(''active_users'', (select count(*) from identity_users where status=''active''), ''active_users_with_portfolios'', (select count(*) from identity_users where status=''active'' and cardinality(portfolios)>0), ''maker_checker_pairs'', (select count(*) from identity_users maker join identity_users checker on maker.tenant_id=checker.tenant_id and maker.subject<>checker.subject where maker.status=''active'' and checker.status=''active'' and ''${trade_query_role}''=any(maker.roles) and ''${approve_query_role}''=any(checker.roles)), ''mandate_signatory_pairs'', (select count(*) from identity_users proposer join identity_users signer on proposer.tenant_id=signer.tenant_id and proposer.subject<signer.subject where proposer.status=''active'' and signer.status=''active'' and ''${mandate_query_role}''=any(proposer.roles) and ''${mandate_query_role}''=any(signer.roles)))")',
    'venue_proof=$(jq -n --slurpfile binance "${base}-binance.json" --slurpfile okx "${base}-okx.json" ''def configured($e; $name): ([$e[] | select(.name==$name) | (((.value // "")|length)>0 or has("valueFrom"))][0] // false); def proof($d; $uid; $allow): ($d.spec.template.spec.containers[0].env // []) as $e | {expected_uid_configured: (configured($e; $uid) or configured($e; ($uid + "_FILE"))), allow_unverified: ([$e[] | select(.name==$allow) | .value][0] // "absent")}; {binance: proof($binance[0]; "BINANCE_VENUE_ACCOUNT_UID"; "BINANCE_ALLOW_UNVERIFIED_ACCOUNT"), okx: proof($okx[0]; "OKX_VENUE_ACCOUNT_UID"; "OKX_ALLOW_UNVERIFIED_ACCOUNT")}'' )',
    'nats_ip=$(/usr/local/bin/k3s kubectl -n kanz-messaging get pod nats-0 -o jsonpath=''{.status.podIP}'')',
    'mandate_count=$(curl -fsS --connect-timeout 3 --max-time 5 "http://${nats_ip}:8222/jsz?streams=true&accounts=true" | jq ''[.account_details[]?.stream_detail[]? | select(.name == "MANDATE") | .state.messages] | if length == 1 then .[0] else error("expected one MANDATE stream") end'')',
    'oms_metrics=$(/usr/local/bin/k3s kubectl get --raw /api/v1/namespaces/kanz-services/services/http:oms:8090/proxy/metrics)',
    'report=$(jq -n --slurpfile deployment "${base}-deployment.json" --slurpfile api_gateway "${base}-gateway.json" --slurpfile binance "${base}-binance.json" --slurpfile okx "${base}-okx.json" --slurpfile pods "${base}-pods.json" --rawfile recent_logs "${base}-logs.txt" --argjson portfolio_count "${portfolio_count}" --argjson mandate_count "${mandate_count}" --argjson identity "${identity}" --argjson venue_proof "${venue_proof}" --arg oms_metrics "${oms_metrics}" ''{deployment: $deployment[0], api_gateway: $api_gateway[0], binance: $binance[0], okx: $okx[0], pods: $pods[0], recent_logs: $recent_logs, portfolio_count: $portfolio_count, mandate_count: $mandate_count, identity: $identity, venue_proof: $venue_proof, oms_metrics: $oms_metrics}'' | jq -c -f "${base}.jq")',
    'printf ''%s\n'' "${report}"',
    'printf ''%s'' "${report}" | jq -e ''.verdict == "PASS"'' >/dev/null'
) } | ConvertTo-Json -Compress -Depth 4

$commandId = (& $aws ssm send-command --profile $Profile --region $Region `
    --instance-ids $InstanceId --document-name AWS-RunShellScript `
    --comment 'Verify Kanz OMS controls before testnet resume' --parameters $parameters `
    --query 'Command.CommandId' --output text).Trim()
if ($LASTEXITCODE -ne 0 -or -not $commandId) { throw 'AWS rejected the OMS go-live verification command.' }

$deadline = [DateTime]::UtcNow.AddMinutes(3)
do {
    Start-Sleep -Milliseconds 750
    $result = & $aws ssm get-command-invocation --profile $Profile --region $Region `
        --command-id $commandId --instance-id $InstanceId `
        --query '{Status:Status,Output:StandardOutputContent,Error:StandardErrorContent}' `
        --output json 2>$null | ConvertFrom-Json
    if ($result.Status -in @('Success', 'Cancelled', 'Failed', 'TimedOut')) { break }
} while ([DateTime]::UtcNow -lt $deadline)

if (-not $result) {
    throw "OMS go-live verification $commandId returned no terminal SSM result."
}
Write-Output "SSM command: $commandId"
if (-not [string]::IsNullOrWhiteSpace($result.Output)) {
    Write-Output $result.Output.Trim()
}

if ($PublishWebEvidence) {
    $reportLine = @($result.Output -split "`r?`n" | Where-Object { -not [string]::IsNullOrWhiteSpace($_) }) | Select-Object -Last 1
    try {
        $report = $reportLine | ConvertFrom-Json
    } catch {
        throw "OMS go-live verification $commandId returned no publishable JSON evidence."
    }
    if ($report.verdict -notin @('PASS', 'FAIL') -or -not $report.deployed_commit) {
        throw "OMS go-live verification $commandId returned incomplete evidence; it was not published."
    }
    $evidence = [ordered]@{
        format = 'kanz-oms-preflight-v1'
        observed_at = [DateTime]::UtcNow.ToString('o')
        verifier_commit = $releaseCommit
        deployed_commit = $report.deployed_commit
        command_id = $commandId
        verdict = $report.verdict
        checks = $report.checks
        workload_images = $report.workload_images
    } | ConvertTo-Json -Compress -Depth 20
    $evidencePayload = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($evidence))
    $publishParameters = @{ commands = @(
        'set -eu',
        "evidence='/var/tmp/kanz-oms-preflight-evidence-$([Guid]::NewGuid().ToString('N')).json'",
        'finish() { rm -f "${evidence}"; }',
        'trap finish EXIT',
        "printf '%s' '$evidencePayload' | base64 -d > " + '"${evidence}"',
        '/usr/local/bin/k3s kubectl -n kanz-services create configmap kanz-oms-preflight-evidence --from-file=evidence.json="${evidence}" --dry-run=client -o yaml | /usr/local/bin/k3s kubectl apply -f -'
    ) } | ConvertTo-Json -Compress -Depth 4
    $publishID = (& $aws ssm send-command --profile $Profile --region $Region `
        --instance-ids $InstanceId --document-name AWS-RunShellScript `
        --comment 'Publish non-secret Kanz OMS preflight evidence for kanz-web' `
        --parameters $publishParameters --query 'Command.CommandId' --output text).Trim()
    if ($LASTEXITCODE -ne 0 -or -not $publishID) { throw 'AWS rejected the web evidence publication command.' }
    & $aws ssm wait command-executed --profile $Profile --region $Region --command-id $publishID --instance-id $InstanceId
    if ($LASTEXITCODE -ne 0) { throw "Web evidence publication $publishID did not complete successfully." }
    Write-Output "Web evidence published: $publishID"
}
if ($result.Status -ne 'Success') {
    $detail = $result.Error.Trim()
    throw "OMS go-live verification $commandId refused resume: $detail"
}
