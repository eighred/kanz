package regulatory

import (
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/regulatory/frtb"
)

// sampleInputs is a small but non-degenerate trading book: an equity delta, an
// equity vega, a curvature pair, one JTD obligor, and one exotic RRAO notional.
func sampleInputs() FRTBInputs {
	return FRTBInputs{
		Delta:     []frtb.Sensitivity{{RiskClass: "Equity", Bucket: "1", Factor: "AAPL", Amount: 1_000_000}},
		Vega:      []frtb.Sensitivity{{RiskClass: "Equity", Bucket: "1", Factor: "AAPL_VOL", Amount: 200_000}},
		Curvature: []frtb.CurvatureSensitivity{{RiskClass: "Equity", Bucket: "1", Factor: "AAPL", Up: -50_000, Down: 40_000}},
		JTD:       []frtb.JTDPosition{{Obligor: "ACME", Bucket: "IG", Rating: "BBB", Amount: 2_000_000}},
		RRAO:      []frtb.RRAOPosition{{Notional: 5_000_000, Exotic: true}},
		Params:    frtb.DefaultParams(),
		DRCParams: frtb.DefaultDRCParams(),
	}
}

// The filing is a faithful assembler: its component breakdown reproduces each
// frtb engine called directly, and the total is their sum.
func TestComputeFRTB_AssemblesEveryCharge(t *testing.T) {
	in := sampleInputs()
	res, err := ComputeFRTB(in)
	if err != nil {
		t.Fatal(err)
	}
	wantDelta := mustFRTBCharge(t, in.Delta, in.Params)
	wantVega := mustFRTBCharge(t, in.Vega, in.Params)
	wantCurv := mustCurvatureCharge(t, in.Curvature, in.Params)
	wantDRC := frtb.DRC(in.JTD, in.DRCParams)
	wantRRAO := frtb.RRAO(in.RRAO)
	if res.Delta != wantDelta || res.Vega != wantVega || res.Curvature != wantCurv ||
		res.DRC != wantDRC || res.RRAO != wantRRAO {
		t.Fatalf("component mismatch: %+v vs direct (%v %v %v %v %v)",
			res, wantDelta, wantVega, wantCurv, wantDRC, wantRRAO)
	}
	wantTotal := wantDelta + wantVega + wantCurv + wantDRC + wantRRAO
	if res.Total != wantTotal {
		t.Fatalf("total = %v want %v (sum of charges)", res.Total, wantTotal)
	}
	// Every charge is a non-negative capital number, and RRAO is 1% of the exotic
	// notional (MAR23.8) — a concrete worked figure the filing must reproduce.
	if res.RRAO != 50000 {
		t.Fatalf("RRAO = %v want 50000 (1%% of 5m exotic)", res.RRAO)
	}
}

// FileFRTB produces a complete, signed filing whose total line item equals the
// computed total, and Reconcile passes against that total and fails off it.
func TestFileFRTB_SignedCompleteAndReconciles(t *testing.T) {
	in := sampleInputs()
	asOf := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	rep, res, err := FileFRTB(in, asOf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Signature == "" {
		t.Fatal("filing must be signed")
	}
	if len(rep.LineItems) != 6 {
		t.Fatalf("filing should carry all 6 FRTB line items, got %d", len(rep.LineItems))
	}
	filed, ok := rep.Lookup("FRTB_TOTAL")
	// The filed value must be the model's total EXACTLY — the conversion at the
	// filing boundary captures the double's true value, it does not round it.
	if !ok || filed.Cmp(new(big.Rat).SetFloat64(res.Total)) != 0 {
		t.Fatalf("filed total %v (ok=%v) != computed %v", filed, ok, res.Total)
	}
	// Reconciliation against the (independent) expected total passes within tol.
	if delta, ok := rep.Reconcile(new(big.Rat).SetFloat64(res.Total), big.NewRat(1, 1)); !ok || delta.Sign() != 0 {
		t.Fatalf("reconcile against exact total failed: delta=%v ok=%v", delta, ok)
	}
	// A total that disagrees beyond tolerance fails the reconciliation gate.
	if _, ok := rep.Reconcile(new(big.Rat).SetFloat64(res.Total+1000), big.NewRat(1, 1)); ok {
		t.Fatal("reconcile should fail when the expected total is off by 1000 > tol")
	}
}

// Invalid supervisory params never yield a filing — the calibration gate fires
// before any capital number is produced.
func TestComputeFRTB_InvalidParamsRejected(t *testing.T) {
	in := sampleInputs()
	in.Params = frtb.Params{"Equity": {RiskWeight: map[string]float64{"1": -1}, IntraCorr: 2, InterCorr: 0}}
	if _, err := ComputeFRTB(in); err == nil {
		t.Fatal("expected invalid params to be rejected")
	}
}

// mustFRTBCharge / mustCurvatureCharge fatal on a refusal (#565). These fixtures
// build their own params beside their own sensitivities, so an uncovered class
// means the fixture is wrong — and a discarded error here would let a test keep
// asserting a total that ComputeFRTB now declines to produce.
func mustFRTBCharge(t *testing.T, sens []frtb.Sensitivity, p frtb.Params) float64 {
	t.Helper()
	v, err := frtb.Charge(sens, p)
	if err != nil {
		t.Fatalf("frtb.Charge refused a fixture whose table should cover it: %v", err)
	}
	return v
}

func mustCurvatureCharge(t *testing.T, sens []frtb.CurvatureSensitivity, p frtb.Params) float64 {
	t.Helper()
	v, err := frtb.CurvatureCharge(sens, p)
	if err != nil {
		t.Fatalf("frtb.CurvatureCharge refused a fixture whose table should cover it: %v", err)
	}
	return v
}
