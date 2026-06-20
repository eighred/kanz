// Package domain holds the in-memory aggregate types the risk engine
// works against — Portfolio, Position, Exposure, ExposureSet,
// MeasureSet. These are the **concrete shapes behind the api/v1
// opaque interfaces** (RISK-01): the engine's compute layer
// (RISK-06/07/08) builds them, the api/v1 surface returns them, and
// callers inspect them through the narrow interface methods.
//
// # Why these types are not the kanz-schemas proto types
//
// kanz-schemas/proto/domain/v1 (PortfolioState, PositionState, ...)
// is the **wire shape** for state events on the bus — designed for
// serialization, evolution, and cross-language consumption. These
// domain types are the **engine's working in-memory shape** —
// designed for fast access, immutability of snapshots, and natural Go
// ergonomics. Two-layer separation: RISK-04 ingests the proto and
// constructs domain values; RISK-10 serializes domain values back to
// proto for publishing. The cost of the duplication is the win of
// being able to evolve either side without churning the other.
//
// # Mutation discipline
//
// Domain types are treated as **immutable from the api/v1 caller's
// perspective**: every method returns a value or a fresh slice / map,
// never the package's internal storage. RISK-05's state-apply layer
// is the only writer; concurrent readers + a single writer is the
// model RISK-04 will use (one ingest goroutine, many query
// goroutines). The accessor methods here are deliberately read-only
// so that contract stays clear.
//
// # Boundary
//
// This package lives under `kanz/internal/risk/` and is therefore
// private to the risk module per RISK-02's arch test. External
// callers see domain values only via the api/v1 interfaces
// (v1.ExposureSet, v1.MeasureSet), never via type assertion to the
// concrete domain.* types.
package domain

import (
	"sort"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
)

// InstrumentID identifies a tradeable instrument. Type-aliased to
// v1.InstrumentID so the api/v1 surface (which carries the type for
// external scenario-shock callers per RISK-09) and the internal
// domain types share one canonical identifier with zero
// conversion. Same key kanz-schemas/proto/market/v1 uses for
// partition_key on market-data events.
type InstrumentID = v1.InstrumentID

// CurrencyCode is an ISO 4217 alphabetic currency code (e.g. "USD").
// Mirrors the `currency_code` field on common.v1.Money — domain uses
// the typed alias for compile-time safety against bare strings.
type CurrencyCode string

// Portfolio is the in-memory aggregate state for one portfolio. It is
// the consumer of domain.v1.PortfolioState + PositionState events
// (kanz-schemas/proto/domain/v1) — RISK-04 ingests those, RISK-05
// applies them. This struct is the engine's working copy.
type Portfolio struct {
	id               v1.PortfolioID
	displayName      string
	baseCurrency     CurrencyCode
	cashBalance      *commonpb.Money
	totalMarketValue *commonpb.Money
	positionCount    uint32
	positions        map[InstrumentID]Position
	asOf             time.Time
	logPosition      *commonpb.LogPosition
}

// AggregateUpdate carries the portfolio-level fields from a
// domain.v1.PortfolioState event payload — passed to SetAggregate
// without coupling the domain package to the kanz-schemas domain.v1
// proto (it already depends on common.v1 for Money / Decimal /
// LogPosition, but limiting the schemas dep at the package boundary
// keeps the proto→domain mapping in one place: RISK-05).
type AggregateUpdate struct {
	AsOf             time.Time
	DisplayName      string
	BaseCurrency     CurrencyCode
	CashBalance      *commonpb.Money
	TotalMarketValue *commonpb.Money
	PositionCount    uint32
}

// NewPortfolio constructs a portfolio with zero positions. RISK-05's
// state-apply layer is the only caller; tests construct directly.
func NewPortfolio(id v1.PortfolioID, baseCurrency CurrencyCode) *Portfolio {
	return &Portfolio{
		id:           id,
		baseCurrency: baseCurrency,
		positions:    make(map[InstrumentID]Position),
	}
}

// ID returns the portfolio identifier.
func (p *Portfolio) ID() v1.PortfolioID { return p.id }

// DisplayName returns the human-readable portfolio label (advisory).
func (p *Portfolio) DisplayName() string { return p.displayName }

// BaseCurrency returns the portfolio's reporting currency.
func (p *Portfolio) BaseCurrency() CurrencyCode { return p.baseCurrency }

// CashBalance returns uninvested cash, in BaseCurrency. nil before any
// PortfolioState event has been applied.
func (p *Portfolio) CashBalance() *commonpb.Money { return p.cashBalance }

// TotalMarketValue returns the mark-to-market value of positions plus
// cash, in BaseCurrency. nil before any PortfolioState event.
func (p *Portfolio) TotalMarketValue() *commonpb.Money { return p.totalMarketValue }

// PositionCount returns the number of non-flat positions as reported
// by the most recent PortfolioState event.
func (p *Portfolio) PositionCount() uint32 { return p.positionCount }

// AsOf returns the timestamp of the most recent state event applied
// to this portfolio. Zero before any state event has been applied.
func (p *Portfolio) AsOf() time.Time { return p.asOf }

// LogPosition returns the durable-log coordinate the most recent
// snapshot was taken at, or nil before a snapshot has been applied.
// Used by the snapshot-then-resume bootstrap path
// (event-class-rules §3).
func (p *Portfolio) LogPosition() *commonpb.LogPosition { return p.logPosition }

