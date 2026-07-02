package frtb

import (
	"errors"
	"fmt"
)

// Supervisory-parameter calibration (PARITY-03g). The SBM engine has always
// been parameterized; this supplies a validated default calibration and the
// validation gate any loaded parameter set must pass. The defaults are
// REPRESENTATIVE of the published BCBS MAR21 magnitudes at the engine's
// granularity (per-bucket risk weight, one ρ/γ per class) — the certification
// run (PARITY-06a) reconciles the exact current publication, loaded as data at
// the composition root, against the regulator's worked examples.

// ErrParams is returned when a parameter set fails validation.
var ErrParams = errors.New("frtb: invalid supervisory parameters")

// ValidateParams gates a loaded parameter set: every class needs at least one
// positive risk weight and correlations in [0,1) — a malformed table must
// never silently price capital.
func ValidateParams(p Params) error {
	if len(p) == 0 {
		return fmt.Errorf("%w: no risk classes", ErrParams)
	}
	for class, cp := range p {
		if len(cp.RiskWeight) == 0 {
			return fmt.Errorf("%w: class %s has no risk weights", ErrParams, class)
		}
		for bucket, rw := range cp.RiskWeight {
			if rw <= 0 {
				return fmt.Errorf("%w: class %s bucket %s weight %.4g must be positive", ErrParams, class, bucket, rw)
			}
		}
		if cp.IntraCorr < 0 || cp.IntraCorr >= 1 || cp.InterCorr < 0 || cp.InterCorr >= 1 {
			return fmt.Errorf("%w: class %s correlations must be in [0,1)", ErrParams, class)
		}
	}
	return nil
}

// DefaultParams is the representative MAR21-magnitude delta calibration:
// GIRR ~1.5% rate-point weights, credit spread by quality bucket, equity by
// market-cap/economy bucket, 15% FX, commodity by group. Always passes
// ValidateParams (pinned by test).
func DefaultParams() Params {
	return Params{
		"GIRR": {
			RiskWeight: map[string]float64{
				"1": 0.017, "2": 0.017, "3": 0.016, "4": 0.013, "5": 0.012,
				"6": 0.011, "7": 0.011, "8": 0.011, "9": 0.011, "10": 0.011,
			},
			IntraCorr: 0.50, InterCorr: 0.50,
		},
		"CSR": {
			RiskWeight: map[string]float64{
				"1": 0.005, "2": 0.010, "3": 0.050, "4": 0.030, "5": 0.030,
				"6": 0.020, "7": 0.015, "8": 0.025, "9": 0.020, "10": 0.040,
				"11": 0.120, "12": 0.070,
			},
			IntraCorr: 0.35, InterCorr: 0.20,
		},
		"Equity": {
			RiskWeight: map[string]float64{
				"1": 0.55, "2": 0.60, "3": 0.45, "4": 0.55, "5": 0.30,
				"6": 0.35, "7": 0.40, "8": 0.50, "9": 0.70, "10": 0.50,
				"11": 0.70, "12": 0.15, "13": 0.25,
			},
			IntraCorr: 0.25, InterCorr: 0.15,
		},
		"FX": {
			RiskWeight: map[string]float64{"1": 0.15},
			IntraCorr:  0.60, InterCorr: 0.60,
		},
		"Commodity": {
			RiskWeight: map[string]float64{
				"1": 0.30, "2": 0.35, "3": 0.60, "4": 0.80, "5": 0.40,
				"6": 0.45, "7": 0.20, "8": 0.35, "9": 0.25, "10": 0.35, "11": 0.50,
			},
			IntraCorr: 0.55, InterCorr: 0.20,
		},
	}
}
