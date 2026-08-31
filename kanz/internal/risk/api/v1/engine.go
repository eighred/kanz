// Package v1 is the narrow, versioned external surface of the risk
// module. Module siblings (the orchestrator, the HTTP API layer,
// scenario tooling, the degraded-mode probe) call into the risk engine
// exclusively through this interface — never through the internal
// implementation packages (`compute/`, `state/`, `ingest/`, etc.).
// RISK-02 wires the architecture test that enforces this boundary at
// build time.
//
// # What this surface intentionally omits
//
// ALL STATE CHANGES ARE EVENT-DRIVEN, so this interface is **query + scenario +
// health** only.
// There is no Apply, no Mutate, no Command method. State arrives via
// the bus (RISK-04 ingests domain/state FACTs); risk-output events
// leave via the bus (RISK-10 publishes them). Callers that need
// notifications on risk-output updates subscribe to the bus directly,
// not through this interface.
//
// The design document that first recorded the rule was deleted on 2026-07-29, so
// the rule is not left resting on it: test/arch/risk_engine_is_query_only_test.go
// fails if a
// mutating method appears on this interface, which is the shape the rule reduces
// to at this boundary.
//
// # Versioning
//
// This package is `kanz/internal/risk/api/v1`. The Engine interface
// and every type in this file is **additive-only within v1** under the
// same logic as kanz-schemas/README.md § Envelope Policy §1: a method
// removed or retyped here breaks every caller, and breaking changes
// are rare and require a sibling `v2/` package + dual-path migration.
// A new method may be added to Engine when every plausible
// implementation can supply a non-degraded answer for it (else it
// belongs on a separate interface).
//
// # Implementations
//
// The production implementation lives in `kanz/internal/risk` (built
// by RISK-04..11). Tests provide their own implementations against
// this interface — the small surface keeps fake construction cheap.
package v1

