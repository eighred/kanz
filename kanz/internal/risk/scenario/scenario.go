// Package scenario implements the risk engine's what-if evaluator
// (RISK-09). Scenarios are PURE FUNCTIONS of (current state, shocks):
// the input Portfolio is never mutated, the engine deep-clones it
// into a transient working copy, applies the shocks, and recomputes
// measures against the shocked state.
//
// # Why the shock concrete types live in api/v1
//
// External callers (the orchestrator's HTTP API, scenario tooling)
// need to CONSTRUCT shocks to drive Engine.EvaluateScenario, but
// RISK-02's arch test forbids them from importing this package. So
// the value types (PriceShock, ParallelShift) live in api/v1 where
// outsiders can reach them; this package owns the DISPATCH + APPLY
// logic, type-asserting on the api/v1 types it knows. An unknown shock type
// records SkipUnknownShockType so the engine refuses the projection: a caller
// and engine that disagree on the vocabulary cannot claim a stress ran.
//
// # Why a deep clone, not a shallow one
//
// Portfolio holds a positions map; SetPosition would mutate it in
// place. We need the original portfolio untouched (the engine is
// serving many concurrent queries plus its ingest goroutine —
// RISK-05's per-portfolio lock is held by the orchestrator for the
// duration of Evaluate, but mutation of the live portfolio under
// that lock would still be wrong because subsequent queries inside
// the same lock-window would see the shocked state). Cheap to
// clone in absolute terms — typical portfolios hold tens to
// hundreds of positions, value-copied.
package scenario

import (
	"context"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/compute/factor"
	"github.com/eighred/kanz/internal/risk/domain"
)

// Option customizes an Evaluate call. Variadic so existing two-positional
// callers (and the api/v1 contract) are unaffected.
type Option func(*evalConfig)

type evalConfig struct {
	classifier factor.Classifier
	revaluer   Revaluer
}

// WithClassifier wires the MODEL-01f factor model so SectorShocks can resolve
// each position's sector at apply time. Without it a SectorShock CANNOT BE
// APPLIED, and Evaluate reports that through its coverage rather than returning
// the unshocked book (#640).
func WithClassifier(c factor.Classifier) Option {
	return func(cfg *evalConfig) { cfg.classifier = c }
}

// WithRevaluer wires the DERIV-01e option pricer so a VolShock can move
// something. It is the SECOND CLASS OF SHOCK made explicit: PriceShock,
// ParallelShift and SectorShock are MarketValue arithmetic and the linear path
// expresses them exactly, while a vol bump has no linear expression at all — it
// reaches a book only by repricing options through vega.
//
// PRESENT ⇒ Evaluate routes through the full-revaluation clone and the VolShock
// is real. ABSENT ⇒ applyShock records SkipNoRevaluer and the request is
// refused, rather than answering with the unshocked book (#1035).
//
// It is an Option rather than a second entry point on purpose: EvaluateReval
// used to be that second path and had no caller, so the day a Revaluer becomes
// constructible the wiring is one line at the risk-engine composition root
// beside WithClassifier, not a fork in the engine.
func WithRevaluer(r Revaluer) Option {
	return func(cfg *evalConfig) { cfg.revaluer = r }
}

// Reasons a shock could not be applied, in the closed-vocabulary form
// v1.InputExclusion requires (compute.SkipNoTerms, SkipNoModel, ... are the
// measure-side siblings). They are metric and audit labels, so they are
// constants here rather than free text at the call site.
const (
	// SkipNoClassifier: no factor.Classifier is wired, so NO position's sector
	// can be resolved and a SectorShock reaches nothing. A whole-evaluation
	// exclusion (empty instrument id), not one per position — the shock never
	// got as far as the book.
	SkipNoClassifier = "no_classifier"
	// SkipUnclassified: a classifier is wired and does not know this instrument,
	// so whether the shock applies to it is unknown. DISTINCT FROM "the position
	// is in a different sector", which is a real answer and is not recorded.
	SkipUnclassified = "unclassified_instrument"
	// SkipShockNamesNoSector: the SectorShock carries neither taxonomy nor code,
	// so there is nothing to match against. A malformed shock, and the only
	// reason here that is the caller's rather than the estate's.
	SkipShockNamesNoSector = "sector_shock_names_no_sector"
	// SkipNoRevaluer: a VolShock was requested and no Revaluer is wired, so the
	// evaluation runs on the linear path where a vol bump has NO EXPRESSION —
	// not a small effect, none. A whole-evaluation exclusion like
	// SkipNoClassifier, and a composition-root gap for the same reason: it is a
	// property of the deployment, not of any holding.
	//
	// It is a DIFFERENT operator action from SkipNoClassifier, which is why it is
	// its own reason. No classifier is a reference-data source nobody wired; no
	// revaluer is blocked further out, on observed option premiums this estate
	// does not carry at all (#509/#203/#345). Loading instrument reference data
	// will not fix it.
	SkipNoRevaluer = "no_revaluer"
	// SkipUnknownShockType: the Go API accepted a ScenarioShock implementation
	// this engine does not dispatch. Serving the unchanged book would claim the
	// stress ran, so the whole evaluation is refused.
	SkipUnknownShockType = "unknown_shock_type"
)

