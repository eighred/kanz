package factormodel

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/eighred/kanz/internal/marketdata/returns"
)

// FACTOR-01c — the statistical factor model (PCA) and the model selector. Where
// the fundamental model imposes observed loadings, the statistical model lets the
// data speak: it eigendecomposes the instrument return covariance and keeps the
// top-K principal components as latent factors. It is the model-agnostic
// complement — it captures common structure a fundamental factor set misses (the
// reason the BLEND runs PCA on the fundamental model's residuals).

// ModelType selects which factor model Fit builds.
type ModelType int

const (
	// Fundamental: observed style/industry/country loadings, factor returns by
	// cross-sectional regression (FACTOR-01b).
	Fundamental ModelType = iota
	// Statistical: PCA on the return covariance (FACTOR-01c).
	Statistical
	// Blend: the fundamental factors PLUS statistical factors fit on the
	// fundamental residuals (orthogonal by construction) — the hybrid that keeps
	// interpretable factors while capturing leftover common structure.
	Blend
)

// Estimation defaults.
const (
	DefaultWindow      = 250
	DefaultStatFactors = 3
	// DefaultRidge regularizes an already identified cross-sectional design.
	// Rank deficiency is rejected before regularization.
	DefaultRidge = 1e-8
)

// Config parameterizes the factor-model estimation.
type Config struct {
	Type ModelType
	// Window is the return lookback for the regression / PCA. 0 ⇒ DefaultWindow.
	Window int
	// StyleFactors are the ordered style-characteristic names the fundamental
	// model builds z-scored loadings from.
	StyleFactors []string
	// StatFactors is the number of principal components (Statistical) or extra
	// blend factors. 0 ⇒ DefaultStatFactors.
	StatFactors int
	// Ridge regularizes the cross-sectional OLS. 0 ⇒ DefaultRidge.
	Ridge float64
	// ModelID overrides the id Fit stamps on the fitted model. Empty ⇒
	// DefaultModelID(cfg), which encodes the estimation parameters so a
	// configuration change cannot silently redefine an existing id.
	ModelID string
}

func (c Config) modelID() string {
	if c.ModelID != "" {
		return c.ModelID
	}
	return DefaultModelID(c)
}

func (c Config) window() int {
	if c.Window <= 0 {
		return DefaultWindow
	}
	return c.Window
}

func (c Config) ridge() float64 {
	if c.Ridge <= 0 {
		return DefaultRidge
	}
	return c.Ridge
}

func (c Config) statFactors() int {
	if c.StatFactors <= 0 {
		return DefaultStatFactors
	}
	return c.StatFactors
}

// Providers bundles the data seams the estimators read. Characteristics may be
// nil for a Statistical-only fit (PCA needs only returns).
type Providers struct {
	Characteristics CharacteristicProvider
	Returns         ReturnsProvider
}

// Fit builds the configured factor model over the instrument universe as of
// asOf. The universe is sorted internally for a deterministic factor model
// (same inputs ⇒ identical model, the EVT-21d replay property).
func Fit(ctx context.Context, cfg Config, instruments []string, asOf time.Time, p Providers) (*Model, error) {
	seen := map[string]bool{}
	for _, id := range instruments {
		if id == "" || seen[id] {
			return nil, fmt.Errorf("factormodel: empty or duplicate instrument %q", id)
		}
		seen[id] = true
	}
	m, err := fit(ctx, cfg, instruments, asOf, p)
	if err != nil {
		return nil, err
	}
	// STAMPED HERE, ON THE ONE PATH EVERY PRODUCTION FIT TAKES. The estimators
	// assemble matrices and know neither the configuration's identity nor the
	// as-of the caller asked for; a model that leaves this function unnamed is a
	// number nobody can reproduce, which is the defect #1039 records.
	m.ModelID, m.AsOf = cfg.modelID(), asOf
	if cfg.Type == Statistical || cfg.Type == Blend {
		m.InputProvenance["pca_solver"] = "scale-relative-jacobi-v2"
	}
	return m, nil
}

func fit(ctx context.Context, cfg Config, instruments []string, asOf time.Time, p Providers) (*Model, error) {
	if p.Returns == nil {
		return nil, fmt.Errorf("factormodel: a ReturnsProvider is required")
	}
	switch cfg.Type {
	case Statistical:
		universe := append([]string(nil), instruments...)
		sort.Strings(universe)
		panel, err := alignedReturns(ctx, p.Returns, universe, asOf, cfg.window())
		if err != nil {
			return nil, err
		}
		m, err := fitStatistical(universe, panel.Values, cfg.statFactors(), "PC")
		if err != nil {
			return nil, err
		}
		m.InputProvenance = panel.Params()
		return m, nil
	case Blend:
		if p.Characteristics == nil {
			return nil, fmt.Errorf("factormodel: Blend requires a CharacteristicProvider")
		}
		fund, regression, err := fitFundamental(ctx, cfg, instruments, asOf, p.Characteristics, p.Returns)
		if err != nil {
			return nil, err
		}
		stat, err := fitStatistical(fund.Instruments, regression.residuals, cfg.statFactors(), "SPC")
		if err != nil {
			return nil, err
		}
		m, err := blend(fund, stat, regression)
		if err != nil {
			return nil, err
		}
		m.InputProvenance = fund.InputProvenance
		m.InputProvenance["blend_covariance"] = "aligned-joint-v1"
		return m, nil
	default: // Fundamental
		if p.Characteristics == nil {
			return nil, fmt.Errorf("factormodel: Fundamental requires a CharacteristicProvider")
		}
		m, _, err := fitFundamental(ctx, cfg, instruments, asOf, p.Characteristics, p.Returns)
		return m, err
	}
}

