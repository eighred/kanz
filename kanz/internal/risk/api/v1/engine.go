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
// same logic as kanz-schemas/docs/envelope-policy.md §1: a method
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
	// AsOf pins the query to a point in time. Zero ⇒ latest.
	// The returned ExposureResponse.AsOf reports the actual state
	// timestamp, which may be older when the engine is degraded.
	AsOf time.Time
}

// ExposureResponse is the engine's reply to ExposureRequest.
type ExposureResponse struct {
	PortfolioID PortfolioID
	// AsOf is the state timestamp the response was computed against —
	// equal to or older than ExposureRequest.AsOf when set.
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
	AsOf        time.Time
	// Measures narrows the response to a named subset (e.g.
	// {"VaR99", "Delta"}); empty ⇒ all measures the engine computes.
	Measures []MeasureName
}

// MeasuresResponse is the engine's reply to MeasuresRequest.
type MeasuresResponse struct {
	PortfolioID  PortfolioID
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
	// The error direction is one-way: dropping positions makes gross
	// exposure, net exposure, VaR and Delta SMALLER, never larger. A
	// limit check against a flagged response can therefore pass when the
	// whole book would breach. Any caller that gates on a money measure
	// (pre-trade check, concentration limit, margin call) must treat this
	// flag as a refusal to answer, not as an annotation on a good number.
	QualityFlagCurrencyExcluded QualityFlag = "CURRENCY_EXCLUDED"
)

// QualityFlags is every flag the engine can attach to a response. It
// exists so translation layers (the query.v1 gRPC mapping in
// services/risk-engine/internal/grpcsrv) can be proven exhaustive by a
// test rather than silently dropping a flag they were never taught —
// which would restore exactly the silence QualityFlagCurrencyExcluded
// was added to break. Append here when adding a flag above.
var QualityFlags = []QualityFlag{
	QualityFlagDegraded,
	QualityFlagStale,
	QualityFlagCurrencyExcluded,
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
)
