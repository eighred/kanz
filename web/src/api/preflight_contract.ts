// Code generated from the BFF preflight evidence requirements. DO NOT EDIT.
export const preflightChecks = [
  "deployed_release_identity",
  "mandate_enforcement",
  "mandate_change_surface",
  "mandate_distinct_signatory_pairs",
  "portfolio_inventory",
  "mandate_inventory",
  "venue_account_enforcement",
  "portfolio_account_binding_coverage",
  "exchange_account_proof_enforcement",
  "exchange_account_proof_sources",
  "exchange_accounts_observed_verified",
  "okx_margin_observation_current",
  "okx_margin_observation_complete",
  "dual_control_enforcement",
  "dual_control_threshold",
  "order_approval_surface",
  "distinct_maker_checker_pairs",
  "oms_singleton_ready",
  "recent_control_posture",
] as const
export const preflightWorkloads = [
  "oms",
  "api_gateway",
  "venue_binance",
  "venue_okx",
] as const
