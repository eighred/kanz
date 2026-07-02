package collateral

import (
	"errors"
	"fmt"
)

// SIMM supervisory calibration (PARITY-03g). The COLL-01b engine has always
// been parameterized "by the ISDA risk weights and correlations"; this
// supplies a validated default calibration per risk class and the validation
// gate any loaded set must pass. The defaults are REPRESENTATIVE of the
// published ISDA SIMM magnitudes at the engine's granularity (per-bucket risk
// weight, one ρ/γ per class); the licensed current ISDA parameter file loads
// as data at the composition root and the certification run reconciles it
// against the ISDA unit tests (PARITY-06a) — parameters are DATA, never code.

// ErrSIMMParams is returned when a calibration fails validation.
var ErrSIMMParams = errors.New("collateral: invalid SIMM calibration")

// ValidateSIMMCalibration gates a loaded calibration: every class needs
// positive risk weights and correlations in [0,1).
func ValidateSIMMCalibration(cal map[string]SIMMParams) error {
	if len(cal) == 0 {
		return fmt.Errorf("%w: no risk classes", ErrSIMMParams)
	}
	for class, p := range cal {
		if len(p.RiskWeight) == 0 {
			return fmt.Errorf("%w: class %s has no risk weights", ErrSIMMParams, class)
		}
		for bucket, rw := range p.RiskWeight {
			if rw <= 0 {
				return fmt.Errorf("%w: class %s bucket %s weight %.4g must be positive", ErrSIMMParams, class, bucket, rw)
			}
		}
		if p.IntraBucketCorr < 0 || p.IntraBucketCorr >= 1 || p.InterBucketCorr < 0 || p.InterBucketCorr >= 1 {
			return fmt.Errorf("%w: class %s correlations must be in [0,1)", ErrSIMMParams, class)
		}
	}
	return nil
}

// DefaultSIMMCalibration is the representative ISDA-SIMM-magnitude delta
// calibration for the five risk classes the margin engine sums over. Always
// passes ValidateSIMMCalibration (pinned by test).
func DefaultSIMMCalibration() map[string]SIMMParams {
	return map[string]SIMMParams{
		"IR": {
			RiskWeight: map[string]float64{
				// Regular-volatility currency tenor buckets (per-unit rate
				// sensitivities, bp-denominated CRIF scaled upstream).
				"2w": 109, "1m": 105, "3m": 90, "6m": 71, "1y": 66,
				"2y": 66, "3y": 64, "5y": 60, "10y": 60, "15y": 61,
				"20y": 61, "30y": 67,
			},
			IntraBucketCorr: 0.73, InterBucketCorr: 0.27,
		},
		"FX": {
			RiskWeight:      map[string]float64{"1": 7.4},
			IntraBucketCorr: 0.50, InterBucketCorr: 0.50,
		},
		"Equity": {
			RiskWeight: map[string]float64{
				"1": 24, "2": 30, "3": 31, "4": 25, "5": 21, "6": 22,
				"7": 27, "8": 24, "9": 33, "10": 34, "11": 17, "12": 17,
			},
			IntraBucketCorr: 0.20, InterBucketCorr: 0.16,
		},
		"Commodity": {
			RiskWeight: map[string]float64{
				"1": 48, "2": 29, "3": 33, "4": 25, "5": 35, "6": 30,
				"7": 60, "8": 52, "9": 68, "10": 63, "11": 21, "12": 21,
				"13": 15, "14": 16, "15": 13, "16": 68, "17": 17,
			},
			IntraBucketCorr: 0.42, InterBucketCorr: 0.20,
		},
		"Credit": {
			RiskWeight: map[string]float64{
				"1": 75, "2": 90, "3": 84, "4": 54, "5": 62, "6": 48,
				"7": 185, "8": 343, "9": 255, "10": 250, "11": 214, "12": 173,
			},
			IntraBucketCorr: 0.35, InterBucketCorr: 0.31,
		},
	}
}
