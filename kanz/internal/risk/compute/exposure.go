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
	items := make([]domain.Exposure, 0, 2*len(positions))
	byCurrency := make(map[string]*domain.Exposure)

	for _, pos := range positions {
		mv := pos.MarketValue
		if mv == nil {
			continue
		}
		gross := absMoney(mv)
		net := mv

		items = append(items, domain.Exposure{
			Dimension: domain.ExposureByInstrument,
			Key:       string(pos.InstrumentID),
			Gross:     gross,
			Net:       net,
		})

		ccy := mv.CurrencyCode
		bucket, ok := byCurrency[ccy]
		if !ok {
			bucket = &domain.Exposure{
				Dimension: domain.ExposureByCurrency,
				Key:       ccy,
				Gross:     zeroMoney(ccy),
				Net:       zeroMoney(ccy),
			}
			byCurrency[ccy] = bucket
		}
		bucket.Gross = addMoney(bucket.Gross, gross)
		bucket.Net = addMoney(bucket.Net, net)
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
		items = append(items, *byCurrency[k])
	}

	return domain.NewExposureSet(p.ID(), p.AsOf(), items)
}
