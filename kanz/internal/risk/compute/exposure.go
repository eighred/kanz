package compute

import (
	"sort"

	"github.com/kanz-eng/kanz/internal/risk/domain"
)

// ComputeExposure builds the canonical ExposureSet for a portfolio.
// Two dimensions are emitted:
//
//   - ExposureByInstrument — one Exposure per position with a
//     mark-to-market value. Gross = |MarketValue|, Net = MarketValue
//     (signed). The canonical level RISK-06's downstream aggregators
//     derive from.
//
//   - ExposureByCurrency — positions grouped by their MarketValue
//     currency code. Gross / Net are the sum across the bucket. No
//     cross-currency conversion happens here — that needs an FX
//     layer outside this package, and bucketing on currency first
//     means the per-bucket sum is well-defined without it.
//
// ExposureBySector is in the closed domain enum (RISK-03) but
// deferred: deriving it needs an instrument→sector reference table
// the engine does not yet own. A follow-up can add it by injecting
// a sector-lookup function, leaving this signature unchanged for
// callers that only care about the two canonical dimensions.
//
// Positions with nil MarketValue are skipped — the engine has no
// way to express their exposure until a mark arrives. Zero-quantity
// positions (kept for audit lineage per RISK-05) flow through
// because their MarketValue is typically zero, producing a
// zero-row exposure that is informative (the position still
// exists) without distorting the aggregate.
//
// AsOf is propagated from the Portfolio so the api/v1 caller can
// see the freshness of the underlying state.
func ComputeExposure(p *domain.Portfolio) *domain.ExposureSet {
	positions := p.Positions()
	// One row per marked instrument + a handful of currency buckets.
	items := make([]domain.Exposure, 0, len(positions)+8)

	// Per-currency gross/net folded with decAccum (O(1) allocs/bucket) rather
	// than addMoney per position (LATENCY-01c). The per-instrument Money below
	// is genuine output, not folding overhead, so it stays.
	type ccyBucket struct{ gross, net decAccum }
	byCurrency := make(map[string]*ccyBucket)

	for _, pos := range positions {
		mv := pos.MarketValue
		if mv == nil {
			continue
		}
		items = append(items, domain.Exposure{
			Dimension: domain.ExposureByInstrument,
			Key:       string(pos.InstrumentID),
			Gross:     absMoney(mv),
			Net:       mv,
		})

		ccy := mv.CurrencyCode
		bucket, ok := byCurrency[ccy]
		if !ok {
			bucket = &ccyBucket{}
			byCurrency[ccy] = bucket
		}
		bucket.gross.add(mv.Amount, true /*absolute*/)
		bucket.net.add(mv.Amount, false /*signed*/)
	}

	// Stable iteration order for tests + audit logs. ExposureSet
	// itself sorts on read, but giving the constructor pre-sorted
	// input keeps copies predictable in the debugger.
	keys := make([]string, 0, len(byCurrency))
	for k := range byCurrency {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		bucket := byCurrency[k]
		items = append(items, domain.Exposure{
			Dimension: domain.ExposureByCurrency,
			Key:       k,
			Gross:     bucket.gross.money(k),
			Net:       bucket.net.money(k),
		})
	}

	return domain.NewExposureSet(p.ID(), p.AsOf(), items)
}