// Clone returns a deep copy of the portfolio's value fields and a fresh
// positions map. Position is a value struct whose pointer fields
// (*Money / *Decimal / *LogPosition) reference protos the engine treats
// as immutable (RISK-05 replaces a whole Position via SetPosition, never
// mutates a Money in place), so copying the map by value is a true
// snapshot. Used by state.Store.Snapshot to hand a consistent, race-free
// portfolio to the compute layer while applies continue on the original.
func (p *Portfolio) Clone() *Portfolio {
	cp := &Portfolio{
		id:               p.id,
		displayName:      p.displayName,
		baseCurrency:     p.baseCurrency,
		cashBalance:      p.cashBalance,
		totalMarketValue: p.totalMarketValue,
		positionCount:    p.positionCount,
		asOf:             p.asOf,
		logPosition:      p.logPosition,
		positions:        make(map[InstrumentID]Position, len(p.positions)),
	}
	for k, v := range p.positions {
		cp.positions[k] = v
	}
	return cp
}

// Position returns the holding for the given instrument and true, or
// the zero Position and false when the portfolio has no position.
func (p *Portfolio) Position(id InstrumentID) (Position, bool) {
	pos, ok := p.positions[id]
	return pos, ok
}

// Positions returns a snapshot of all current positions in stable
// lexicographic instrument-ID order. The returned slice is owned by
// the caller — Portfolio's internal map is unchanged.
func (p *Portfolio) Positions() []Position {
	out := make([]Position, 0, len(p.positions))
	for _, pos := range p.positions {
		out = append(out, pos)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].InstrumentID < out[j].InstrumentID
	})
	return out
}

// SetPosition is the single-writer mutation hook used by RISK-05's
// state-apply layer. A zero-quantity Position is retained for audit
// lineage; explicit removal happens via Forget.
func (p *Portfolio) SetPosition(pos Position) {
	p.positions[pos.InstrumentID] = pos
	if pos.AsOf.After(p.asOf) {
		p.asOf = pos.AsOf
	}
}

// Forget removes a position. Used when a state event explicitly
// retires the instrument from the portfolio.
func (p *Portfolio) Forget(id InstrumentID) {
	delete(p.positions, id)
}

// SetSnapshot records the durable-log coordinate associated with the
// most recent snapshot apply, so RISK-04's bootstrap path knows where
// to resume the log.
func (p *Portfolio) SetSnapshot(asOf time.Time, lp *commonpb.LogPosition) {
	p.asOf = asOf
	p.logPosition = lp
}

// SetAggregate bulk-updates the portfolio-level fields from a
// PortfolioState apply (RISK-05). asOf monotonicity is the caller's
// responsibility — state.Store serializes applies per-portfolio so
// the AsOf check happens at the apply-layer, not here.
func (p *Portfolio) SetAggregate(u AggregateUpdate) {
	p.displayName = u.DisplayName
	p.baseCurrency = u.BaseCurrency
	p.cashBalance = u.CashBalance
	p.totalMarketValue = u.TotalMarketValue
	p.positionCount = u.PositionCount
	if u.AsOf.After(p.asOf) {
		p.asOf = u.AsOf
	}
}

// ClearPositions removes every tracked position. Used by RISK-05's
// snapshot-apply path: a PortfolioSnapshot is a full reset, so the
// existing position set is discarded before the snapshot's positions
// are applied.
func (p *Portfolio) ClearPositions() {
	p.positions = make(map[InstrumentID]Position)
}

// Position is one instrument holding within a portfolio. Quantity is
// signed: positive = long, negative = short, zero = closed but
// tracked. The engine retains zero-quantity positions for audit
// lineage until explicitly forgotten via a state event (Portfolio.Forget).
//
// Fields mirror domain.v1.PositionState (kanz-schemas) but as a
// plain Go struct — proto types are wire-shape, this is the
// engine's working-shape (see package doc).
type Position struct {
	InstrumentID InstrumentID
	// Quantity is the signed holding size. Decimal (not float64) per
	// the BRAIN "double is banned for prices/sizes/money" rule.
	Quantity *commonpb.Decimal
	// AveragePrice is the volume-weighted entry price per unit. nil for
	// a zero-quantity position.
	AveragePrice *commonpb.Decimal
	// MarketValue is the current mark-to-market value of the position.
	MarketValue *commonpb.Money
	// MarketValueUncertainty is the absolute one-sigma uncertainty
	// band around MarketValue, used by RISK-08's propagation through
	// measure computations. nil ⇒ no uncertainty (treated as zero
	// when propagating; matches the api/v1 Measure.UncertaintyAbs
	// "nil ⇒ no uncertainty propagated" convention).
	//
	// The wire proto (domain.v1.PositionState) does not yet carry a
	// matching field; the engine populates this from staleness
	// signals (RISK-11) or, since MODEL-01g, from the volatility model
	// (compute.PopulateUncertainty fills it with |MarketValue| × σ at
	// each snapshot boundary when a vol model is wired).
	MarketValueUncertainty *commonpb.Money
	// RealizedPnL is cumulative realized profit and loss over the
	// position's life; UnrealizedPnL is the mark-to-market gain/loss
	// on the open quantity.
	RealizedPnL   *commonpb.Money
	UnrealizedPnL *commonpb.Money
	// AsOf is the timestamp of the state event that produced this
	// position version.
	AsOf time.Time
}
