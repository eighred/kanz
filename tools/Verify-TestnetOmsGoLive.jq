def oms_env:
  (.deployment.spec.template.spec.containers | map(select(.name == "oms"))) as $containers
  | if ($containers | length) != 1 then error("expected exactly one oms container") else
      ($containers[0].env // []
       | map(select(has("value")))
       | map({key: .name, value: .value})
       | from_entries)
    end;

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
| bindings($env) as $bindings
| ($bindings | map(split("@")[0]) | unique) as $bound_portfolios
| (metric("kanz_oms_unverified_venue_account_total")) as $unverified
| (metric("kanz_oms_venue_margin_accounts_uncovered")) as $uncovered
| (metric("kanz_oms_venue_margin_accounts_current")) as $margin_current
| [
    check("mandate_enforcement"; $env.OMS_REQUIRE_MANDATE == "true";
      ($env.OMS_REQUIRE_MANDATE // "absent")),
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
| {verdict: (if ($checks | all(.pass)) then "PASS" else "FAIL" end), checks: $checks}
