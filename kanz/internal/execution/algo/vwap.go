package algo

// VWAP — volume-weighted average price (#869).
//
// # What it is
//
// The parent is divided across the window in proportion to the volume expected to
// trade in each slice's own interval, so the order is large when the market is and
// small when it is not. It is the benchmark most institutional executions are
// measured against, and until this landed the platform could neither target it nor
// report against it: every parent was worked on a wall-clock grid that met the
// busiest half-hour of a session with the same size it used at its quietest.
//
// # It REFUSES on an UNKNOWN profile — it does not degrade to TWAP
//
// This is the one property #869 states as non-negotiable, and it is why VWAP is
// a handful of lines on top of expectedVolumes rather than a schedule with a
// fallback. A flat curve is not a neutral default: a flat curve IS TWAP. So a
// VWAP order scheduled against one is a TWAP order wearing the wrong name — the
// fills arrive, the parent completes, and #866's attribution decomposes the
// shortfall of an algorithm that never ran. Every reader downstream is then
// reading a label nobody can defend, and nothing anywhere says so.
//
// Refusing is louder and cheaper: the order does not exist, the desk is told which
// interval could not be sized, and nothing traded.
//
// # What VWAP is NOT
//
// It is not a participation control. Its participation in any interval is whatever
// Total/V happens to be, and on a thin instrument that can be most of the tape —
// which is exactly the exposure POV exists to bound. An operator who needs "never
// more than n% of prints" is asking for POV, and asking for it is how they get it:
// VWAP does not read Plan.MaxParticipation, because an algorithm that silently
// enforced a cap the operator did not select would be a second control nobody
// could see.
const NameVWAP Name = "VWAP"

// vwapAlgo is VWAP's registration.
type vwapAlgo struct{}

// Name is NameVWAP.
func (vwapAlgo) Name() Name { return NameVWAP }

// Schedule derives the children.
//
// PARENT STATE IS NOT READ, and that is the algorithm rather than an omission.
// The allocation is a function of the parent and of the forecast curve; what has
// already been sent does not change what slice 7 should be, and reading it would
// make the schedule depend on the order the driver happened to send in — the
// non-determinism test/arch/schedule_is_derived_test.go exists to refuse.
func (vwapAlgo) Schedule(p Plan, _ ParentState, mkt MarketView) ([]Slice, error) {
	return vwap(p, mkt)
}

// vwap is the arithmetic, unexported.
//
// DELIBERATELY NOT AN EXPORTED FREE FUNCTION, unlike TWAP. TWAP is exported
// because it predates the seam and because its closed-form output is the oracle
// the seam was measured against — and test/arch/every_algo_is_reachable_test.go
// has to spend an entire arm forbidding the rest of the module from calling it,
// because a caller that does has re-hardwired the algorithm the ORDER names. A
// planner with no exported name cannot be reached around the registry at all,
// which is the same property without the guard.
func vwap(p Plan, mkt MarketView) ([]Slice, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	vols, total, err := expectedVolumes(p, mkt)
	if err != nil {
		return nil, err
	}
	return allocateProportional(p, vols, total)
}
