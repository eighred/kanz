package collateral

import (
	"errors"
	"math"
	"testing"
)

func TestDefaultSIMMCalibration_Valid(t *testing.T) {
	cal := DefaultSIMMCalibration()
	if err := ValidateSIMMCalibration(cal); err != nil {
		t.Fatalf("default calibration must validate: %v", err)
	}
	for _, class := range []string{"IR", "FX", "Equity", "Commodity", "Credit"} {
		if _, ok := cal[class]; !ok {
			t.Errorf("calibration missing risk class %s", class)
		}
	}
}

// The calibrated engine must reproduce the hand-computed single-class margin:
// two equity factors in bucket 1 (RW 24), s = {100, −50} ⇒ WS = {2400, −1200},
// K = √(2400² + 1200² + 2·0.2·2400·(−1200)).
func TestSIMM_WithDefaultCalibration(t *testing.T) {
	cal := DefaultSIMMCalibration()
	sens := []Sensitivity{
		{RiskClass: "Equity", Bucket: "1", RiskFactor: "AAPL", Amount: 100},
		{RiskClass: "Equity", Bucket: "1", RiskFactor: "XOM", Amount: -50},
	}
	got := SIMM(sens, cal["Equity"])
	want := math.Sqrt(2400*2400 + 1200*1200 + 2*0.2*2400*(-1200))
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("calibrated SIMM: got %v want %v", got, want)
	}
}

func TestValidateSIMMCalibration_Rejects(t *testing.T) {
	cases := map[string]map[string]SIMMParams{
		"empty":       {},
		"no weights":  {"IR": {IntraBucketCorr: 0.5}},
		"zero weight": {"IR": {RiskWeight: map[string]float64{"1": 0}}},
		"corr ≥ 1":    {"IR": {RiskWeight: map[string]float64{"1": 60}, IntraBucketCorr: 1.0}},
	}
	for name, cal := range cases {
		if err := ValidateSIMMCalibration(cal); !errors.Is(err, ErrSIMMParams) {
			t.Errorf("%s: want ErrSIMMParams, got %v", name, err)
		}
	}
}