// Evaluate applies shocks to a deep clone of p and returns the
// MeasureSet computed against the shocked state, plus the coverage record for
// the SHOCK APPLICATION itself. p is not mutated. The registry parameter
// selects which measures to compute; nil falls back to
// compute.DefaultRegistry() for parity with compute.ComputeMeasures.
//
// # The second return value is the whole point, and a zero one is a claim
//
// A shock that cannot be applied used to return silently, so Evaluate handed
// back measures computed over the UNSHOCKED book and nothing said so. Every
// named scenario in the library — GFC_2008, COVID_2020 and the curve, factor,
// liquidity and climate stresses — is built from SectorCurve and emits
// SectorShocks exclusively, so with no classifier wired a 2008 replay reported
// "no impact" on a real book. That is byte-identical to the answer for a
// portfolio with no exposure, and it reached a desk through the live
// POST /v1/portfolios/{id}/scenario route (#640).
//
// The coverage is v1.InputCoverage rather than a new type because it is the same
// question that type already answers for a measure: what did this computation
// fail to see, how much of it, and which holdings. Contributed counts the
// positions whose sector WAS resolved, so Contributed=0 with a non-zero
// ExcludedCount is the total-blindness case.
//
// CALLERS MUST NOT IGNORE IT. Engine.EvaluateScenario refuses the request when
// ExcludedCount is non-zero — see v1.ErrScenarioUnresolvable for why a refusal
// and not a quality flag.
func Evaluate(p *domain.Portfolio, shocks []v1.ScenarioShock, registry *compute.Registry, opts ...Option) (*domain.MeasureSet, v1.InputCoverage) {
	var cfg evalConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	cov := newShockCoverage()
	for _, shock := range shocks {
		if !knownShockKind(shock) {
			cov.unknownShockType()
		}
	}
	// ONE ENTRY POINT, TWO CLONE STRATEGIES. With a Revaluer wired the shocks are
	// applied by repricing (cloneWithReval), which is the only way a VolShock
	// moves a number; without one they are applied linearly, and applyShock
	// records the vol shocks that therefore reach nothing. EvaluateReval funnels
	// here rather than duplicating this, so there is one place the coverage is
	// assembled and one place a new shock class has to be considered.
	var shocked *domain.Portfolio
	if cfg.revaluer == nil {
		shocked = cloneWithShocks(p, shocks, cfg, cov)
	} else {
		shocked = cloneWithReval(p, shocks, cfg.revaluer, cfg, cov)
	}
	return compute.ComputeMeasures(shocked, registry, nil), cov.result()
}

// shockCoverage is compute.Coverage plus the per-EVALUATION dedup a shock batch
// needs. It is not a second coverage record — it is the one record, filled once
// per position rather than once per (position, shock).
//
// A NAMED SCENARIO IS ELEVEN SHOCKS, NOT ONE. library.SectorCurve emits a
// SectorShock per GICS sector, and the apply loop visits every position for each
// of them. Counting straight onto the accumulator would report the same
// unclassified holding eleven times, blow the bounded sample on eleven copies of
// one instrument, and make the ratio in the refusal message ("N of M resolved")
// a multiple of the book rather than the book. The counts have to mean
// positions, because that is what an operator goes and fixes.
type shockCoverage struct {
	cov          compute.Coverage
	noClassifier bool
	noRevaluer   bool
	unknownShock bool
	// seen holds the positions already accounted for, contributed or excluded.
	// One decision per position per evaluation: the classification does not
	// change between shocks in the same batch, so the second look would be the
	// same answer counted twice.
	seen map[domain.InstrumentID]bool
}

