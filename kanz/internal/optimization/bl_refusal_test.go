package optimization

import (
	"math"
	"testing"
)

func toyBLInput() BLInput {
	return BLInput{Covariance: [][]float64{{1}}, MarketWeights: []float64{1}, RiskAversion: 1, Tau: 1,
		P: [][]float64{{1}}, Q: []float64{3}}
}

func TestBlackLittermanIndependentToyControls(t *testing.T) {
	// Prior=1, posterior=1+(3-1)/(1+Omega): 2 at Omega=1,
	// 3/2 at Omega=3. Zero/nil select the documented default Omega=1.
	for _, tc := range []struct {
		name   string
		mutate func(*BLInput)
		want   float64
	}{
		{"nil_default", func(*BLInput) {}, 2},
		{"zero_default", func(in *BLInput) { in.Omega = []float64{0} }, 2},
		{"unit_variance", func(in *BLInput) { in.Omega = []float64{1} }, 2},
		{"variance_three", func(in *BLInput) { in.Omega = []float64{3} }, 1.5},
		{"no_views", func(in *BLInput) { in.P, in.Q = nil, nil }, 1},
		{"signed_view", func(in *BLInput) { in.Q[0] = -3 }, -1},
		{"signed_picking", func(in *BLInput) { in.P[0][0] = -1 }, -1},
		{"singular_covariance_prior", func(in *BLInput) { in.Covariance[0][0] = 0; in.P, in.Q = nil, nil }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := toyBLInput()
			tc.mutate(&in)
			mu, err := BlackLitterman(in)
			if err != nil || len(mu) != 1 || math.IsNaN(mu[0]) || math.IsInf(mu[0], 0) || math.Abs(mu[0]-tc.want) > 1e-12 {
				t.Fatalf("posterior=%v err=%v, want finite %v", mu, err, tc.want)
			}
		})
	}
}

func TestBlackLittermanRefusesNegativeVariance(t *testing.T) {
	in := toyBLInput()
	in.Omega = []float64{-1}
	if mu, err := BlackLitterman(in); err == nil || mu != nil {
		t.Fatalf("negative variance selected a default: posterior=%v err=%v", mu, err)
	}
}

func TestBlackLittermanRefusesNonfiniteInputs(t *testing.T) {
	for _, v := range []struct {
		name string
		v    float64
	}{{"nan", math.NaN()}, {"positive_infinity", math.Inf(1)}, {"negative_infinity", math.Inf(-1)}} {
		for _, tc := range []struct {
			name   string
			mutate func(*BLInput)
		}{
			{"covariance", func(in *BLInput) { in.Covariance[0][0] = v.v }},
			{"market_weight", func(in *BLInput) { in.MarketWeights[0] = v.v }},
			{"risk_aversion", func(in *BLInput) { in.RiskAversion = v.v }},
			{"tau", func(in *BLInput) { in.Tau = v.v }},
			{"picking", func(in *BLInput) { in.P[0][0] = v.v }},
			{"view", func(in *BLInput) { in.Q[0] = v.v }},
			{"variance", func(in *BLInput) { in.Omega = []float64{v.v} }},
			{"no_view_weight", func(in *BLInput) { in.P, in.Q = nil, nil; in.MarketWeights[0] = v.v }},
			{"no_view_tau", func(in *BLInput) { in.P, in.Q = nil, nil; in.Tau = v.v }},
		} {
			t.Run(v.name+"/"+tc.name, func(t *testing.T) {
				in := toyBLInput()
				tc.mutate(&in)
				if mu, err := BlackLitterman(in); err == nil || mu != nil {
					t.Fatalf("nonfinite input returned a posterior: %v, %v", mu, err)
				}
			})
		}
	}
}

func TestBlackLittermanNoViewsStillValidatesViewDimensions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*BLInput)
	}{
		{"orphan_view", func(in *BLInput) { in.Q = []float64{3} }},
		{"orphan_variance", func(in *BLInput) { in.Omega = []float64{1} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := toyBLInput()
			in.P, in.Q = nil, nil
			tc.mutate(&in)
			if mu, err := BlackLitterman(in); err != ErrBLDims || mu != nil {
				t.Fatalf("unused view input was ignored: posterior=%v err=%v", mu, err)
			}
		})
	}
}

func TestBlackLittermanRefusesNonfiniteArithmetic(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*BLInput)
	}{
		{"prior_product", func(in *BLInput) { in.P, in.Q = nil, nil; in.Covariance[0][0], in.MarketWeights[0] = 1e308, 2 }},
		{"prior_scale", func(in *BLInput) { in.P, in.Q = nil, nil; in.Covariance[0][0], in.RiskAversion = 1e308, 2 }},
		{"tau_scale", func(in *BLInput) { in.Covariance[0][0], in.Tau, in.MarketWeights[0] = 1e308, 2, 0 }},
		{"weighted_picking", func(in *BLInput) { in.Covariance[0][0], in.P[0][0], in.MarketWeights[0] = 2, 1e308, 0 }},
		{"view_covariance", func(in *BLInput) { in.P[0][0], in.MarketWeights[0] = 1e200, 0 }},
		{"view_diagonal", func(in *BLInput) { in.P[0][0], in.MarketWeights[0] = 1e154, 0 }},
		{"projected_prior", func(in *BLInput) { in.RiskAversion, in.P[0][0] = 1e308, 2 }},
		{"view_difference", func(in *BLInput) { in.RiskAversion, in.MarketWeights[0], in.Q[0] = 1e308, -1, 1e308 }},
		{"linear_solution", func(in *BLInput) { in.Covariance[0][0], in.MarketWeights[0], in.Q[0] = 1e-14, 0, 1e308 }},
		{"posterior_adjustment", func(in *BLInput) {
			in.Covariance[0][0], in.MarketWeights[0], in.P[0][0], in.Q[0] = 1e308, 0, 1e-154, 1e308
		}},
		{"posterior_sum", func(in *BLInput) { in.Covariance[0][0], in.P[0][0], in.Q[0] = 1e308, 1e-154, 3e154 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := toyBLInput()
			tc.mutate(&in)
			if mu, err := BlackLitterman(in); err == nil || mu != nil {
				t.Fatalf("nonfinite arithmetic returned a posterior: %v, %v", mu, err)
			}
		})
	}
}
