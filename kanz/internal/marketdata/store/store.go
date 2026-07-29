// Package store is the market-data time-series store (MODEL-01b): the durable
// home of the historical price series the real risk-analytics plane reads
// point-in-time-correct. The market-data ingestion service folds market.v1
// events into Observations here; the historical-returns provider (MODEL-01c)
// and the volatility/covariance path (MODEL-01g) read them back.
//
// # The stored record, not the wire/proto type
//
// Observation is the Go-native durable shape of reference.v1.PriceObservation
// (MODEL-01a) — the same two-layer split as persist.PortfolioRecord vs the
// domain.Portfolio working type, or commonpb value types vs SQL columns. The
// proto is the wire/publish contract; this is the row a store writes and reads.
// Price stays an exact common.v1.Decimal end-to-end (double is banned for
// prices, EVT-10).
//
// # Bitemporal point-in-time correctness (the load-bearing property, MODEL-01i)
//
// Every Observation carries two times: ObservationTime is the domain time the
// price *applies to* (the close it marks); KnowledgeTime is when that value
// *became known* to Kanz. A late vendor correction restates a past
// ObservationTime with a newer KnowledgeTime. A read scoped to a knowledge
// horizon (Query.AsOf) sees only observations with KnowledgeTime <= AsOf and,
// for each ObservationTime, the latest such version — so a backtest as of T
// reproduces exactly what was knowable at T and a correction that arrived after
// T never leaks backward. A store that kept "latest value wins" would silently
// leak the future into every backtest; this contract forces the honest read on
// every implementation (the same stance as the PRED-11 point-in-time feature
// store).
package store

import (
	"context"
	"errors"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

// PriceKind mirrors reference.v1.PriceKind — which mark an Observation carries.
// The numbering matches the proto so the two never drift.
type PriceKind int32

const (
	PriceKindUnspecified   PriceKind = 0
	PriceKindClose         PriceKind = 1
	PriceKindAdjustedClose PriceKind = 2
	PriceKindOpen          PriceKind = 3
	PriceKindVWAP          PriceKind = 4
	PriceKindMid           PriceKind = 5
	PriceKindSettlement    PriceKind = 6
	PriceKindLast          PriceKind = 7
)

// Observation is one marked price on an instrument's historical series — the
// durable analogue of reference.v1.PriceObservation.
type Observation struct {
	// InstrumentID is the canonical Kanz instrument id (== market.v1
	// instrument_id / envelope partition_key).
	InstrumentID string
	// ObservationTime is the domain time the price applies to — the series'
	// ordering axis.
	ObservationTime time.Time
	// Price is the marked price, exact.
	Price *commonpb.Decimal
	// Kind disambiguates which price this is.
	Kind PriceKind
	// CurrencyCode is the ISO 4217 code the price is quoted in. May be empty:
	// market.v1 events carry no currency, so the ingestion path leaves it for a
	// later reference-data join (reference.v1.InstrumentReference.currency_code)
	// — returns are currency-agnostic per single instrument, so VaR does not
	// block on it.
	CurrencyCode string
	// KnowledgeTime is when this value became known to Kanz — the bitemporal
	// "as-recorded" axis that makes point-in-time reads correct.
	KnowledgeTime time.Time
}

// validate enforces the invariants every stored Observation must hold. Currency
// is intentionally optional (see the field doc).
func (o Observation) validate() error {
	switch {
	case o.InstrumentID == "":
		return errors.New("store: observation has empty instrument_id")
	case o.ObservationTime.IsZero():
		return errors.New("store: observation has zero observation_time")
	case o.Price == nil:
		return errors.New("store: observation has nil price")
	case o.Kind == PriceKindUnspecified:
		return errors.New("store: observation has unspecified kind")
	case o.KnowledgeTime.IsZero():
		return errors.New("store: observation has zero knowledge_time")
	}
	return nil
}

// Query selects a slice of an instrument's history. The window is on
// ObservationTime; AsOf is the bitemporal knowledge horizon.
type Query struct {
	// InstrumentID is required.
	InstrumentID string
	// Kind filters to one price kind; PriceKindUnspecified ⇒ any kind.
	Kind PriceKind
	// Start / End bound ObservationTime inclusively. A zero Start ⇒ unbounded
	// below; a zero End ⇒ unbounded above.
	Start time.Time
	End   time.Time
	// AsOf is the knowledge horizon: only observations with KnowledgeTime <=
	// AsOf are visible, and for each ObservationTime only the latest such
	// version. A zero AsOf ⇒ latest knowledge (no point-in-time filter) — the
	// live read; a non-zero AsOf is the backtest/replay read.
	AsOf time.Time
}

// Store is the market-data time-series store. Implementations must be safe for
// concurrent use.
type Store interface {
	// Put stores observations idempotently: re-putting the identical
	// (instrument, observation_time, kind, knowledge_time) tuple is a no-op
	// (a price at one knowledge_time is immutable — a restatement is a new
	// knowledge_time, not a mutation). Each observation is validated; an
	// invalid one fails the whole batch.
	Put(ctx context.Context, obs []Observation) error

	// History returns the point-in-time-correct series for q, ordered by
	// ObservationTime ascending: filtered to the window + kind, restricted to
	// the AsOf knowledge horizon, with each ObservationTime collapsed to its
	// latest-known version.
	History(ctx context.Context, q Query) ([]Observation, error)

	// LatestAsOf returns the single most recent point-in-time price for an
	// instrument/kind as of asOf — the spot read for MODEL-01c/g. Both axes are
	// bounded at asOf (ObservationTime <= asOf AND KnowledgeTime <= asOf), so it
	// answers "the price we believed was current at asOf". Reports ok=false when
	// no such observation exists. A zero asOf means now (latest of everything).
	LatestAsOf(ctx context.Context, instrumentID string, kind PriceKind, asOf time.Time) (Observation, bool, error)

	// Ping checks store reachability for the readiness probe.
	Ping(ctx context.Context) error
}