// knownShockKind is the allocation-free admission check for the v1 shock
// vocabulary. Keep it beside applyShock: one admits a kind and the other applies
// it. The architecture guard derives the same vocabulary from api/v1.
func knownShockKind(shock v1.ScenarioShock) bool {
	switch shock.(type) {
	case v1.PriceShock, v1.ParallelShift, v1.SectorShock, v1.VolShock:
		return true
	default:
		return false
	}
}

func newShockCoverage() *shockCoverage {
	return &shockCoverage{seen: map[domain.InstrumentID]bool{}}
}

// noClassifierWired records the whole-evaluation exclusion, at most once. NOT
// one per position: the shock never got as far as the book, and inflating the
// count to the position count would make a deployment-wide gap look like a
// per-instrument reference-data hole — the two an operator must not confuse.
func (s *shockCoverage) noClassifierWired() {
	if s.noClassifier {
		return
	}
	s.noClassifier = true
	s.cov.ExcludeWhole(SkipNoClassifier)
}

// noRevaluerWired records the whole-evaluation exclusion for a VolShock reaching
// the linear path, at most once per evaluation.
//
// AT MOST ONCE, AND THAT IS NOT COSMETIC. A vol-surface stress is one VolShock
// per underlying — a book with forty option underlyings sends forty — and
// counting each would put "0 of 40 resolved" in the refusal, which reads as a
// fact about the book. It is one fact about the deployment: no revaluer.
func (s *shockCoverage) noRevaluerWired() {
	if s.noRevaluer {
		return
	}
	s.noRevaluer = true
	s.cov.ExcludeWhole(SkipNoRevaluer)
}

// unknownShockType records one deployment/API incompatibility for the whole
// batch. More unknown values do not make the same evaluation more incomplete.
func (s *shockCoverage) unknownShockType() {
	if s.unknownShock {
		return
	}
	s.unknownShock = true
	s.cov.ExcludeWhole(SkipUnknownShockType)
}

// malformedShock records a shock that named no sector. Per SHOCK and not
// deduped, because each malformed entry in a curve is its own defect in the
// caller's request.
func (s *shockCoverage) malformedShock() { s.cov.ExcludeWhole(SkipShockNamesNoSector) }

// unclassified records a holding whose sector the wired classifier does not
// know, so whether any sector shock applies to it is unknown.
func (s *shockCoverage) unclassified(id domain.InstrumentID) {
	if s.seen[id] {
		return
	}
	s.seen[id] = true
	s.cov.Exclude(id, SkipUnclassified)
}

// resolved records a holding whose sector WAS resolved — it either took a shock
// or is genuinely in an unshocked sector, and both are real answers.
func (s *shockCoverage) resolved(id domain.InstrumentID) {
	if s.seen[id] {
		return
	}
	s.seen[id] = true
	s.cov.Contributed++
}

func (s *shockCoverage) result() v1.InputCoverage { return s.cov.Result() }

// cloneWithShocks deep-copies p, applies every known shock, returns
// the working copy. Position is a plain value struct so the map
// copy is a true deep copy at the field level.
func cloneWithShocks(p *domain.Portfolio, shocks []v1.ScenarioShock, cfg evalConfig, cov *shockCoverage) *domain.Portfolio {
	cp := domain.NewPortfolio(p.ID(), p.BaseCurrency())
	cp.SetAggregate(domain.AggregateUpdate{
		AsOf:             p.AsOf(),
		DisplayName:      p.DisplayName(),
		BaseCurrency:     p.BaseCurrency(),
		CashBalance:      p.CashBalance(),
		TotalMarketValue: p.TotalMarketValue(),
		PositionCount:    p.PositionCount(),
	})
	for _, pos := range p.Positions() {
		cp.SetPosition(pos)
	}
	for _, shock := range shocks {
		applyShock(cp, shock, cfg, cov)
	}
	return cp
}

// applyShock dispatches one shock to its handler ON THE LINEAR PATH. Evaluate
// routes to cloneWithReval when a Revaluer is wired, so reaching here means
// MarketValue arithmetic is the only tool available. Unknown shock types were
// rejected by Evaluate's preflight before this dispatch runs.
//
// EVERY ARM MUST MOVE THE BOOK OR RECORD ON cov, and that is checked rather than
// remembered: test/arch/every_shock_kind_answers_or_refuses_test.go derives the
// shock set from the api/v1 AST and fails an arm that does neither. An arm that
// does neither is a shock the caller can ask for and the response cannot
// distinguish from "applied, and this book is neutral to it".
func applyShock(p *domain.Portfolio, shock v1.ScenarioShock, cfg evalConfig, cov *shockCoverage) {
	switch s := shock.(type) {
	case v1.PriceShock:
		applyPriceShock(p, s)
	case v1.ParallelShift:
		applyParallelShift(p, s)
	case v1.SectorShock:
		applySectorShock(p, s, cfg.classifier, cov)
	case v1.VolShock:
		applyVolShock(cov)
	}
}

