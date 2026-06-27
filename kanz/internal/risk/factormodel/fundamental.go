package factormodel

import (
	"context"
	"sort"
	"time"
)

// FACTOR-01b — the fundamental factor model. Loadings are OBSERVED from
// reference/market characteristics (style scores z-scored cross-sectionally,
// industry/country membership dummies); factor RETURNS are ESTIMATED by a
// cross-sectional OLS regression of instrument returns on those loadings at each
// date; the factor covariance is the sample covariance of the factor-return
// series and the specific variance is the per-instrument residual variance. This
// is the BARRA-style construction.

// ReturnsProvider supplies an instrument's historical return series as of a
// knowledge horizon. Declared here (not imported from compute) so the compute
// layer can depend on factormodel without an import cycle — compute's
// StoreReturnsProvider satisfies this structurally, so the engine wires one
// provider into both VaR and the factor model (one consistent history).
type ReturnsProvider interface {
	Returns(ctx context.Context, instrumentID string, asOf time.Time, window int) ([]float64, error)
}

// Characteristics is the factor-relevant reference data for one instrument — the
// raw inputs the fundamental loadings are built from.
type Characteristics struct {
	// Style maps a style-factor name to its raw cross-sectional characteristic
	// (e.g. "Size": ln(market cap), "Momentum": trailing 12-1 return). Raw values
	// are z-scored across the universe into loadings.
	Style map[string]float64
	// Industry / Country are membership labels turned into 0/1 dummy loadings.
	// "" ⇒ no membership (no dummy set for that instrument).
	Industry string
	Country  string
}

// CharacteristicProvider resolves an instrument's fundamental characteristics as
// of a point in time — the reference mirror of the ReturnsProvider.
type CharacteristicProvider interface {
	Characteristics(ctx context.Context, instrumentID string, asOf time.Time) (Characteristics, bool)
}

// buildFundamentalLoadings assembles the N×K loading matrix and the factor
// descriptors from the universe's characteristics: z-scored style columns, then
// one 0/1 dummy per distinct industry and country present (sorted for
// determinism). An instrument missing a style value contributes the cross-
// sectional mean (z-score 0); a missing industry/country sets no dummy.
func buildFundamentalLoadings(styleNames, instruments []string, chars map[string]Characteristics) ([]Factor, [][]float64) {
	n := len(instruments)

	// Style columns: collect raw, z-score.
	styleCols := make([][]float64, len(styleNames))
	for s, name := range styleNames {
		raw := make([]float64, n)
		for i, id := range instruments {
			raw[i] = chars[id].Style[name] // missing ⇒ 0, recentered by z-score
		}
		styleCols[s] = zScore(raw)
	}

	industries := distinctLabels(instruments, chars, func(c Characteristics) string { return c.Industry })
	countries := distinctLabels(instruments, chars, func(c Characteristics) string { return c.Country })

	factors := make([]Factor, 0, len(styleNames)+len(industries)+len(countries))
	for _, name := range styleNames {
		factors = append(factors, Factor{Name: name, Type: FactorStyle})
	}
	for _, ind := range industries {
		factors = append(factors, Factor{Name: "IND:" + ind, Type: FactorIndustry})
	}
	for _, ctry := range countries {
		factors = append(factors, Factor{Name: "CTY:" + ctry, Type: FactorCountry})
	}

	loadings := make([][]float64, n)
	for i, id := range instruments {
		row := make([]float64, len(factors))
		col := 0
		for s := range styleNames {
			row[col] = styleCols[s][i]
			col++
		}
		c := chars[id]
		for _, ind := range industries {
			if c.Industry == ind {
				row[col] = 1
			}
			col++
		}
		for _, ctry := range countries {
			if c.Country == ctry {
				row[col] = 1
			}
			col++
		}
		loadings[i] = row
	}
	return factors, loadings
}

