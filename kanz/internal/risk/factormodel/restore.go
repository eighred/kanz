package factormodel

import (
	"errors"
	"maps"
	"math"

	factorpb "github.com/eighred/kanz/kanz-schemas-go/factor/v1"
	"google.golang.org/protobuf/proto"
)

// FromSnapshot reconstructs the fitted instance, including its lookup index.
// No estimation or current market data is consulted. Malformed matrices must
// refuse reconstruction rather than become zero factor or specific risk.
func FromSnapshot(s *factorpb.FactorModelSnapshot) (*Model, error) {
	bad := errors.New("factor: invalid retained model snapshot")
	if s == nil || s.Model == nil || s.Model.ModelId == "" || s.Model.AsOf == nil || s.Model.AsOf.CheckValid() != nil || s.Model.AsOf.AsTime().IsZero() {
		return nil, bad
	}
	k, n := len(s.Model.Factors), len(s.Exposures)
	c := s.Covariance
	// Bound reconstruction work at the network/storage boundary. The live
	// model uses three factors; 256 admits institutional factor catalogs while
	// keeping covariance validation bounded independently of the universe.
	if k == 0 || k > 256 || n == 0 || c == nil || int(c.Dimension) != k || len(c.Values)/k != k || len(c.Values)%k != 0 || c.ModelId != s.Model.ModelId || !proto.Equal(c.AsOf, s.Model.AsOf) {
		return nil, bad
	}
	factors := make([]Factor, k)
	names := make(map[string]bool, k)
	for i, f := range s.Model.Factors {
		if f == nil || f.Name == "" || names[f.Name] || f.Type < factorpb.FactorType_FACTOR_TYPE_STYLE || f.Type > factorpb.FactorType_FACTOR_TYPE_STATISTICAL {
			return nil, bad
		}
		names[f.Name] = true
		factors[i] = Factor{Name: f.Name, Type: FactorType(f.Type - factorpb.FactorType_FACTOR_TYPE_STYLE)}
	}
	ids := make([]string, n)
	loadings := make([][]float64, n)
	specific := make(map[string]float64, n)
	for i, e := range s.Exposures {
		if e == nil || e.InstrumentId == "" || e.ModelId != s.Model.ModelId || !proto.Equal(e.AsOf, s.Model.AsOf) || len(e.Loadings) != k || !finite(e.SpecificVariance) || e.SpecificVariance < 0 {
			return nil, bad
		}
		if _, duplicate := specific[e.InstrumentId]; duplicate {
			return nil, bad
		}
		for _, v := range e.Loadings {
			if !finite(v) {
				return nil, bad
			}
		}
		ids[i], loadings[i], specific[e.InstrumentId] = e.InstrumentId, append([]float64(nil), e.Loadings...), e.SpecificVariance
	}
	cov := make([][]float64, k)
	for i := range k {
		cov[i] = append([]float64(nil), c.Values[i*k:(i+1)*k]...)
		for j, v := range cov[i] {
			if !finite(v) || v != c.Values[j*k+i] || (i == j && v < 0) {
				return nil, bad
			}
		}
	}
	if !retainedCovarianceValid(cov) {
		return nil, bad
	}
	m := newModel(factors, ids, loadings, cov, specific)
	m.ModelID, m.AsOf, m.InputProvenance = s.Model.ModelId, s.Model.AsOf.AsTime(), maps.Clone(s.InputProvenance)
	return m, nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func retainedCovarianceValid(cov [][]float64) bool {
	var scale float64
	for _, row := range cov {
		for _, v := range row {
			scale = math.Max(scale, math.Abs(v))
		}
	}
	if scale == 0 {
		return true
	}
	normalized := make([][]float64, len(cov))
	for i, row := range cov {
		normalized[i] = make([]float64, len(row))
		for j, v := range row {
			normalized[i][j] = v / scale
		}
	}
	values, _ := eigenSym(normalized)
	for _, v := range values {
		// Permit only rounding noise relative to the matrix scale. A genuinely
		// indefinite covariance would be clamped to zero risk by Risk().
		if !finite(v) || v < -1e-12 {
			return false
		}
	}
	return true
}