import (
	"context"
	"errors"
	"fmt"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

// Engine is the risk module's external surface. Implementations are
// expected to be safe for concurrent use — the production engine
// serves many concurrent queries while RISK-04's ingest goroutine
// applies state in parallel.
type Engine interface {
	// Exposure returns the latest computed exposure aggregate for a
	// portfolio. Returns ErrPortfolioNotFound if the portfolio is
	// unknown to the engine.
	Exposure(ctx context.Context, req ExposureRequest) (ExposureResponse, error)

	// Measures returns the latest computed risk measures (VaR,
	// sensitivities, ...) for a portfolio. The MeasuresRequest.Measures
	// filter narrows the response; empty filter ⇒ all measures.
	Measures(ctx context.Context, req MeasuresRequest) (MeasuresResponse, error)

	// EvaluateScenario applies the request's shocks to a transient
	// copy of the current portfolio state and returns the projected
	// measures. State is never mutated — scenarios are pure functions
	// of (state, shocks).
	EvaluateScenario(ctx context.Context, req ScenarioRequest) (ScenarioResponse, error)

	// Health reports the engine's current mode and staleness. Callers
	// that cannot tolerate degraded data branch on Mode before reading
	// the other query methods; the same Mode is also surfaced on each
	// query response via QualityFlags so callers cannot accidentally
	// consume degraded results without seeing the signal.
	Health(ctx context.Context) (Health, error)

	// ListPortfolios names the portfolios this engine holds state for.
	//
	// IT IS NOT A DIRECTORY OF THE FUND'S PORTFOLIOS. It lists what the
	// engine has folded, which is the only set the other four methods
	// can answer about — a portfolio that exists but has had no event
	// reach this engine is absent, and saying otherwise would offer a
	// caller a choice that then returns ErrPortfolioNotFound.
	ListPortfolios(ctx context.Context) ([]PortfolioSummary, error)
}

// PortfolioSummary is one entry in ListPortfolios: enough to choose a
// portfolio and no more.
//
// NO MONEY ON IT, DELIBERATELY. Cash balance and market value are what
// an Exposure read returns; carrying them here would mean every caller
// who wanted a picker also received the fund's valuations. AsOf IS
// carried, because staleness is the one property that distinguishes two
// otherwise identical rows and it is invisible unless the list shows it.
type PortfolioSummary struct {
	ID            PortfolioID
	DisplayName   string
	BaseCurrency  string
	AsOf          time.Time
	PositionCount uint32
}

// PortfolioID identifies a portfolio aggregate. Stable across the
// portfolio's lifetime; the same identifier RISK-05's per-aggregate
// ordering keys on.
type PortfolioID string

// InstrumentID identifies a tradeable instrument. Stable for the
// instrument's lifetime. Lifted into api/v1 (rather than left in
// domain) so external callers constructing scenario shocks like
// PriceShock can reference an instrument without importing the
// risk-internal domain package — RISK-02's arch test forbids
// outsiders from importing kanz/internal/risk/{anything-not-api}.
type InstrumentID string

// MeasureName names one risk measure (e.g. "VaR99", "Delta", "Gamma").
// The catalog of valid names is owned by RISK-07.
type MeasureName string

// --- Exposure ----------------------------------------------------------

// ExposureRequest scopes an exposure query.
type ExposureRequest struct {
	PortfolioID PortfolioID
	// AsOf is RESERVED AND NOT HONOURED. Zero ⇒ latest, which is the only
	// query this engine can answer; any other value is REFUSED with
	// ErrAsOfNotSupported rather than silently answered from live state
	// (#859). Retiring that refusal needs a point-in-time read of risk
	// state, which internal/risk/state does not have.
	//
	// The field is kept rather than deleted because the wire contract
	// (query.v1.ExposureRequest.as_of) is additive-only within v1 and the
	// gateway already accepts the parameter; removing it here would move the
	// refusal to a shape mismatch nobody can read.
	AsOf time.Time
}

// ExposureResponse is the engine's reply to ExposureRequest.
type ExposureResponse struct {
	PortfolioID PortfolioID
	// AsOf is the state timestamp the response was computed against. It is
	// always the latest applied state, because ExposureRequest.AsOf is
	// refused rather than honoured (#859) — so this reports what the answer
	// IS, never what the caller asked for.
	AsOf time.Time
	// Set is the exposure aggregate. The concrete implementation lives
	// in `kanz/internal/risk/domain` (RISK-03); api/v1 keeps the
	// surface narrow with a minimal interface so domain types can
	// evolve without churning callers.
	Set ExposureSet
	// QualityFlags annotate freshness and trust (see QualityFlag).
	QualityFlags []QualityFlag
	// SourcePosition is the durable-log coordinate the served state was
	// folded to — the citation seed a governed reader stamps onto an
	// answer so it is verifiable against the log (query.v1 source_position,
	// WIRE-03). It is the LogPosition of the last durable PortfolioSnapshot
	// applied for this portfolio (RISK-05); nil when no snapshot with a
	// position has been applied, or when the response was served from the
	// degraded cache rather than live state (a cached value has no
	// log anchor). Incremental live applies off the NATS spine after that
	// snapshot are not individually log-anchored — the durable Kafka offset
	// is not on the live envelope — so this is the honest last-anchored
	// coordinate, not necessarily the latest applied event.
	SourcePosition *commonpb.LogPosition
}

// ExposureSet is the engine's exposure-aggregate response. RISK-03
// provides the concrete implementation; api/v1 surfaces only the
// freshness signal callers need to decide whether to act.
type ExposureSet interface {
	// AsOf is the state timestamp the exposure was computed against.
	AsOf() time.Time
}

// --- Measures ----------------------------------------------------------

// MeasuresRequest scopes a risk-measure query.
type MeasuresRequest struct {
	PortfolioID PortfolioID
	// AsOf is RESERVED AND NOT HONOURED — see ExposureRequest.AsOf. Zero ⇒
	// latest; any other value is REFUSED with ErrAsOfNotSupported (#859).
	AsOf time.Time
	// Measures narrows the response to a named subset (e.g.
	// {"VaR99", "Delta"}); empty ⇒ all measures the engine computes.
	Measures []MeasureName
}

// MeasuresResponse is the engine's reply to MeasuresRequest.
type MeasuresResponse struct {
	PortfolioID PortfolioID
	// AsOf is the state timestamp the response was computed against, which is
	// always the latest applied state — MeasuresRequest.AsOf is refused
	// rather than honoured (#859).
	AsOf         time.Time
	Set          MeasureSet
	QualityFlags []QualityFlag
	// SourcePosition is the durable-log coordinate the served state was
	// folded to — see ExposureResponse.SourcePosition for the full contract
	// (WIRE-03). nil on a degraded cache read or when no positioned snapshot
	// has been applied.
	SourcePosition *commonpb.LogPosition
}

// MeasureSet exposes the named measure values for a query.
// RISK-03/07 provide the concrete type; api/v1 keeps the surface
// narrow with a single look-up method.
type MeasureSet interface {
	AsOf() time.Time
	// Lookup returns the named measure and true, or zero Measure and
	// false when the measure is not in the set.
	Lookup(MeasureName) (Measure, bool)
	// CurrencyExclusions lists the positions that contributed to NO
	// measure in this set because their currency is not the portfolio's
	// base (see QualityFlagCurrencyExcluded). Empty ⇒ the whole book was
	// measured.
	//
	// This is on the interface rather than only on the response so the
	// coverage travels WITH the values. A MeasureSet served from the
	// degraded cache, narrowed by a measure filter, or projected through
	// a scenario carries its own exclusions, so no path can hand a
	// caller a partial number stripped of the fact that it is partial.
	CurrencyExclusions() []CurrencyExclusion
}

// Measure is one named risk-measure value plus its propagated
// uncertainty (RISK-08).
type Measure struct {
	Name MeasureName
	// Value is the canonical exact-decimal value. common.v1.Decimal is
	// the system-wide money/size/price representation
	// (kanz-schemas/proto/common/v1/decimal.proto) — using it here
	// keeps the api surface coherent with the risk-output events
	// RISK-10 publishes.
	Value *commonpb.Decimal
	// UncertaintyAbs is the absolute one-sigma uncertainty band around
	// Value (RISK-08). nil ⇒ no uncertainty was propagated.
	UncertaintyAbs *commonpb.Decimal
	// Coverage says how much of the book Value was actually derived
	// from. See InputCoverage — its zero value means "this measure does
	// not report input coverage", NOT "everything resolved".
	Coverage InputCoverage
}

// InputCoverage records how much of the book one measure was actually
// computed over — the difference between a MEASURED zero and a
// CONFIDENT one (#527).
//
// # The failure it exists to break
//
// A measure that resolves per-position inputs through a provider —
// bond terms, a discount curve, a factor model, a return series —
// skips whatever does not resolve and then returns its accumulator
// regardless. When nothing resolved, the accumulator is zero, and that
// zero is served with a 200 as an ACTIVE CLAIM: DV01 = 0 says the book
// carries no interest-rate risk. It is byte-identical to the answer for
// a book that genuinely holds no bonds. The contract-terms store has no
// production writer at all, so on this estate that is not a corner case
// — it is the answer every FI query currently gets.
//
// # The direction of the error is NOT one-way, and assuming it is has
// # already produced a false comment
//
// QualityFlagCurrencyExcluded's doc below was written for the four
// sum-over-base-currency measures and says the error is "SMALLER, never
// larger". That holds for a SUM. It does not hold for the other
// aggregation forms now in the registry:
//
//   - RATIO / WEIGHTED AVERAGE (Duration, Convexity, SpreadDuration,
//     StructDuration, HHI, LiquidationHorizon) — dropping a
//     below-average element RAISES the result. Three bonds with
//     durations 8/2/5 average 5.0; drop the 2 and the same measure
//     reports 6.5. concentration.go has said this about HHI since #257.
//   - QUANTILE OF A PORTFOLIO P&L PATH (the historical VaR99/ES99/
//     MaxDrawdown family, FactorVaR99, SystematicRisk) — dropping one
//     leg of a hedged pair removes the OFFSET, so the measured risk
//     goes UP.
//
// So Coverage carries no direction claim. It says what was left out and
// how much; what that does to the number is a property of the measure's
// aggregation form, and any caller gating on a money measure must treat
// a non-zero ExcludedCount as a REFUSAL TO ANSWER rather than as an
// annotation on a good number.
//
// # Why this is on the Measure and not on the MeasureSet
//
// CurrencyExclusions is a property of the PORTFOLIO — one filter, every
// measure — so it rides on the set. This is per-MEASURE by nature: a
// bond with no discount curve is missing from DV01 and present in
// GrossExposure, and a single set-level list would say neither which
// measure lost it nor that the others did not. Riding on the value is
// also strictly stronger than riding beside it: a Lookup cannot hand
// back the number without the coverage, on the live path, through a
// filter, or off the degraded cache.
type InputCoverage struct {
	// Contributed is how many positions reached the measure's
	// arithmetic. Zero WITH a non-zero ExcludedCount is the #527 case:
	// the measure was computed over nothing at all.
	Contributed int
	// ExcludedCount is how many positions could not be assessed. It is
	// the TOTAL, not len(Exclusions) — see Exclusions. Zero ⇒ either the
	// whole applicable book was covered, or this measure does not report
	// coverage at all; those are told apart by Contributed and by the
	// measure's own documentation, not by this field.
	ExcludedCount int
	// Exclusions is a BOUNDED SAMPLE of the excluded positions, capped
	// at MaxInputExclusions. Unlike a currency exclusion — a handful of
	// holdings on a mostly-single-currency book — an unresolved-input
	// exclusion is the DEFAULT state of a book whose reference data was
	// never loaded, which is every position on every query. Enumerating
	// it in full would put the whole book in every response and in the
	// degraded cache; ExcludedCount carries the magnitude and this
	// carries the identity.
	Exclusions []InputExclusion
}

// MaxInputExclusions caps InputCoverage.Exclusions. One constant for
// every measure family so a response's size cannot depend on which
// family is degraded, and so the sample a reader sees means the same
// thing everywhere.
const MaxInputExclusions = 32

// InputExclusion names one position left out of one measure because an
// input that measure needed did not resolve. The evidence behind
// QualityFlagInputsUnresolved, in the role CurrencyExclusion plays for
// QualityFlagCurrencyExcluded.
type InputExclusion struct {
	// InstrumentID is the position that was left out. EMPTY means the
	// exclusion is a property of the whole evaluation rather than of any
	// holding — no factor model resolved for this snapshot, no
	// counterparty exposures, too little return history to form a
	// distribution. A reader labelling by instrument must expect the
	// empty value and must not drop it: the empty one is the whole-book
	// event and the most severe.
	InstrumentID InstrumentID
	// Reason is drawn from the measure family's own closed vocabulary
	// (compute.SkipNoTerms, SkipNoCurve, SkipNoModel, ...) rather than
	// being free text, because it is also a metric label and a label
	// must not be whatever a future edit writes.
	Reason string
}

// --- Scenario ----------------------------------------------------------

// ScenarioRequest evaluates a what-if scenario against the current
// portfolio state without mutating it.
type ScenarioRequest struct {
	PortfolioID PortfolioID
	// Shocks describe the hypothetical perturbations. RISK-09 owns the
	// concrete shock taxonomy; api/v1 leaves it as an opaque interface
	// so the domain can evolve without churning this surface.
	Shocks []ScenarioShock
}

// ScenarioShock is one perturbation in a scenario. RISK-09 provides
// concrete shock types (parallel-shift, factor-shock, etc.).
type ScenarioShock interface {
	// Description is a human-readable label for the shock, suitable
	// for inclusion in audit logs and scenario reports.
	Description() string
}

// ScenarioResponse carries the projected measures under the requested
// shocks.
type ScenarioResponse struct {
	PortfolioID  PortfolioID
	Projected    MeasureSet
	QualityFlags []QualityFlag
}

// --- Health ------------------------------------------------------------

// Health reports the engine's operational state.
type Health struct {
	Mode Mode
	// AsOf is the latest applied state-event time across all aggregates.
	AsOf time.Time
	// Staleness is the gap between AsOf and the engine's wall clock at
	// the moment Health was sampled. Callers compare it against their
	// own freshness budget.
	Staleness time.Duration
}

// Mode names the engine's operational mode.
type Mode string

const (
	// ModeNormal: state ingestion is current and risk measures are
	// computed from the latest applied state.
	ModeNormal Mode = "NORMAL"
	// ModeDegraded: the engine is serving cached / last-known values
	// because state ingestion stalled, computation failed, or a
	// dependency is unavailable. Responses still come back; callers
	// must check QualityFlags / Health.Mode and decide whether to act.
	ModeDegraded Mode = "DEGRADED"
)

// QualityFlag annotates a response with trust signals. Mirrors the
// envelope QualityFlag concept (kanz-schemas/proto/envelope/v1/envelope.proto)
// without depending on the proto type, so api/v1 stays free of
// envelope-versioning churn for purely Go-API concerns.
type QualityFlag string

const (
	// QualityFlagDegraded: the engine was in ModeDegraded when this
	// response was computed; the values are cached / last-known.
	QualityFlagDegraded QualityFlag = "DEGRADED"
	// QualityFlagStale: the response data is older than the caller's
	// freshness budget (request AsOf vs response AsOf gap), but the
	// engine itself is not degraded.
	QualityFlagStale QualityFlag = "STALE"
	// QualityFlagCurrencyExcluded: the measures were computed over a
	// SUBSET of the portfolio. The engine has no FX layer (RISK-06), so
	// positions whose MarketValue is not denominated in the portfolio's
	// BaseCurrency contribute to no base-currency measure. The excluded
	// positions are enumerated by MeasureSet.CurrencyExclusions.
	//
	// A limit check against a flagged response can pass when the whole
	// book would breach. Any caller that gates on a money measure
	// (pre-trade check, concentration limit, margin call) must treat this
	// flag as a refusal to answer, not as an annotation on a good number.
	//
	// THIS DOC USED TO SAY THE ERROR DIRECTION WAS ONE-WAY — "SMALLER,
	// never larger". That was written in #257 for the four
	// sumInBaseCurrency measures and it was never true of the set as a
	// whole; it is false for 11 of the measures now registered, and it is
	// corrected here rather than left as dated evidence a sizing decision
	// could rest on:
	//
	//   - The flag is attached to the WHOLE set (compute.ComputeMeasures),
	//     including the FI, Greek, structured and XVA measures, which
	//     never consult InBaseCurrency at all. For those, a flagged
	//     response is not "smaller because positions were dropped" — it is
	//     a currency-MIXED number, and the flag over-claims.
	//   - A ratio drops in either direction. HHI is the case
	//     concentration.go has documented since #257: "dropping positions
	//     raises it towards 1 as often as it lowers it".
	//   - A quantile of a portfolio P&L path is not monotone in its legs.
	//     Excluding one leg of a hedged pair removes the OFFSET, so VaR99
	//     and ES99 come out LARGER.
	//
	// See InputCoverage for the same argument stated once for all measure
	// families. What survives is the operational instruction above: do not
	// gate on a flagged number. What does not survive is the claim that
	// you know which way it is wrong.
	QualityFlagCurrencyExcluded QualityFlag = "CURRENCY_EXCLUDED"

	// QualityFlagInputsUnresolved: at least one measure in this response
	// was computed over positions whose pricing inputs did not resolve —
	// possibly over NONE of them. The evidence is on each affected
	// measure, as Measure.Coverage (InputCoverage), naming the positions
	// and the reason each was left out.
	//
	// DISTINCT FROM CurrencyExcluded ON PURPOSE, not a second spelling of
	// it. That flag is about a filter the engine applies deliberately
	// because it has no FX layer; this one is about DATA THAT IS NOT
	// THERE — contract terms nothing writes, a discount curve calibration
	// never ran for, a factor universe an instrument is outside of, a
	// return series the price store does not hold. Merging them would put
	// "we chose not to include this" and "we could not find out" behind
	// one signal, and only the second is a defect somebody must fix.
	//
	// THE MEASURE IS STILL PRESENT, and the value it carries is not a
	// claim. A DV01 of zero on a flagged response means the engine priced
	// no bonds, which is not the same statement as "this book holds no
	// bonds" — that second statement is one the engine cannot make, and
	// this flag is what stops it being read out of a number that looks
	// identical. The direction of the error is NOT knowable from the
	// flag: see InputCoverage. Any caller that gates must refuse.
	QualityFlagInputsUnresolved QualityFlag = "INPUTS_UNRESOLVED"
)

// QualityFlags is every flag the engine can attach to a response. It
// exists so translation layers (the query.v1 gRPC mapping in
// services/risk-engine/internal/grpcsrv) can be proven exhaustive by a
// test rather than silently dropping a flag they were never taught —
// which would restore exactly the silence QualityFlagCurrencyExcluded
// was added to break. Append here when adding a flag above.
//
// FORGETTING TO APPEND IS THE SILENT FAILURE, AND IT DISARMS THE OTHERS.
// A flag left out of this slice is not merely unlisted: every guard that
// sweeps it — TestProtoFlags_EveryAPIFlagMaps most of all — then sweeps a
// set that does not contain the new flag and passes, so the wire mapping
// is never checked and the flag is dropped in translation with nothing
// red. test/arch/quality_flag_completeness_test.go closes that by reading
// the const block out of the source and comparing it against this
// literal; it is not a style rule, it is what keeps the exhaustiveness
// test honest.
var QualityFlags = []QualityFlag{
	QualityFlagDegraded,
	QualityFlagStale,
	QualityFlagCurrencyExcluded,
	QualityFlagInputsUnresolved,
}

// CurrencyExclusion names one position left out of every base-currency
// measure because its MarketValue is denominated in something other than
// the portfolio's BaseCurrency (or carries no MarketValue at all). It is
// the evidence behind QualityFlagCurrencyExcluded: a caller can see
// exactly which holdings the number does not include.
type CurrencyExclusion struct {
	// InstrumentID is the excluded holding.
	InstrumentID InstrumentID
	// Currency is the currency its MarketValue was denominated in, or
	// "" when the position had no MarketValue to read (unmarked — the
	// engine cannot express its exposure until a mark arrives).
	Currency string
}

// --- Sentinel errors ---------------------------------------------------

var (
	// ErrPortfolioNotFound: the requested portfolio is unknown to the
	// engine. The portfolio may exist in the source-of-truth log but
	// no state event has been ingested for it yet.
	ErrPortfolioNotFound = errors.New("risk: portfolio not found")

	// ErrInvalidRequest: a request field violates the api contract
	// (empty PortfolioID, malformed AsOf, etc.). Distinct from
	// transport/runtime errors so callers can branch correctly.
	ErrInvalidRequest = errors.New("risk: invalid request")

	// ErrAsOfNotSupported: the caller pinned a query to a point in time and
	// this engine cannot answer one (#859).
	//
	// # A REFUSAL, BECAUSE THE ALTERNATIVE WAS A LIE
	//
	// ExposureRequest.AsOf and MeasuresRequest.AsOf were declared, documented
	// as working, parsed by the gateway and forwarded through gRPC — and then
	// never read. Both queries answered from store.Snapshot(id), the LIVE
	// portfolio, and stamped the response with the live state's timestamp. So
	// "what were this portfolio's exposures last Tuesday" returned today's
	// book, internally consistent and wrong, with nothing logged, counted or
	// flagged. A reconciliation, a regulatory as-of report or an investigation
	// pinning a date could not tell.
	//
	// A silently ignored parameter is worse than an unsupported one precisely
	// because the caller cannot tell — the platform's "nothing configured" and
	// "checked, and fine" rule, applied to a request field. This makes the gap
	// visible to every caller on the first call instead of to nobody, ever.
	//
	// # It wraps ErrInvalidRequest, deliberately
	//
	// So the existing transport mapping carries it unchanged: the gRPC server
	// already answers ErrInvalidRequest with codes.InvalidArgument, and the
	// gateway already renders that as 400. A second sentinel with its own
	// mapping would be a second thing to keep in step. Callers that want to
	// distinguish "not supported yet" from "malformed" still can, with
	// errors.Is(err, ErrAsOfNotSupported).
	//
	// # What retires it
	//
	// A point-in-time read of risk state. internal/risk/state has none today —
	// Snapshot(id) is live-only — so honouring as_of is a persistence change,
	// not a branch here. It is tracked separately, and whoever does it must
	// revisit internal/risk/pricing/pit.DefaultHorizon in the same change: that
	// horizon was sized on the fact that no caller can pin a historical as-of
	// (#811), and a pricing store that starts empty on every pod would answer
	// an arbitrary historical as-of with no curve — reporting DV01 = 0, which
	// is this same defect one layer down.
	ErrAsOfNotSupported = fmt.Errorf(
		"%w: as_of is accepted by the API and cannot be honoured by this engine — "+
			"it would be answered from live state and stamped with the live timestamp, so the "+
			"answer would look consistent and describe the wrong instant. Omit as_of to query "+
			"the latest state (#859)", ErrInvalidRequest)

	// ErrPortfolioNotOwned: this REPLICA does not own the portfolio, so it
	// has no state for it and will not answer. The portfolio exists; another
	// replica holds it (#110).
	//
	// THIS IS NOT ErrPortfolioNotFound, and collapsing the two would be the
	// worst available answer. On a sharded fleet the query Service fans out
	// across every replica, so the same request reaches a different pod each
	// time — and a bare "not found" from the two thirds that do not own the
	// portfolio reads as "no such portfolio", which is false, non-deterministic
	// between identical calls, and indistinguishable from the genuine case.
	// A caller that sees this error knows to look elsewhere; the message names
	// where. Wrapped errors carry the owning replica's id.
	ErrPortfolioNotOwned = errors.New("risk: portfolio is not owned by this replica")

	// ErrScenarioUnresolvable: a shock in the request could not be applied
	// because an input it needed did not resolve, so the projected measures
	// would describe a book the scenario never reached. Wrapped errors name
	// the reason (scenario.SkipNoClassifier, SkipUnclassified, ...), the
	// count, and a bounded sample of the holdings (#640).
	//
	// # A REFUSAL, AND NOT A QUALITY FLAG, WHICH IS THE OPPOSITE CHOICE FROM
	// # QualityFlagInputsUnresolved — deliberately
	//
	// That flag is right for a MEASURE SET, where a caller asked for a
	// portfolio's risk and one family of numbers in it is partial: the other
	// measures are still answers, so the response is worth returning with the
	// bad part labelled. A SCENARIO is not that shape. The caller asked one
	// question — what does GFC_2008 do to this book — and if the shock did not
	// land, EVERY number in the response is the book as it already is. There is
	// no good part to keep.
	//
	// The failure mode is also worse than a partial number, because the
	// unshocked answer is PLAUSIBLE. A 2008 replay reporting no impact reads as
	// a resilient portfolio rather than as a broken control, and this estate has
	// no instrument classifier at any composition root — so that was the answer
	// every named scenario gave, on a live authorized route, with a 200.
	//
	// It maps to FailedPrecondition at the gRPC edge, not InvalidArgument: with
	// the one exception of a malformed shock naming no sector, the request is
	// well-formed and the deployment is what cannot serve it, so retrying the
	// same call elsewhere is not the remedy.
	ErrScenarioUnresolvable = errors.New("risk: scenario shock could not be applied — an input it needs did not resolve")
)
