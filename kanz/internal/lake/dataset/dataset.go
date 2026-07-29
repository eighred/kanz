// Package dataset materializes point-in-time-correct feature rows (LAKE-01b):
// the training/backtest datasets the lakehouse exists to produce. Every read is
// scoped to a knowledge horizon (Sample.AsOf), so a value that became known
// AFTER AsOf — a late vendor correction, a backfill — never leaks into a row.
// That no-future-leakage property is what makes a dataset honest: a model
// trained or a strategy backtested on it sees exactly what was knowable at each
// point in time (MODEL-01i bitemporal store + the PRED-11 point-in-time stance).
//
// Features are derived by the SAME compute code the live risk engine uses
// (returns.StoreReturnsProvider / ReturnsVolModel over the MODEL-01b store), so
// a feature value materialized here is bit-for-bit the value the engine computed
// live — the property LAKE-01e's backtest-reproduces-live test leans on.
package dataset

import (
	"context"
	"math"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/marketdata/returns"
	"github.com/eighred/kanz/internal/marketdata/store"
)

// FeatureSource is the MLOPS-01f point-in-time feature-store seam: features
// beyond the market-derived ones, keyed by (instrument, asOf) and likewise
// knowledge-horizon-correct. nil ⇒ market-derived features only (MLOPS-01f is
// not built yet; this is where it joins in).
type FeatureSource interface {
	FeaturesAsOf(ctx context.Context, instrumentID string, asOf time.Time) (map[string]float64, error)
}

// Sample requests one row: the features for InstrumentID as known at AsOf.
type Sample struct {
	InstrumentID string
	AsOf         time.Time
}

// Row is a materialized point-in-time feature row.
type Row struct {
	InstrumentID string             `json:"instrument_id"`
	AsOf         time.Time          `json:"as_of"`
	Features     map[string]float64 `json:"features"`
	// Complete reports whether every market-derived feature was computable from
	// the point-in-time history available at AsOf. Incomplete rows are still
	// emitted (a dataset gap is itself signal) but flagged so a trainer can drop
	// them rather than silently learning on partial inputs.
	Complete bool `json:"complete"`
}

// Config parameterizes the market-derived features.
type Config struct {
	// Window is the trailing return/volatility lookback. 0 ⇒ the compute default.
	Window int
	// Kind is the price mark spot/return features read. Unspecified ⇒ close.
	Kind store.PriceKind
}

// Materializer builds point-in-time feature rows over the MODEL-01b store,
// optionally joined with an MLOPS-01f FeatureSource.
type Materializer struct {
	store    store.Store
	returns  *returns.StoreReturnsProvider
	vol      *returns.ReturnsVolModel
	features FeatureSource
	kind     store.PriceKind
	window   int
}

// NewMaterializer wires the market-derived feature path over s. features may be
// nil. Both the returns provider and the vol model read s with an AsOf horizon,
// so the whole row is point-in-time correct.
func NewMaterializer(s store.Store, features FeatureSource, cfg Config) *Materializer {
	kind := cfg.Kind
	if kind == store.PriceKindUnspecified {
		kind = store.PriceKindClose
	}
	rp := returns.NewStoreReturnsProvider(s, returns.ReturnsConfig{Window: cfg.Window, Kind: kind})
	return &Materializer{
		store:    s,
		returns:  rp,
		vol:      returns.NewReturnsVolModel(rp, cfg.Window),
		features: features,
		kind:     kind,
		window:   cfg.Window,
	}
}

// Materialize builds the feature row for one sample. Store/feature-source errors
// are genuine data-plane failures and propagate; insufficient history is not an
// error — it flags the row incomplete (mirroring the VaR/vol "degrade, don't
// fail" contract).
func (m *Materializer) Materialize(ctx context.Context, s Sample) (Row, error) {
	row := Row{InstrumentID: s.InstrumentID, AsOf: s.AsOf, Features: map[string]float64{}, Complete: true}

	// Spot: the price believed current at AsOf — both axes bounded at AsOf, so a
	// correction stamped with a later KnowledgeTime is invisible.
	spot, ok, err := m.store.LatestAsOf(ctx, s.InstrumentID, m.kind, s.AsOf)
	if err != nil {
		return Row{}, err
	}
	if ok {
		if f, fok := decimalToFloat(spot.Price); fok {
			row.Features["spot"] = f
		} else {
			row.Complete = false
		}
	} else {
		row.Complete = false
	}

	// Trailing returns (point-in-time series).
	rets, err := m.returns.Returns(ctx, s.InstrumentID, s.AsOf, m.window)
	if err != nil {
		return Row{}, err
	}
	if len(rets) > 0 {
		row.Features["ret_last"] = rets[len(rets)-1]
		row.Features["ret_mean"] = mean(rets)
	} else {
		row.Complete = false
	}

	// Volatility: sample stdev of the same point-in-time returns.
	sigma, ok, err := m.vol.Volatility(ctx, s.InstrumentID, s.AsOf)
	if err != nil {
		return Row{}, err
	}
	if ok {
		row.Features["vol"] = sigma
	} else {
		row.Complete = false
	}

	// MLOPS-01f join (point-in-time too); namespaced so it never collides with a
	// market-derived key.
	if m.features != nil {
		extra, err := m.features.FeaturesAsOf(ctx, s.InstrumentID, s.AsOf)
		if err != nil {
			return Row{}, err
		}
		for k, v := range extra {
			row.Features["feat_"+k] = v
		}
	}
	return row, nil
}

// MaterializeAll materializes a batch in order. Rows preserve the input order so
// the dataset is reproducible; the first error aborts.
func (m *Materializer) MaterializeAll(ctx context.Context, samples []Sample) ([]Row, error) {
	out := make([]Row, 0, len(samples))
	for _, s := range samples {
		row, err := m.Materialize(ctx, s)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// decimalToFloat converts an exact common.v1.Decimal to float64 for use as a
// model feature. Prices stay exact Decimal on the wire/store (double is banned,
// EVT-10); the lossy conversion is confined to the ML feature plane, where a
// float64 input is the contract.
func decimalToFloat(d *commonpb.Decimal) (float64, bool) {
	if d == nil {
		return 0, false
	}
	return float64(d.GetCoefficient()) * math.Pow10(int(d.GetExponent())), true
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}