// fitStatistical runs PCA on the return covariance and keeps the top-k principal
// components as factors. Loadings are the eigenvector components, the factor
// covariance is diagonal in the (uncorrelated) eigenvalues, and the specific
// variance is each instrument's total variance net of what the kept components
// explain. namePrefix labels the factors ("PC" / "SPC" for the blend residuals).
func fitStatistical(instruments []string, returns [][]float64, k int, namePrefix string) (*Model, error) {
	n := len(instruments)
	cov := sampleCov(returns)
	vals, vecs := eigenSym(cov)
	for _, value := range vals {
		if !finite(value) {
			return nil, fmt.Errorf("factormodel: nonfinite statistical covariance")
		}
	}

	// Order eigenpairs by descending eigenvalue; keep the top k (≤ n).
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return vals[order[a]] > vals[order[b]] })
	if k > n {
		k = n
	}

	factors := make([]Factor, k)
	loadings := make([][]float64, n)
	for i := range loadings {
		loadings[i] = make([]float64, k)
	}
	factorCov := make([][]float64, k)
	for j := 0; j < k; j++ {
		factorCov[j] = make([]float64, k)
		lambda := vals[order[j]]
		if lambda < 0 {
			lambda = 0 // round-off on a near-singular covariance
		}
		factorCov[j][j] = lambda
		factors[j] = Factor{Name: fmt.Sprintf("%s%d", namePrefix, j+1), Type: FactorStatistical}
		vec := vecs[order[j]]
		for i := 0; i < n; i++ {
			loadings[i][j] = vec[i]
		}
	}

	// Specific variance = total variance − variance explained by the kept PCs.
	specific := make(map[string]float64, n)
	for i, id := range instruments {
		explained := 0.0
		for j := 0; j < k; j++ {
			explained += loadings[i][j] * loadings[i][j] * factorCov[j][j]
		}
		s := cov[i][i] - explained
		if s < 0 {
			s = 0
		}
		specific[id] = s
	}
	return newModel(factors, instruments, loadings, factorCov, specific), nil
}

// blend estimates all factor covariances from the same dated observations.
// Cross-sectional regression does not imply zero time-series covariance
// between fundamental factor returns and residual principal components.
func blend(fund, stat *Model, regression *regressionFit) (*Model, error) {
	kf, ks := len(fund.Factors), len(stat.Factors)
	k := kf + ks
	factors := make([]Factor, 0, k)
	factors = append(factors, fund.Factors...)
	factors = append(factors, stat.Factors...)
	names := make(map[string]struct{}, k)
	for _, f := range factors {
		if _, exists := names[f.Name]; exists {
			return nil, fmt.Errorf("factormodel: duplicate blended factor name %q", f.Name)
		}
		names[f.Name] = struct{}{}
	}

	n := len(fund.Instruments)
	loadings := make([][]float64, n)
	for i := 0; i < n; i++ {
		row := make([]float64, 0, k)
		row = append(row, fund.Loadings[i]...)
		row = append(row, stat.Loadings[i]...)
		loadings[i] = row
	}

	// The return of each residual PC on date t is its loading vector
	// projected onto that date's instrument residuals.
	jointReturns := make([][]float64, k)
	copy(jointReturns, regression.factorReturns)
	for j := range ks {
		scores := make([]float64, len(regression.residuals[0]))
		for i, row := range regression.residuals {
			for t, value := range row {
				scores[t] += stat.Loadings[i][j] * value
			}
		}
		jointReturns[kf+j] = scores
	}
	cov := sampleCov(jointReturns)
	for _, row := range cov {
		for _, v := range row {
			if !finite(v) {
				return nil, fmt.Errorf("factormodel: nonfinite blend covariance")
			}
		}
	}
	for _, specific := range stat.SpecificVar {
		if !finite(specific) {
			return nil, fmt.Errorf("factormodel: nonfinite blend specific variance")
		}
	}
	return newModel(factors, fund.Instruments, loadings, cov, stat.SpecificVar), nil
}

// alignedReturns refuses incomplete universes: an absent history is not a
// zero-risk row. Every estimator uses the same canonical dated panel as VaR.
func alignedReturns(ctx context.Context, rp ReturnsProvider, instruments []string, asOf time.Time, window int) (returns.Panel, error) {
	panel, err := returns.Load(ctx, rp, instruments, asOf, window)
	if err != nil {
		return panel, err
	}
	if len(panel.Missing) != 0 {
		return panel, fmt.Errorf("factormodel: incomplete dated universe: %v", panel.Missing)
	}
	return panel, nil
}
