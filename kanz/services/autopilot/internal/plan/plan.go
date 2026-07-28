// Package plan is the default control policy: which conditions the autopilot
// recognizes (the Matcher rules) and the runbook each maps to (the Registry).
// Kept separate from both the controller and the actions so the policy is one
// readable place — the closest thing to a declarative "what does autopilot do"
// table — and so main and the AUTO-01e tests build the exact same wiring.
package plan

import (
	"github.com/eighred/kanz/services/autopilot/internal/actuate"
	"github.com/eighred/kanz/services/autopilot/internal/remediate"
	"github.com/eighred/kanz/services/autopilot/internal/runbook"
	"github.com/eighred/kanz/services/autopilot/internal/signal"
)

// Conditions the default policy recognizes.
const (
	CondDataGapCritical      = "data_gap_critical"
	CondReconcileDivergence  = "reconcile_divergence"
	CondModelDrift           = "model_drift"
	CondIngestStaleness      = "ingest_staleness"
	CondInferenceCircuitOpen = "inference_circuit_open"
	CondSLOBurn              = "slo_burn"
)

// Deps are the remediation/actuation seams the runbooks act through.
type Deps struct {
	Quarantiner remediate.Quarantiner
	ModelRoller remediate.ModelRoller
	Scaler      actuate.Scaler
	Failover    actuate.Failover
}

// DefaultMatcher recognizes the operational conditions. The severity floors keep
// remediation conservative: only CRITICAL data/drift/staleness/SLO conditions
// auto-remediate, so a WARNING is left to escalation (a human decides). A
// reconciliation divergence or a tripped circuit is actionable at WARNING — both
// already mean something concrete diverged/failed.
func DefaultMatcher() *runbook.Matcher {
	return runbook.NewMatcher(
		runbook.Rule{Kind: signal.KindDataGap, MinSeverity: signal.SeverityCritical, Condition: CondDataGapCritical},
		runbook.Rule{Kind: signal.KindReconcileDivergence, MinSeverity: signal.SeverityWarning, Condition: CondReconcileDivergence},
		runbook.Rule{Kind: signal.KindDrift, MinSeverity: signal.SeverityCritical, Condition: CondModelDrift},
		runbook.Rule{Kind: signal.KindStaleness, MinSeverity: signal.SeverityCritical, Condition: CondIngestStaleness},
		runbook.Rule{Kind: signal.KindCircuitOpen, MinSeverity: signal.SeverityWarning, Condition: CondInferenceCircuitOpen},
		runbook.Rule{Kind: signal.KindSLOBurn, MinSeverity: signal.SeverityCritical, Condition: CondSLOBurn},
	)
}

// DefaultRegistry binds each condition to its runbook. autoFailover gates the
// most drastic, hardest-to-reverse action (a DR-01d regional failover): off by
// default, a sustained critical SLO burn only auto-scales and otherwise escalates
// for a human to decide the failover (AUTO-01d human-in-the-loop). With it on,
// the SLO-burn runbook scales out AND fails the region over autonomously.
func DefaultRegistry(d Deps, autoFailover bool) *runbook.Registry {
	r := runbook.NewRegistry()

	// AUTO-01b data-plane remediation.
	r.Register(&runbook.Runbook{Condition: CondDataGapCritical, Steps: []runbook.Action{
		remediate.QuarantineAction(d.Quarantiner),
	}})
	r.Register(&runbook.Runbook{Condition: CondReconcileDivergence, Steps: []runbook.Action{
		remediate.QuarantineAction(d.Quarantiner),
	}})
	r.Register(&runbook.Runbook{Condition: CondModelDrift, Steps: []runbook.Action{
		remediate.RollbackAction(d.ModelRoller),
	}})

	// AUTO-01c infrastructure actuation.
	r.Register(&runbook.Runbook{Condition: CondIngestStaleness, Steps: []runbook.Action{
		actuate.ScaleOutAction(d.Scaler, "market-data"),
	}})
	r.Register(&runbook.Runbook{Condition: CondInferenceCircuitOpen, Steps: []runbook.Action{
		actuate.ScaleOutAction(d.Scaler, "inference"),
	}})

	sloSteps := []runbook.Action{actuate.ScaleOutAction(d.Scaler, "risk-engine")}
	if autoFailover {
		sloSteps = append(sloSteps, actuate.FailoverAction(d.Failover))
	}
	r.Register(&runbook.Runbook{Condition: CondSLOBurn, Steps: sloSteps})

	return r
}
