package performance

import (
	"context"
	"math"
	"sort"
	"time"
)

// Brinson attribution (PERF-01d): decompose active return into allocation,
// selection, and interaction effects by sector, with Carino multi-period
// linking so the per-sector effects sum to the geometrically-linked active
// return over a multi-period window.
//
// # The classifier is performance's own seam (RISK-02)
//
// Sector bucketing mirrors the risk module's factor.Classifier conceptually, but
// performance lives outside kanz/internal/risk and cannot import it. It declares
// its own Classifier — the same "carry your own seam, built on the shared
// schema" stance COMP-01 took with its Book. The bucket key is "taxonomy:code"
// (e.g. "GICS:40"), identical to factor.Sector.Key() so a shared reference store
// can satisfy both without translation.

// Classifier resolves an instrument's sector bucket point-in-time. ok=false ⇒
// unclassified (bucketed into UnclassifiedSector so the decomposition still
// reconciles with the total).
type Classifier interface {
	Sector(ctx context.Context, instrumentID string, asOf time.Time) (string, bool)
}

// UnclassifiedSector is the catch-all bucket for instruments the classifier does
// not know — an unknown instrument is bucketed, never dropped, so the sector
// decomposition reconciles with the portfolio total (the same reconciliation
// invariant as factor.SectorExposure).
const UnclassifiedSector = "UNCLASSIFIED"

// WeightedReturn is one instrument's weight and period return on one side
// (portfolio or benchmark) — the input rows BucketBySector aggregates.
type WeightedReturn struct {
	InstrumentID string
	Weight       float64
	Return       float64
}

// BucketBySector aggregates portfolio and benchmark instrument rows into the
// per-sector SectorData a Brinson decomposition needs, classifying each
// instrument point-in-time via the classifier. Within a sector the weight is the
// sum of instrument weights and the return is the weight-weighted average of
// instrument returns (Σ wᵢrᵢ / Σ wᵢ). An instrument the classifier doesn't know
// falls into UnclassifiedSector — bucketed, never dropped, so the decomposition
// reconciles with the totals. Output is sorted by sector. A nil classifier
// buckets everything as unclassified (the no-classifier degradation).
func BucketBySector(ctx context.Context, classifier Classifier, asOf time.Time, portfolio, benchmark []WeightedReturn) []SectorData {
	type acc struct{ pw, pwr, bw, bwr float64 }
	buckets := map[string]*acc{}
	get := func(sector string) *acc {
		a, ok := buckets[sector]
		if !ok {
			a = &acc{}
			buckets[sector] = a
		}
		return a
	}
	sectorOf := func(id string) string {
		if classifier == nil {
			return UnclassifiedSector
		}
		if s, ok := classifier.Sector(ctx, id, asOf); ok && s != "" {
			return s
		}
		return UnclassifiedSector
	}
	for _, r := range portfolio {
		a := get(sectorOf(r.InstrumentID))
		a.pw += r.Weight
		a.pwr += r.Weight * r.Return
	}
	for _, r := range benchmark {
		a := get(sectorOf(r.InstrumentID))
		a.bw += r.Weight
		a.bwr += r.Weight * r.Return
	}
	out := make([]SectorData, 0, len(buckets))
	for sector, a := range buckets {
		sd := SectorData{Sector: sector, PortfolioWeight: a.pw, BenchmarkWeight: a.bw}
		if a.pw != 0 {
			sd.PortfolioReturn = a.pwr / a.pw
		}
		if a.bw != 0 {
			sd.BenchmarkReturn = a.bwr / a.bw
		}
		out = append(out, sd)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sector < out[j].Sector })
	return out
}

// SectorData is one sector's weights and returns in the portfolio and the
// benchmark over a single period — the input to the Brinson decomposition.
type SectorData struct {
	Sector          string
	PortfolioWeight float64
	BenchmarkWeight float64
	PortfolioReturn float64
	BenchmarkReturn float64
}

// SectorEffect is the Brinson decomposition for one sector.
type SectorEffect struct {
	Sector          string
	PortfolioWeight float64
	BenchmarkWeight float64
	PortfolioReturn float64
	BenchmarkReturn float64
	Allocation      float64
	Selection       float64
	Interaction     float64
}

// Total is the sector's total contribution to active return.
func (e SectorEffect) Total() float64 { return e.Allocation + e.Selection + e.Interaction }

