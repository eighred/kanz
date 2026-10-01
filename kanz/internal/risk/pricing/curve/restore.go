package curve

import (
	"errors"
	"math"

	"github.com/eighred/kanz/internal/dec"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"
)

// FromArtifact reconstructs a retained calibrated curve with its original
// interpolation and coverage. Unsupported conventions are refused, never guessed.
func FromArtifact(a *domainpb.CalibratedCurve) (*Curve, error) {
	bad := errors.New("curve: invalid retained calibration artifact")
	if a == nil || a.Curve == nil {
		return nil, bad
	}
	c := a.Curve
	if c.CurrencyCode == "" || c.CurveId != c.CurrencyCode || c.AsOf == nil || c.AsOf.CheckValid() != nil || c.AsOf.AsTime().IsZero() || c.CurveType != referencepb.CurveType_CURVE_TYPE_ZERO || c.Compounding != referencepb.Compounding_COMPOUNDING_CONTINUOUS || len(c.Points) == 0 {
		return nil, bad
	}
	var interpolation Interpolation
	switch c.Interpolation {
	case referencepb.Interpolation_INTERPOLATION_LINEAR_ZERO:
		interpolation = LinearZero
	case referencepb.Interpolation_INTERPOLATION_LOG_LINEAR_DF:
		interpolation = LogLinearDF
	default:
		return nil, bad
	}
	tenors, rates := make([]float64, len(c.Points)), make([]float64, len(c.Points))
	for i, p := range c.Points {
		if p == nil || math.IsNaN(p.TenorYears) || math.IsInf(p.TenorYears, 0) || p.TenorYears <= 0 || (i > 0 && p.TenorYears <= tenors[i-1]) {
			return nil, bad
		}
		r, ok := dec.Float64(p.Rate)
		if !ok {
			return nil, bad
		}
		tenors[i], rates[i] = p.TenorYears, r
	}
	out, err := NewZeroCurve(tenors, rates, Continuous, interpolation)
	if err != nil {
		return nil, err
	}
	if a.StripCoverage != nil {
		c := a.StripCoverage
		if c.Configured == 0 || c.Quoted > c.Configured || uint64(c.Quoted)+uint64(len(c.Missing)) != uint64(c.Configured) {
			return nil, bad
		}
		coverage := StripCoverage{Configured: int(c.Configured), Quoted: int(c.Quoted)}
		seen := make(map[string]bool, len(c.Missing))
		for _, m := range c.Missing {
			if m == nil || m.InstrumentId == "" || m.Reason == "" || seen[m.InstrumentId] {
				return nil, bad
			}
			seen[m.InstrumentId] = true
			coverage.Missing = append(coverage.Missing, MissingQuote{InstrumentID: m.InstrumentId, Reason: m.Reason})
		}
		out = out.withStripCoverage(coverage)
	}
	return out, nil
}
