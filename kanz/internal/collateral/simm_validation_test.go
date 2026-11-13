package collateral

import (
	"errors"
	"math"
	"testing"
)

func toySIMMParams() SIMMParams {
	return SIMMParams{RiskWeight: map[string]float64{"A": 2}, IntraBucketCorr: 0.5, InterBucketCorr: 0.25}
}

// These are arithmetic controls for the existing aggregation, not licensed
// SIMM calibration fixtures or evidence of production IM approval.
func TestSIMMIndependentToyControls(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []Sensitivity
		want float64
	}{
		{"ten_times_two", []Sensitivity{{Bucket: "A", RiskFactor: "X", Amount: 10}}, 20},
		{"negative_sensitivity", []Sensitivity{{Bucket: "A", RiskFactor: "X", Amount: -10}}, 20},
		{"reported_zero", []Sensitivity{{Bucket: "A", RiskFactor: "X", Amount: 0}}, 0},
		{"netted_zero", []Sensitivity{{Bucket: "A", RiskFactor: "X", Amount: 10}, {Bucket: "A", RiskFactor: "X", Amount: -10}}, 0},
		{"empty_sensitivities", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustSIMM(t, tc.rows, toySIMMParams()); math.Abs(got-tc.want) > 1e-12 {
				t.Fatalf("margin = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSIMMRefusesMissingBuckets(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []Sensitivity
	}{
		{"only_unknown", []Sensitivity{{Bucket: "missing", Amount: 10}}},
		{"partial", []Sensitivity{{Bucket: "A", Amount: 10}, {Bucket: "missing", Amount: 10}}},
		{"unknown_zero", []Sensitivity{{Bucket: "missing", Amount: 0}}},
		{"unknown_netted_zero", []Sensitivity{{Bucket: "missing", Amount: 10}, {Bucket: "missing", Amount: -10}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SIMM(tc.rows, toySIMMParams())
			if !errors.Is(err, ErrSIMMInput) || got != 0 {
				t.Fatalf("unknown bucket returned a measurement: margin=%v err=%v", got, err)
			}
		})
	}
}

func TestSIMMRefusesNonfiniteSensitivities(t *testing.T) {
	for _, v := range []struct {
		name string
		v    float64
	}{{"nan", math.NaN()}, {"positive_infinity", math.Inf(1)}, {"negative_infinity", math.Inf(-1)}} {
		t.Run(v.name, func(t *testing.T) {
			got, err := SIMM([]Sensitivity{{Bucket: "A", Amount: 10}, {Bucket: "A", Amount: v.v}}, toySIMMParams())
			if !errors.Is(err, ErrSIMMInput) || got != 0 {
				t.Fatalf("nonfinite sensitivity returned a measurement: margin=%v err=%v", got, err)
			}
		})
	}
}

func TestSIMMRefusesNonfiniteIntermediates(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []Sensitivity
		p    SIMMParams
	}{
		{"factor_sum", []Sensitivity{{Bucket: "A", Amount: math.MaxFloat64}, {Bucket: "A", Amount: math.MaxFloat64}}, toySIMMParams()},
		{"weighted_sensitivity", []Sensitivity{{Bucket: "A", Amount: math.MaxFloat64}}, toySIMMParams()},
		{"bucket_variance", []Sensitivity{{Bucket: "A", Amount: 1e200}}, toySIMMParams()},
		{"bucket_sum_square", []Sensitivity{{Bucket: "A", RiskFactor: "X", Amount: 6e153}, {Bucket: "A", RiskFactor: "Y", Amount: 6e153}, {Bucket: "A", RiskFactor: "Z", Amount: 6e153}}, SIMMParams{RiskWeight: map[string]float64{"A": 1}}},
		{"across_bucket_variance", []Sensitivity{{Bucket: "A", Amount: 1e154}, {Bucket: "B", Amount: 1e154}}, SIMMParams{RiskWeight: map[string]float64{"A": 1, "B": 1}}},
		{"across_bucket_cross_term", []Sensitivity{{Bucket: "A", Amount: 7e153}, {Bucket: "B", Amount: 7e153}}, SIMMParams{RiskWeight: map[string]float64{"A": 1, "B": 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SIMM(tc.rows, tc.p)
			if !errors.Is(err, ErrSIMMNumerical) || got != 0 {
				t.Fatalf("nonfinite arithmetic returned a measurement: margin=%v err=%v", got, err)
			}
		})
	}
}

func TestSIMMCalibrationRefusesInvalidParametersBeforeMeasurement(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*SIMMParams)
	}{
		{"missing_weights", func(p *SIMMParams) { p.RiskWeight = nil }},
		{"zero_weight", func(p *SIMMParams) { p.RiskWeight["A"] = 0 }},
		{"negative_weight", func(p *SIMMParams) { p.RiskWeight["A"] = -1 }},
		{"negative_intra_correlation", func(p *SIMMParams) { p.IntraBucketCorr = -0.1 }},
		{"unit_intra_correlation", func(p *SIMMParams) { p.IntraBucketCorr = 1 }},
		{"negative_inter_correlation", func(p *SIMMParams) { p.InterBucketCorr = -0.1 }},
		{"unit_inter_correlation", func(p *SIMMParams) { p.InterBucketCorr = 1 }},
	}
	for _, v := range []struct {
		name string
		v    float64
	}{{"nan", math.NaN()}, {"positive_infinity", math.Inf(1)}, {"negative_infinity", math.Inf(-1)}} {
		cases = append(cases,
			struct {
				name   string
				mutate func(*SIMMParams)
			}{v.name + "_weight", func(p *SIMMParams) { p.RiskWeight["A"] = v.v }},
			struct {
				name   string
				mutate func(*SIMMParams)
			}{v.name + "_unused_weight", func(p *SIMMParams) { p.RiskWeight["unused"] = v.v }},
			struct {
				name   string
				mutate func(*SIMMParams)
			}{v.name + "_intra_correlation", func(p *SIMMParams) { p.IntraBucketCorr = v.v }},
			struct {
				name   string
				mutate func(*SIMMParams)
			}{v.name + "_inter_correlation", func(p *SIMMParams) { p.InterBucketCorr = v.v }},
		)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := toySIMMParams()
			tc.mutate(&p)
			if err := ValidateSIMMCalibration(map[string]SIMMParams{"toy": p}); !errors.Is(err, ErrSIMMParams) {
				t.Fatalf("invalid calibration accepted: %v", err)
			}
			for _, rows := range [][]Sensitivity{nil, {{Bucket: "A", Amount: 10}}} {
				got, err := SIMM(rows, p)
				if !errors.Is(err, ErrSIMMParams) || got != 0 {
					t.Fatalf("invalid calibration returned a measurement: margin=%v err=%v", got, err)
				}
			}
		})
	}
}
