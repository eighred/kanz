package returnbasis

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"regexp"
	"sort"
	"time"

	"github.com/eighred/kanz/internal/performance"
	"github.com/eighred/kanz/internal/refdata"
	performancepb "github.com/eighred/kanz/kanz-schemas-go/performance/v1"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var sectorCode = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_]{0,31}$`)

type datedSectors struct {
	horizon  time.Time
	taxonomy string
	codes    map[string]string
}

func (c datedSectors) Sector(_ context.Context, id string, horizon time.Time) (string, bool) {
	code := c.codes[id]
	if !horizon.Equal(c.horizon) || !sectorCode.MatchString(code) || !sectorCode.MatchString(c.taxonomy) {
		return "", false
	}
	return (refdata.Sector{Taxonomy: c.taxonomy, Code: code}).Key(), true
}

func attributeSectors(ctx context.Context, tx pgx.Tx, e Evidence, report *performancepb.DailyReturnAssessment,
	periods []contributionPeriod, events []*ledger.Event,
) error {
	out := report.SectorAttribution
	unassessed := func(reason string) error {
		out.Result = &performancepb.DailySectorAttribution_UnassessedReason{UnassessedReason: reason}
		return nil
	}
	if len(periods) != 1 || e.PositionBasis.TradeCoverage != nil {
		return unassessed("sector_regime_unsupported")
	}
	period := periods[0]
	if period.from.nav.Cash.Sign() != 0 || period.to.nav.Cash.Sign() != 0 ||
		period.from.nav.Accrued.Sign() != 0 || period.to.nav.Accrued.Sign() != 0 {
		return unassessed("sector_regime_unsupported")
	}
	start, end := report.Start.AsTime(), report.End.AsTime()
	for _, event := range events {
		if event.Effective.After(start) && !event.Effective.After(end) {
			return unassessed("sector_regime_unsupported")
		}
	}
	if report.GetBenchmarkComparison().GetMeasured() == nil {
		return unassessed("sector_benchmark_coverage_missing")
	}
	o := e.Benchmark.Observation
	if o.Composition == nil || len(o.Composition.Constituents) == 0 {
		return unassessed("sector_benchmark_coverage_missing")
	}
	out.BenchmarkCompositionReference = o.Composition.Reference
	benchmark, benchWeight, benchReturn := benchmarkAttributionRows(o.Composition)
	if benchWeight.Cmp(big.NewRat(1, 1)) != 0 {
		return unassessed("sector_benchmark_coverage_missing")
	}
	if benchReturn.Cmp(benchmarkFraction(o)) != 0 {
		return unassessed("sector_benchmark_not_reconciled")
	}
	portfolio, portfolioReturn := portfolioAttributionRows(period)
	wantPortfolio, _ := exactReturn(e)
	if len(portfolio) == 0 || portfolioReturn.Cmp(wantPortfolio) != 0 {
		return unassessed("sector_portfolio_unassessed")
	}
	classifier := datedSectors{horizon: e.AsOf}
	var known time.Time
	var sectorsJSON []byte
	err := tx.QueryRow(ctx, `SELECT reference, known_at, taxonomy, sectors
		FROM return_basis_sector_classifications
		WHERE tenant_id=public.app_current_tenant() AND tenant_id=$1 AND portfolio_id=$2 AND currency=$3
		  AND close_date=$4::text::date AND known_at <= $5 AND recorded_at <= $5
		ORDER BY known_at DESC LIMIT 1`, e.Tenant, e.Portfolio, e.Currency, e.CloseDate, e.AsOf).
		Scan(&out.ClassificationReference, &known, &classifier.taxonomy, &sectorsJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return unassessed("sector_classification_missing")
	}
	if err != nil {
		return err
	}
	if !identifier.MatchString(out.ClassificationReference) {
		out.ClassificationReference = ""
		return unassessed("sector_classification_missing")
	}
	out.ClassificationKnownAt = timestamppb.New(known)
	if json.Unmarshal(sectorsJSON, &classifier.codes) != nil {
		return unassessed("sector_classification_missing")
	}
	// The existing bucketer reports its unknown bucket for other callers.
	// This governed report requires every instrument on both sides to resolve.
	sectors, err := performance.BucketBySector(ctx, classifier, e.AsOf, portfolio, benchmark)
	if err != nil {
		return err
	}
	for _, sector := range sectors {
		if sector.Sector == performance.UnclassifiedSector {
			return unassessed("sector_classification_missing")
		}
		if sector.PortfolioWeight <= 0 || sector.BenchmarkWeight <= 0 {
			return unassessed("sector_coverage_mismatch")
		}
	}
	active, _ := new(big.Rat).Sub(portfolioReturn, benchReturn).Float64()
	result := &performancepb.AttributionResult{
		PortfolioId: report.PortfolioId, BenchmarkId: o.BenchmarkID, Start: report.Start, End: report.End, AsOf: report.AsOf, ActiveReturn: active,
	}
	for _, sector := range performance.Brinson(sectors) {
		result.Sectors = append(result.Sectors, &performancepb.SectorAttribution{
			Sector: sector.Sector, PortfolioWeight: sector.PortfolioWeight, BenchmarkWeight: sector.BenchmarkWeight,
			PortfolioReturn: sector.PortfolioReturn, BenchmarkReturn: sector.BenchmarkReturn,
			Allocation: sector.Allocation, Selection: sector.Selection, Interaction: sector.Interaction,
		})
		result.TotalAllocation += sector.Allocation
		result.TotalSelection += sector.Selection
		result.TotalInteraction += sector.Interaction
	}
	// We reconcile the underlying weighted returns in exact rationals above.
	// The existing statistical kernel uses binary64 for the published effects.
	const tolerance = 1e-12
	sum := result.TotalAllocation + result.TotalSelection + result.TotalInteraction
	if !(math.Abs(sum-active) <= tolerance) ||
		!(math.Abs(report.BenchmarkComparison.GetMeasured().ActiveReturn-active) <= tolerance) {
		return unassessed("sector_effects_not_reconciled")
	}
	out.Result = &performancepb.DailySectorAttribution_Measured{Measured: result}
	return nil
}

func portfolioAttributionRows(period contributionPeriod) ([]performance.WeightedReturn, *big.Rat) {
	rows := make([]performance.WeightedReturn, 0, len(period.from.positions))
	totalReturn := new(big.Rat)
	if len(period.from.positions) != len(period.to.positions) {
		return nil, totalReturn
	}
	ids := make([]string, 0, len(period.from.positions))
	for id := range period.from.positions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		opening, closing := period.from.positions[id], period.to.positions[id]
		if opening.Sign() <= 0 || closing == nil {
			return nil, totalReturn
		}
		pnl := new(big.Rat).Sub(closing, opening)
		totalReturn.Add(totalReturn, new(big.Rat).Quo(pnl, period.from.nav.Total))
		rows = append(rows, weightedReturnRow(id, new(big.Rat).Quo(opening, period.from.nav.Total), new(big.Rat).Quo(pnl, opening)))
	}
	return rows, totalReturn
}

func benchmarkAttributionRows(b *BenchmarkComposition) ([]performance.WeightedReturn, *big.Rat, *big.Rat) {
	rows := make([]performance.WeightedReturn, 0, len(b.Constituents))
	totalWeight, totalReturn := new(big.Rat), new(big.Rat)
	for _, constituent := range b.Constituents {
		weight, _ := constituent.Weight.Rat()
		rate, _ := constituent.Return.Rat()
		totalWeight.Add(totalWeight, weight)
		totalReturn.Add(totalReturn, new(big.Rat).Mul(weight, rate))
		rows = append(rows, weightedReturnRow(constituent.InstrumentID, weight, rate))
	}
	return rows, totalWeight, totalReturn
}

func weightedReturnRow(id string, weight, rate *big.Rat) performance.WeightedReturn {
	w, _ := weight.Float64()
	r, _ := rate.Float64()
	return performance.WeightedReturn{InstrumentID: id, Weight: w, Return: r}
}
