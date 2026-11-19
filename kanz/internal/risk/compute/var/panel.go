package varmodel

import (
	"context"
	"maps"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/marketdata/returns"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
)

// One panel owns both empirical P&L and Monte-Carlo covariance. Provenance is
// cloned per evaluation because registered measure closures run concurrently.
func portfolioPanel(ctx context.Context, p *domain.Portfolio, rp compute.ReturnsProvider, window int, cov *compute.Coverage, prov *v1.MeasureProvenance, requirePath bool) (returns.Panel, []float64, bool) {
	var ids []string
	var excluded compute.ExcludedExposure
	positions := p.Positions()
	marks := make(map[string]float64, len(positions))
	for _, pos := range positions {
		if !pos.InBaseCurrency(p.BaseCurrency()) {
			continue
		}
		value, ok := dec.Float64(pos.MarketValue.Amount)
		if !ok {
			cov.Exclude(pos.InstrumentID, compute.SkipNoMarketValue)
			excluded.Add(pos.MarketValue.Amount)
			continue
		}
		// An unvalued holding cannot narrow the dated intervals of the priced book.
		id := string(pos.InstrumentID)
		ids = append(ids, id)
		marks[id] = value
	}
	panel, err := returns.Load(ctx, rp, ids, p.AsOf(), window)
	prov.Params = maps.Clone(prov.Params)
	if prov.Params == nil {
		prov.Params = map[string]string{}
	}
	maps.Copy(prov.Params, panel.Params())
	delete(prov.Params, "panel_digest")
	prov.InputDigest = panel.Digest
	if panel.Contiguous() {
		prov.Params["panel_contiguous"] = "true"
	} else {
		prov.Params["panel_contiguous"] = "false"
	}
	failed := err != nil || (requirePath && !panel.Contiguous())
	for _, id := range ids {
		if _, missing := panel.Missing[id]; missing || failed {
			pos, _ := p.Position(domain.InstrumentID(id))
			if missing {
				cov.Exclude(pos.InstrumentID, SkipNoReturns)
			}
			excluded.Add(pos.MarketValue.Amount)
		}
	}
	prov.ExcludedGross = excluded.Result(cov)
	if failed {
		if len(ids) == 0 && cov.Result().ExcludedCount > 0 {
			// No readable marks were available to request return history for.
			return panel, nil, false
		}
		reason := SkipInsufficientHistory
		if err == nil {
			reason = "non_contiguous_history"
		}
		cov.ExcludeWhole(reason)
		return panel, nil, false
	}
	values := make([]float64, len(panel.Instruments))
	for i, id := range panel.Instruments {
		values[i] = marks[id]
	}
	cov.Contributed += len(values)
	return panel, values, true
}
