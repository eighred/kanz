package factormodel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
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
	// AsOf is the requested PIT cut, not an invented observation timestamp.
	AsOf time.Time
	// SourceDigest identifies the input artifact used to derive these values.
	SourceDigest string
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
// determinism). Callers must first exclude incomplete descriptor rows.
func buildFundamentalLoadings(styleNames, instruments []string, chars map[string]Characteristics) ([]Factor, [][]float64, error) {
	n := len(instruments)

	// Style columns: collect raw, z-score.
	styleCols := make([][]float64, len(styleNames))
	for s, name := range styleNames {
		raw := make([]float64, n)
		for i, id := range instruments {
			v, ok := chars[id].Style[name]
			if !ok || !finiteDescriptor(v) {
				return nil, nil, fmt.Errorf("factormodel: missing/non-finite %s for %s", name, id)
			}
			raw[i] = v
		}
		var err error
		styleCols[s], err = descriptorScores(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("style %s: %w", name, err)
		}
	}

	industries := distinctLabels(instruments, chars, func(c Characteristics) string { return c.Industry })
	countries := distinctLabels(instruments, chars, func(c Characteristics) string { return c.Country })
	// Full industry indicators already span the intercept. Omit a canonical
	// country baseline so the two complete partitions do not duplicate it.
	if len(industries) > 0 && len(countries) > 0 {
		countries = countries[1:]
	}

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

	names := map[string]bool{}
	for _, factor := range factors {
		if names[factor.Name] {
			return nil, nil, fmt.Errorf("factormodel: duplicate factor name %s", factor.Name)
		}
		names[factor.Name] = true
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
	return factors, loadings, nil
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

// crossSectionalFit refuses unidentified designs before ridge regularization.
// QR of [B; sqrt(ridge) I] avoids squaring the design condition number.
func crossSectionalFit(b [][]float64, values [][]float64, ridge float64) ([][]float64, []float64, [][]float64, error) {
	fail := func() ([][]float64, []float64, [][]float64, error) {
		return nil, nil, nil, fmt.Errorf("factormodel: invalid or unidentified cross-sectional fit")
	}
	n := len(b)
	if n < 2 || len(b[0]) == 0 || len(values) != n || len(values[0]) < 2 || !finiteDescriptor(ridge) || ridge < 0 {
		return fail()
	}
	k, l := len(b[0]), len(values[0])
	if n <= k {
		return fail()
	}
	for i := range b {
		if len(b[i]) != k || len(values[i]) != l {
			return fail()
		}
		for _, v := range b[i] {
			if !finiteDescriptor(v) {
				return fail()
			}
		}
		for _, v := range values[i] {
			if !finiteDescriptor(v) {
				return fail()
			}
		}
	}
	if _, _, err := designQR(b); err != nil {
		return fail()
	}
	augmented := make([][]float64, n+k)
	copy(augmented, b)
	for j := 0; j < k; j++ {
		augmented[n+j] = make([]float64, k)
		augmented[n+j][j] = math.Sqrt(ridge)
	}
	q, r, err := designQR(augmented)
	if err != nil {
		return fail()
	}
	series := make([][]float64, k)
	for j := range series {
		series[j] = make([]float64, l)
	}
	residuals := make([][]float64, n)
	for i := range residuals {
		residuals[i] = make([]float64, l)
	}
	for t := 0; t < l; t++ {
		f := make([]float64, k)
		for j := k - 1; j >= 0; j-- {
			v := 0.0
			for i := 0; i < n; i++ {
				v += q[j][i] * values[i][t]
			}
			for h := j + 1; h < k; h++ {
				v -= r[j][h] * f[h]
			}
			f[j] = v / r[j][j]
			if !finiteDescriptor(f[j]) {
				return fail()
			}
			series[j][t] = f[j]
		}
		for i := 0; i < n; i++ {
			v := values[i][t]
			for j := 0; j < k; j++ {
				v -= b[i][j] * f[j]
			}
			if !finiteDescriptor(v) {
				return fail()
			}
			residuals[i][t] = v
		}
	}
	cov := sampleCov(series)
	specific := make([]float64, n)
	for _, row := range cov {
		for _, v := range row {
			if !finiteDescriptor(v) {
				return fail()
			}
		}
	}
	for i := range specific {
		specific[i] = sampleVar(residuals[i])
		if !finiteDescriptor(specific[i]) {
			return fail()
		}
	}
	return cov, specific, residuals, nil
}

// fitFundamental builds the fundamental model over the universe, returning the
// model plus the residual return series (N×L, instrument-row order) the blend
// model runs PCA on. instruments is sorted for a deterministic universe order.
func fitFundamental(ctx context.Context, cfg Config, instruments []string, asOf time.Time, chars CharacteristicProvider, rp ReturnsProvider) (*Model, [][]float64, error) {
	if asOf.IsZero() || !finiteDescriptor(cfg.Ridge) || cfg.Ridge < 0 {
		return nil, nil, fmt.Errorf("factormodel: invalid horizon or ridge")
	}
	seen := map[string]bool{}
	for _, name := range cfg.StyleFactors {
		if name == "" || seen[name] {
			return nil, nil, fmt.Errorf("factormodel: empty/duplicate style factor")
		}
		seen[name] = true
	}
	universe := append([]string(nil), instruments...)
	sort.Strings(universe)
	charMap := make(map[string]Characteristics, len(universe))
	excluded := map[string]string{}
	for _, id := range universe {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		c, ok := chars.Characteristics(ctx, id, asOf)
		reason := ""
		switch {
		case !ok:
			reason = "missing_characteristics"
		case !c.AsOf.Equal(asOf) || c.SourceDigest == "":
			reason = "unproven_characteristics"
		default:
			for _, name := range cfg.StyleFactors {
				v, present := c.Style[name]
				if !present {
					reason = "missing_style:" + name
					break
				}
				if !finiteDescriptor(v) {
					reason = "nonfinite_style:" + name
					break
				}
			}
		}
		if reason != "" {
			excluded[id] = reason
			continue
		}
		// Snapshot only configured, finite fields; unused vendor descriptors neither
		// contaminate normalization nor introduce NaNs into artifact serialization.
		c.Style = copyStyles(c.Style, cfg.StyleFactors)
		c.AsOf = c.AsOf.UTC()
		charMap[id] = c
	}
	industry, country := false, false
	for _, c := range charMap {
		industry = industry || c.Industry != ""
		country = country || c.Country != ""
	}
	eligible := make([]string, 0, len(universe))
	for _, id := range universe {
		c, ok := charMap[id]
		if !ok {
			continue
		}
		if (industry && c.Industry == "") || (country && c.Country == "") {
			excluded[id] = "missing_classification"
			delete(charMap, id)
			continue
		}
		eligible = append(eligible, id)
	}
	universe = eligible
	if len(universe) < 2 {
		return nil, nil, fmt.Errorf("factormodel: insufficient descriptor coverage: %v", excluded)
	}

	factors, loadings, err := buildFundamentalLoadings(cfg.StyleFactors, universe, charMap)
	if err != nil {
		return nil, nil, err
	}

	panel, err := alignedReturns(ctx, rp, universe, asOf, cfg.window())
	if err != nil {
		return nil, nil, err
	}
	factorCov, specificSlice, residuals, err := crossSectionalFit(loadings, panel.Values, cfg.ridge())
	if err != nil {
		return nil, nil, err
	}

	specific := make(map[string]float64, len(universe))
	for i, id := range universe {
		specific[id] = specificSlice[i]
	}
	m := newModel(factors, universe, loadings, factorCov, specific)
	m.InputProvenance = panel.Params()
	descriptor, err := json.Marshal(struct {
		Policy   string
		Config   Config
		Values   map[string]Characteristics
		Excluded map[string]string
	}{DescriptorPolicy, cfg, charMap, excluded})
	if err != nil {
		return nil, nil, err
	}
	digest := sha256.Sum256(descriptor)
	exclusions, _ := json.Marshal(excluded)
	m.InputProvenance["descriptor_policy"] = DescriptorPolicy
	m.InputProvenance["descriptor_digest"] = hex.EncodeToString(digest[:])
	m.InputProvenance["descriptor_exclusions"] = string(exclusions)
	m.InputProvenance["descriptor_eligible"] = strconv.Itoa(len(universe))
	if industry && country {
		m.InputProvenance["country_reference"] = distinctLabels(universe, charMap, func(c Characteristics) string { return c.Country })[0]
	}
	combined := sha256.Sum256([]byte(panel.Digest + ":" + hex.EncodeToString(digest[:])))
	m.InputProvenance["input_digest"] = hex.EncodeToString(combined[:])
	return m, residuals, nil
}
