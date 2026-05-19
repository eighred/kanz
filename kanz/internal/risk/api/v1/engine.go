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
// Per the KANZ_BRAIN.md anti-decision "all state changes must be
// event-driven", this interface is **query + scenario + health** only.
// There is no Apply, no Mutate, no Command method. State arrives via
// the bus (RISK-04 ingests domain/state FACTs); risk-output events
// leave via the bus (RISK-10 publishes them). Callers that need
// notifications on risk-output updates subscribe to the bus directly,
// not through this interface.
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

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
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
}

// MeasureSet exposes the named measure values for a query.
// RISK-03/07 provide the concrete type; api/v1 keeps the surface
// narrow with a single look-up method.
type MeasureSet interface {
	AsOf() time.Time
	// Lookup returns the named measure and true, or zero Measure and
	// false when the measure is not in the set.
	Lookup(MeasureName) (Measure, bool)
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
)

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