func distinctLabels(instruments []string, chars map[string]Characteristics, pick func(Characteristics) string) []string {
	set := make(map[string]struct{})
	for _, id := range instruments {
		if v := pick(chars[id]); v != "" {
			set[v] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// crossSectionalFit estimates factor returns and the factor covariance from a
// loading matrix B (N×K) and aligned instrument returns (rows: N instruments ×
// L periods). At each period t it solves the ridge-regularized normal equations
// (BᵀB + λI) f_t = Bᵀ r_t (the ridge keeps collinear industry/country dummies
// invertible), then the factor covariance is sampleCov(f) and the specific
// variance is the residual variance per instrument. Returns the factor-return
// series (K×L), the factor covariance (K×K), the specific variances, and the
// residual series (N×L) the blend model factors further.
func crossSectionalFit(b [][]float64, returns [][]float64, ridge float64) (factorCov [][]float64, specific []float64, residuals [][]float64) {
	n := len(b)
	if n == 0 {
		return nil, nil, nil
	}
	k := len(b[0])
	l := len(returns[0])

	// Normal-equation matrix M = BᵀB + λI (constant across t).
	m := make([][]float64, k)
	for a := 0; a < k; a++ {
		m[a] = make([]float64, k)
		for c := 0; c < k; c++ {
			var s float64
			for i := 0; i < n; i++ {
				s += b[i][a] * b[i][c]
			}
			if a == c {
				s += ridge
			}
			m[a][c] = s
		}
	}

	factorSeries := make([][]float64, k) // factorSeries[f] = f_f over t
	for f := range factorSeries {
		factorSeries[f] = make([]float64, l)
	}
	residuals = make([][]float64, n)
	for i := range residuals {
		residuals[i] = make([]float64, l)
	}

	rhs := make([]float64, k)
	for t := 0; t < l; t++ {
		// Bᵀ r_t.
		for a := 0; a < k; a++ {
			var s float64
			for i := 0; i < n; i++ {
				s += b[i][a] * returns[i][t]
			}
			rhs[a] = s
		}
		f, ok := solveLinear(m, rhs)
		if !ok {
			f = make([]float64, k) // singular even with ridge ⇒ no factor return this period
		}
		for a := 0; a < k; a++ {
			factorSeries[a][t] = f[a]
		}
		// Residual ε_i,t = r_i,t − B_i·f_t.
		for i := 0; i < n; i++ {
			fitted := 0.0
			for a := 0; a < k; a++ {
				fitted += b[i][a] * f[a]
			}
			residuals[i][t] = returns[i][t] - fitted
		}
	}

	factorCov = sampleCov(factorSeries)
	specific = make([]float64, n)
	for i := 0; i < n; i++ {
		specific[i] = sampleVar(residuals[i])
	}
	return factorCov, specific, residuals
}

// fitFundamental builds the fundamental model over the universe, returning the
// model plus the residual return series (N×L, instrument-row order) the blend
// model runs PCA on. instruments is sorted for a deterministic universe order.
func fitFundamental(ctx context.Context, cfg Config, instruments []string, asOf time.Time, chars CharacteristicProvider, rp ReturnsProvider) (*Model, [][]float64, error) {
	universe := append([]string(nil), instruments...)
	sort.Strings(universe)

	charMap := make(map[string]Characteristics, len(universe))
	for _, id := range universe {
		if c, ok := chars.Characteristics(ctx, id, asOf); ok {
			charMap[id] = c
		}
	}
	factors, loadings := buildFundamentalLoadings(cfg.StyleFactors, universe, charMap)

	returns, err := alignedReturns(ctx, rp, universe, asOf, cfg.window())
	if err != nil {
		return nil, nil, err
	}
	factorCov, specificSlice, residuals := crossSectionalFit(loadings, returns, cfg.ridge())

	specific := make(map[string]float64, len(universe))
	for i, id := range universe {
		specific[id] = specificSlice[i]
	}
	return newModel(factors, universe, loadings, factorCov, specific), residuals, nil
}