// Brinson decomposes one period's active return by sector (Brinson-Hood-Beebower):
//
//	allocationᵢ  = (wpᵢ − wbᵢ)·Rbᵢ
//	selectionᵢ   = wbᵢ·(Rpᵢ − Rbᵢ)
//	interactionᵢ = (wpᵢ − wbᵢ)·(Rpᵢ − Rbᵢ)
//
// The three effects sum, across sectors, EXACTLY to the active return
// Rp − Rb = Σ wpᵢRpᵢ − Σ wbᵢRbᵢ (the PERF-01f reconciliation). Effects are
// returned sorted by sector for deterministic output.
func Brinson(sectors []SectorData) []SectorEffect {
	out := make([]SectorEffect, 0, len(sectors))
	for _, s := range sectors {
		dw := s.PortfolioWeight - s.BenchmarkWeight
		dr := s.PortfolioReturn - s.BenchmarkReturn
		out = append(out, SectorEffect{
			Sector:          s.Sector,
			PortfolioWeight: s.PortfolioWeight,
			BenchmarkWeight: s.BenchmarkWeight,
			PortfolioReturn: s.PortfolioReturn,
			BenchmarkReturn: s.BenchmarkReturn,
			Allocation:      dw * s.BenchmarkReturn,
			Selection:       s.BenchmarkWeight * dr,
			Interaction:     dw * dr,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sector < out[j].Sector })
	return out
}

// PeriodReturn is the portfolio and benchmark total return for one sub-period —
// the Carino linking inputs alongside the period's Brinson effects.
type PeriodReturn struct {
	Portfolio float64
	Benchmark float64
}

// PeriodBrinson is one sub-period's Brinson effects plus its total returns.
type PeriodBrinson struct {
	Returns PeriodReturn
	Effects []SectorEffect
}

// carinoCoefficient is the Carino linking coefficient for a period with
// portfolio return rp and benchmark return rb:
//
//	k = (ln(1+rp) − ln(1+rb)) / (rp − rb),  or  1/(1+rp) when rp == rb.
//
// Scaling each period's arithmetic effects by kₜ/k (k the whole-window
// coefficient) makes them sum to the GEOMETRICALLY-linked active return — the
// standard solution to "arithmetic effects don't compound".
func carinoCoefficient(rp, rb float64) float64 {
	if math.Abs(rp-rb) < 1e-12 {
		return 1 / (1 + rp)
	}
	return (math.Log(1+rp) - math.Log(1+rb)) / (rp - rb)
}

// LinkedAttribution is the multi-period-linked Brinson result.
type LinkedAttribution struct {
	Sectors          []SectorEffect // per-sector effects, Carino-linked
	TotalAllocation  float64
	TotalSelection   float64
	TotalInteraction float64
	ActiveReturn     float64 // geometrically-linked Rp − Rb (== Σ sector totals)
}

// LinkCarino links per-period Brinson effects across a multi-period window using
// Carino smoothing: each period's effects are scaled by kₜ/k and summed per
// sector, so the linked per-sector effects sum to the geometrically-linked
// active return (the PERF-01f multi-period reconciliation). A single period is
// returned unscaled (k_t/k = 1).
func LinkCarino(periods []PeriodBrinson) LinkedAttribution {
	// Geometrically-linked total returns over the window.
	gp, gb := 1.0, 1.0
	for _, p := range periods {
		gp *= 1 + p.Returns.Portfolio
		gb *= 1 + p.Returns.Benchmark
	}
	rp, rb := gp-1, gb-1
	k := carinoCoefficient(rp, rb)

	linked := map[string]*SectorEffect{}
	var order []string
	for _, p := range periods {
		kt := carinoCoefficient(p.Returns.Portfolio, p.Returns.Benchmark)
		scale := 1.0
		if k != 0 {
			scale = kt / k
		}
		for _, e := range p.Effects {
			agg, ok := linked[e.Sector]
			if !ok {
				agg = &SectorEffect{Sector: e.Sector}
				linked[e.Sector] = agg
				order = append(order, e.Sector)
			}
			agg.Allocation += scale * e.Allocation
			agg.Selection += scale * e.Selection
			agg.Interaction += scale * e.Interaction
		}
	}

	sort.Strings(order)
	res := LinkedAttribution{ActiveReturn: rp - rb}
	for _, s := range order {
		e := linked[s]
		res.Sectors = append(res.Sectors, *e)
		res.TotalAllocation += e.Allocation
		res.TotalSelection += e.Selection
		res.TotalInteraction += e.Interaction
	}
	return res
}

// SingleAttribution wraps the single-period Brinson decomposition as a
// LinkedAttribution (no linking needed) — the common one-period reporting path.
func SingleAttribution(sectors []SectorData) LinkedAttribution {
	return LinkCarino([]PeriodBrinson{{
		Returns: PeriodReturn{Portfolio: weightedReturn(sectors, true), Benchmark: weightedReturn(sectors, false)},
		Effects: Brinson(sectors),
	}})
}

// weightedReturn is Σ wᵢ·Rᵢ over the portfolio (portfolio=true) or benchmark
// side — the side's total return.
func weightedReturn(sectors []SectorData, portfolio bool) float64 {
	var sum float64
	for _, s := range sectors {
		if portfolio {
			sum += s.PortfolioWeight * s.PortfolioReturn
		} else {
			sum += s.BenchmarkWeight * s.BenchmarkReturn
		}
	}
	return sum
}