// applyVolShock records that the vol bump reached nothing. It takes no
// portfolio because there is nothing it could do to one: a vol shock moves an
// option's price through vega, and a MarketValue has no vega.
//
// # Why a refusal and not a declaration on the response
//
// The alternative considered was to answer and label the projection — the
// QualityFlagInputsUnresolved shape, or a MeasureProvenance saying "linear, vol
// leg omitted" (#1037). It is the wrong call HERE for the reason
// v1.ErrScenarioUnresolvable already gives for #640: a measure set with one
// partial family still contains real answers worth returning with the bad part
// labelled, while a scenario answers ONE question, and if a shock did not land
// then every number in the response is the book as it already is. There is no
// good part to keep. A +15 vol-point stress reporting VaR99 unchanged is not a
// partial answer, it is a different scenario's answer, and it is plausible —
// which is what makes a label something a desk reads past.
//
// It is also not merely the vol leg that is lost. library.VolSpikeRiskOff pairs
// a −20% spot drop WITH the vol spike because an option book's convexity and its
// vega only bite together; served linearly it degrades to the spot leg alone and
// tells a short-vol desk its worst regime costs it the delta.
//
// This arm fires unconditionally rather than testing cfg.revaluer, because
// Evaluate has already made that decision: applyShock is not reached when a
// Revaluer is wired. A condition here would be a second copy of that routing and
// would be dead in one of its two states.
func applyVolShock(cov *shockCoverage) { cov.noRevaluerWired() }

// applyPriceShock changes one instrument's MarketValue. A shock
// targeting an instrument the portfolio does not hold is a no-op —
// scenario callers commonly fire watchlist-wide batches against
// multiple portfolios.
func applyPriceShock(p *domain.Portfolio, s v1.PriceShock) {
	pos, ok := p.Position(s.InstrumentID)
	if !ok {
		return
	}
	pos.MarketValue = compute.ShockMoney(pos.MarketValue, s.Pct)
	p.SetPosition(pos)
}

// applyParallelShift shocks every position's MarketValue by the
// same percentage. The textbook stress test.
func applyParallelShift(p *domain.Portfolio, s v1.ParallelShift) {
	for _, pos := range p.Positions() {
		pos.MarketValue = compute.ShockMoney(pos.MarketValue, s.Pct)
		p.SetPosition(pos)
	}
}

// applySectorShock shocks every position whose instrument classifies into
// s.Sector, resolved point-in-time as of the portfolio's state time. ctx is
// Background: the dispatch is a pure, synchronous step and the classifier's
// point-in-time semantics ride on asOf, not the context.
//
// # "In a different sector" and "sector unknown" are different answers
//
// This doc used to say a nil classifier "makes the shock a no-op — same
// degradation as an unknown shock type", and that instruments the classifier
// does not know "are skipped (they don't belong to the shocked sector)". The
// second sentence is the defect stated as if it were a design: NOT KNOWING an
// instrument's sector is not evidence that it is outside the shocked one. Both
// cases left the position at its unshocked value and said nothing, so a stress
// test on a book nothing could classify returned the book (#640).
//
// A position the classifier resolves to a DIFFERENT sector is genuinely
// unaffected and is not recorded — that is a real answer, the same way a
// PriceShock against an instrument the portfolio does not hold is a real no-op.
// The two unresolved cases are recorded, and the caller refuses on them.
func applySectorShock(p *domain.Portfolio, s v1.SectorShock, c factor.Classifier, cov *shockCoverage) {
	want := factor.Sector{Taxonomy: s.Taxonomy, Code: s.Code}
	if want.IsZero() {
		cov.malformedShock()
		return
	}
	if c == nil {
		cov.noClassifierWired()
		return
	}
	asOf := p.AsOf()
	for _, pos := range p.Positions() {
		cl, ok := c.Classify(context.Background(), string(pos.InstrumentID), asOf)
		if !ok {
			cov.unclassified(pos.InstrumentID)
			continue
		}
		cov.resolved(pos.InstrumentID)
		if cl.Sector != want {
			continue
		}
		pos.MarketValue = compute.ShockMoney(pos.MarketValue, s.Pct)
		p.SetPosition(pos)
	}
}
