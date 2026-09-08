def oms_env:
  (.deployment.spec.template.spec.containers | map(select(.name == "oms"))) as $containers
  | if ($containers | length) != 1 then error("expected exactly one oms container") else
      ($containers[0].env // []
       | map(select(has("value")))
       | map({key: .name, value: .value})
       | from_entries)
    end;

def failures:
  oms_env as $env
  | [
      (if $env.OMS_REQUIRE_MANDATE != "true" then "mandate enforcement is not armed" else empty end),
      (if $env.OMS_REQUIRE_VENUE_ACCOUNT != "true" then "venue-account binding is not armed" else empty end),
      (if $env.OMS_REQUIRE_VERIFIED_ACCOUNT != "true" then "exchange account proof is not armed" else empty end),
      (if $env.OMS_REQUIRE_DUAL_CONTROL != "true" then "dual control is not armed" else empty end),
      (if (($env.OMS_DUAL_CONTROL_MIN_NOTIONAL // "") | length) == 0 then "dual-control threshold is absent" else empty end),
      (if (($env.OMS_VENUE_ACCOUNTS // "") | length) == 0 then "portfolio-to-account bindings are absent" else empty end),
      (if (.pods.items | length) != 1 then "expected exactly one OMS Pod" else empty end),
      (if (.pods.items | all(.status.phase == "Running" and (.status.containerStatuses | any(.name == "oms" and .ready == true)))) | not then "OMS Pod is not Running and Ready" else empty end),
      (if .recent_logs | test("MANDATE ADVISORY|COLLATERAL IS SHARED|UNVERIFIED account|NO DUAL-CONTROL|did not report part of this account's margin state"; "i") then "recent OMS logs contain an unsafe go-live posture" else empty end)
    ];

failures as $failures
| if ($failures | length) == 0
  then "testnet-oms-go-live-preflight-ok"
  else error($failures | join("; "))
  end
