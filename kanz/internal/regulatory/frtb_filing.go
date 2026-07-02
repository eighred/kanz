package regulatory

import (
	"fmt"
	"time"

	"github.com/kanz-eng/kanz/internal/regulatory/frtb"
)

// FRTB capital filing end-to-end (PARITY-06a). The frtb package ships the SBM
// (delta/vega), curvature, DRC, and RRAO engines as isolated charges; this is the
// assembler that drives them from ONE live CRIF + position set into the signed,
// completeness-gated RegReport a bank actually files — MAR total = SBM(delta) +
// SBM(vega) + curvature + DRC + RRAO. It reuses the REG-01d BuildReport signing
// path (no parallel report machinery) and lives in the parent regulatory package
// so it can compose every frtb subpackage engine (frtb imports nothing here — no
// cycle), the same "assemble the delivered pieces at the reporting layer" stance
// the analytics take.

// FRTBInputs is the complete input set for one FRTB market-risk capital filing.
// Sensitivities are the CRIF (frtb.DeltaSensitivities / VegaSensitivities parse
// them from a vendor CRIF file); the JTD and RRAO position sets drive the two
// non-SBM add-ons. Params is the supervisory delta/vega/curvature calibration;
// DRCParams the default-risk calibration.
type FRTBInputs struct {
	Delta     []frtb.Sensitivity
	Vega      []frtb.Sensitivity
	Curvature []frtb.CurvatureSensitivity
	JTD       []frtb.JTDPosition
	RRAO      []frtb.RRAOPosition
	Params    frtb.Params
	DRCParams frtb.DRCParams
}

// FRTBResult is the component breakdown behind a filing — retained alongside the
// signed Report as the sign-off package's supporting worksheet (each MAR charge
// shown, so a reviewer reconciles the total against its parts).
type FRTBResult struct {
	Delta     float64
	Vega      float64
	Curvature float64
	DRC       float64
	RRAO      float64
	Total     float64
}

// Values renders the breakdown to the report.BuildReport code→amount map — the
// exact set the FRTB template requires, so the filing is complete by construction.
func (r FRTBResult) Values() map[string]float64 {
	return map[string]float64{
		"FRTB_DELTA":     r.Delta,
		"FRTB_VEGA":      r.Vega,
		"FRTB_CURVATURE": r.Curvature,
		"FRTB_DRC":       r.DRC,
		"FRTB_RRAO":      r.RRAO,
		"FRTB_TOTAL":     r.Total,
	}
}

// ComputeFRTB runs every FRTB charge over the inputs and totals them. It validates
// the supervisory params first (a mis-calibrated table must not silently produce a
// capital number), so a filing is never assembled on invalid parameters.
func ComputeFRTB(in FRTBInputs) (FRTBResult, error) {
	if err := frtb.ValidateParams(in.Params); err != nil {
		return FRTBResult{}, fmt.Errorf("regulatory: FRTB params invalid: %w", err)
	}
	res := FRTBResult{
		Delta:     frtb.Charge(in.Delta, in.Params),
		Vega:      frtb.Charge(in.Vega, in.Params),
		Curvature: frtb.CurvatureCharge(in.Curvature, in.Params),
		DRC:       frtb.DRC(in.JTD, in.DRCParams),
		RRAO:      frtb.RRAO(in.RRAO),
	}
	res.Total = res.Delta + res.Vega + res.Curvature + res.DRC + res.RRAO
	return res, nil
}

// FileFRTB computes the charges and assembles the signed, completeness-gated FRTB
// filing as of asOf. It returns both the Report (the filed artifact) and the
// FRTBResult breakdown (the sign-off worksheet). A nil signer defaults to the
// content-hash HashSigner; a deployment injects the AUDIT-01 hash-chain signer.
func FileFRTB(in FRTBInputs, asOf time.Time, signer Signer) (Report, FRTBResult, error) {
	res, err := ComputeFRTB(in)
	if err != nil {
		return Report{}, FRTBResult{}, err
	}
	rep, err := BuildReport(FRTB, asOf, res.Values(), signer)
	if err != nil {
		return Report{}, FRTBResult{}, err
	}
	return rep, res, nil
}

// Reconcile checks a filing's total line item against an independently supplied
// expected total (the regulator's worked example, or an independent recompute)
// within tol. It is the reconciliation gate the sign-off package records: a
// filing whose total does not reproduce the benchmark is NOT signed off, even
// though it is internally complete. Returns the signed delta (filed − expected)
// and whether it is within tolerance.
func (r Report) Reconcile(expectedTotal, tol float64) (delta float64, ok bool) {
	filed, found := r.Lookup("FRTB_TOTAL")
	if !found {
		return 0, false
	}
	delta = filed - expectedTotal
	d := delta
	if d < 0 {
		d = -d
	}
	return delta, d <= tol
}
