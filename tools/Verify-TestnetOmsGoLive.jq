def workload_env($deployment; $container):
  ($deployment.spec.template.spec.containers | map(select(.name == $container))) as $containers
  | if ($containers | length) != 1 then error("expected exactly one " + $container + " container") else
      ($containers[0].env // []
       | map(select(has("value")))
       | map({key: .name, value: .value})
       | from_entries)
    end;

def oms_env: workload_env(.deployment; "oms");
def gateway_env: workload_env(.api_gateway; "api-gateway");

def release_commit($deployment): ($deployment.metadata.annotations["kanz.io/release-commit"] // "");
def workload_image($deployment; $container):
  [$deployment.spec.template.spec.containers[] | select(.name == $container) | .image]
  | if length == 1 then .[0] else "" end;

def metric($name):
  [.oms_metrics
   | split("\n")[]
   | select(startswith($name + " "))
   | split(" ")[1]
   | tonumber] as $values
  | if ($values | length) == 1 then $values[0] else null end;

def bindings($env):
  (($env.OMS_VENUE_ACCOUNTS // "") | split(",") | map(select(length > 0)));

def check($name; $pass; $observed):
  {name: $name, pass: $pass, observed: $observed};

oms_env as $env
| gateway_env as $gateway
| bindings($env) as $bindings
| ($bindings | map(split("@")[0]) | unique) as $bound_portfolios
| (metric("kanz_oms_unverified_venue_account_total")) as $unverified
| (metric("kanz_oms_venue_margin_accounts_uncovered")) as $uncovered
| (metric("kanz_oms_venue_margin_accounts_current")) as $margin_current
| [release_commit(.deployment), release_commit(.api_gateway), release_commit(.binance), release_commit(.okx)] as $release_commits
| [
    check("deployed_release_identity";
      ($release_commits | all(test("^[0-9a-f]{40}$"))) and ($release_commits | unique | length) == 1;
      {commit: ($release_commits[0] // "absent"), consistent: (($release_commits | unique | length) == 1)}),
    check("mandate_enforcement"; $env.OMS_REQUIRE_MANDATE == "true";
      ($env.OMS_REQUIRE_MANDATE // "absent")),
    check("mandate_change_surface";
      (($gateway.API_GATEWAY_COMPLIANCE_ADDR // "") | length) > 0 and
      (($gateway.API_GATEWAY_MANDATE_ROLE // "") | length) > 0 and
      $gateway.API_GATEWAY_MANDATE_ROLE != $gateway.API_GATEWAY_APPROVE_ROLE;
      {compliance_upstream: ((($gateway.API_GATEWAY_COMPLIANCE_ADDR // "") | length) > 0),
       mandate_role: ((($gateway.API_GATEWAY_MANDATE_ROLE // "") | length) > 0),
       role_separated: ($gateway.API_GATEWAY_MANDATE_ROLE != $gateway.API_GATEWAY_APPROVE_ROLE)}),
    check("mandate_distinct_signatory_pairs";
      .identity.mandate_signatory_pairs > 0;
      .identity.mandate_signatory_pairs),
    check("portfolio_inventory"; .portfolio_count > 0; .portfolio_count),
    check("mandate_inventory";
      .portfolio_count > 0 and .mandate_count == .portfolio_count;
      {portfolios: .portfolio_count, compacted_mandates: .mandate_count}),
    check("venue_account_enforcement"; $env.OMS_REQUIRE_VENUE_ACCOUNT == "true";
      ($env.OMS_REQUIRE_VENUE_ACCOUNT // "absent")),
    check("portfolio_account_binding_coverage";
      .portfolio_count > 0 and ($bound_portfolios | length) == .portfolio_count;
      {portfolios: .portfolio_count, bound_portfolios: ($bound_portfolios | length), bindings: ($bindings | length)}),
    check("exchange_account_proof_enforcement";
      $env.OMS_REQUIRE_VERIFIED_ACCOUNT == "true";
      ($env.OMS_REQUIRE_VERIFIED_ACCOUNT // "absent")),
    check("exchange_account_proof_sources";
      .venue_proof.binance.expected_uid_configured and
      (.venue_proof.binance.allow_unverified == "false") and
      .venue_proof.okx.expected_uid_configured and
      (.venue_proof.okx.allow_unverified == "false");
      .venue_proof),
    check("exchange_accounts_observed_verified";
      $unverified == 0;
      (if $unverified == null then "metric absent" else $unverified end)),
    check("okx_margin_observation_current";
      $margin_current != null and $margin_current > 0;
      (if $margin_current == null then "metric absent" else $margin_current end)),
    check("okx_margin_observation_complete";
      $uncovered == 0;
      (if $uncovered == null then "metric absent" else $uncovered end)),
    check("dual_control_enforcement"; $env.OMS_REQUIRE_DUAL_CONTROL == "true";
      ($env.OMS_REQUIRE_DUAL_CONTROL // "absent")),
    check("dual_control_threshold";
      (($env.OMS_DUAL_CONTROL_MIN_NOTIONAL // "") | length) > 0;
      ($env.OMS_DUAL_CONTROL_MIN_NOTIONAL // "absent")),
    check("order_approval_surface";
      (($gateway.API_GATEWAY_APPROVE_ROLE // "") | length) > 0 and
      (($gateway.API_GATEWAY_OMS_READ_ADDR // "") | length) > 0;
      {approve_role: ((($gateway.API_GATEWAY_APPROVE_ROLE // "") | length) > 0),
       pending_queue_upstream: ((($gateway.API_GATEWAY_OMS_READ_ADDR // "") | length) > 0)}),
    check("distinct_maker_checker_pairs";
      .identity.maker_checker_pairs > 0;
      .identity.maker_checker_pairs),
    check("oms_singleton_ready";
      (.pods.items | length) == 1 and
      (.pods.items | all(.status.phase == "Running" and
        (.status.containerStatuses | any(.name == "oms" and .ready == true))));
      {pods: (.pods.items | length), ready: [.pods.items[] |
        (.status.containerStatuses // [] | any(.name == "oms" and .ready == true))]}),
    check("recent_control_posture";
      (.recent_logs | test("MANDATE ADVISORY|COLLATERAL IS SHARED|UNVERIFIED account|NO DUAL-CONTROL|did not report part of this account's margin state"; "i") | not);
      (if (.recent_logs | test("MANDATE ADVISORY|COLLATERAL IS SHARED|UNVERIFIED account|NO DUAL-CONTROL|did not report part of this account's margin state"; "i"))
       then "unsafe warning present" else "no matching warning" end))
  ] as $checks
| {
    verdict: (if ($checks | all(.pass)) then "PASS" else "FAIL" end),
    deployed_commit: ($release_commits[0] // ""),
    workload_images: {
      oms: workload_image(.deployment; "oms"),
      api_gateway: workload_image(.api_gateway; "api-gateway"),
      venue_binance: workload_image(.binance; "venue-binance"),
      venue_okx: workload_image(.okx; "venue-okx")
    },
    checks: $checks
  }
