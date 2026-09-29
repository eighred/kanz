package varmodel

import (
	"context"
	"maps"
	"math/big"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/marketdata/returns"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

// One panel owns both empirical P&L and Monte-Carlo covariance. Provenance is
// cloned per evaluation because registered measure closures run concurrently.
func portfolioPanel(ctx context.Context, p *domain.Portfolio, rp compute.ReturnsProvider, window int, cov *compute.Coverage, prov *v1.MeasureProvenance, requirePath bool) (returns.Panel, []float64, bool) {
	var ids []string
	positions := p.Positions()
	for _, pos := range positions {
		if pos.InBaseCurrency(p.BaseCurrency()) {
			ids = append(ids, string(pos.InstrumentID))
		}
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
	values := make([]float64, len(panel.Instruments))
	row := map[string]int{}
	for i, id := range panel.Instruments {
		row[id] = i
	}
	excluded := new(big.Rat)
	excludedDecimal := &commonpb.Decimal{}
	excludedKnown := true
	for _, pos := range positions {
		if !pos.InBaseCurrency(p.BaseCurrency()) {
			continue
		}
		id := string(pos.InstrumentID)
		if _, missing := panel.Missing[id]; missing || failed {
			if missing {
				cov.Exclude(pos.InstrumentID, SkipNoReturns)
			}
			amount, valid := dec.FromProtoChecked(pos.MarketValue.Amount)
			if valid {
				excluded.Add(excluded, amount.Abs(amount))
				absolute, fits := dec.Abs(pos.MarketValue.Amount)
				if fits {
					excludedDecimal, fits = dec.Add(excludedDecimal, absolute)
				}
				excludedKnown = excludedKnown && fits
			} else {
				excludedKnown = false
			}
			continue
		}
		values[row[id]] += dec.Float64Or(pos.MarketValue.Amount, 0)
		cov.Contributed++
	}
	// Metadata is still money: never serialize a rounded exclusion as exact.
	if excludedKnown && dec.FromProto(excludedDecimal).Cmp(excluded) == 0 {
		prov.ExcludedGross = excludedDecimal
	} else {
		prov.ExcludedGross = nil
		cov.ExcludeWhole("excluded_exposure_unrepresentable")
	}
	if failed {
		reason := SkipInsufficientHistory
		if err == nil {
			reason = "non_contiguous_history"
		}
		cov.ExcludeWhole(reason)
		return panel, nil, false
	}
	return panel, values, true
}
