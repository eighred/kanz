package factor

import (
	"context"
	"sort"

	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
)

// UnclassifiedSector is the catch-all bucket for positions whose instrument has
// no sector mapping (unknown instrument, or a sectorless asset like FX/rates).
// Bucketing them rather than dropping them keeps total sector exposure
// reconciled with total instrument exposure.
const UnclassifiedSector = "UNCLASSIFIED"

// SectorExposure completes the RISK-06 ExposureBySector deferral: it buckets the
// portfolio's positions by their instrument's sector (via the classifier),
// summing gross/net per bucket — the same shape as compute.ComputeExposure's
// ExposureByCurrency dimension.
//
// Like the RISK-07 measures, it sums only positions denominated in the
// portfolio base currency (the sector aggregate is base-currency; cross-currency
// roll-up needs an FX layer this package does not have). Positions with a nil
// MarketValue are skipped (matching ComputeExposure). Output is sorted by bucket
// key for deterministic iteration.
func SectorExposure(ctx context.Context, p *domain.Portfolio, c Classifier) []domain.Exposure {
	base := p.BaseCurrency()
	baseStr := string(base)
	bySector := make(map[string]*domain.Exposure)
	for _, pos := range p.Positions() {
		if !pos.InBaseCurrency(base) {
			continue
		}
		mv := pos.MarketValue
		key := UnclassifiedSector
		if cl, ok := c.Classify(ctx, string(pos.InstrumentID), p.AsOf()); ok && !cl.Sector.IsZero() {
			key = cl.Sector.Key()
		}
		bucket, ok := bySector[key]
		if !ok {
			bucket = &domain.Exposure{
				Dimension: domain.ExposureBySector,
				Key:       key,
				Gross:     compute.ZeroMoney(baseStr),
				Net:       compute.ZeroMoney(baseStr),
			}
			bySector[key] = bucket
		}
		bucket.Gross = compute.AddMoney(bucket.Gross, compute.AbsMoney(mv))
		bucket.Net = compute.AddMoney(bucket.Net, mv)
	}

	keys := make([]string, 0, len(bySector))
	for k := range bySector {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]domain.Exposure, 0, len(keys))
	for _, k := range keys {
		out = append(out, *bySector[k])
	}
	return out
}

// ComputeExposure returns the full ExposureSet including the SECTOR dimension,
// layering SectorExposure onto compute.ComputeExposure's instrument + currency
// dimensions. The engine/query path uses this when a Classifier is wired; absent
// one it calls compute.ComputeExposure directly (two dimensions) — that
// signature stays untouched, as the RISK-06 deferral note intended.
func ComputeExposure(ctx context.Context, p *domain.Portfolio, c Classifier) *domain.ExposureSet {
	base := compute.ComputeExposure(p)
	items := base.Items()
	items = append(items, SectorExposure(ctx, p, c)...)
	return domain.NewExposureSet(p.ID(), p.AsOf(), items)
}
