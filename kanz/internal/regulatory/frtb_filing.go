package regulatory

import (
	"fmt"
	"math/big"
	"time"

	"github.com/eighred/kanz/internal/regulatory/frtb"
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
// Values renders the model's capital charges as EXACT rationals for the filing.
//
// # This is the one honest float boundary in the filing path
//
// FRTBResult stays float64 on purpose, and that is not laziness. The FRTB
// aggregation takes SQUARE ROOTS of quadratic forms over correlation matrices —
// sqrt(x) is irrational for almost every x, so the charge is NOT representable as
// a base-10 rational at all. A big.Rat pipeline through the model would be a lie
// told in a stricter-looking type.
//
// What this conversion does: capture the double's TRUE value exactly (SetFloat64
// is lossless — a float64 IS a rational), so nothing further is lost between the
// model and the regulator, and the filed number is deterministic and signable.
//
// What it does NOT do: invent precision the model never had. The money filings
// (Form PF, AIFMD) ARE exact end to end, because sums and quotients of exact
// figures stay exact. A capital charge derived through a square root is not, and
// pretending otherwise would be theatre.
//
// A non-finite charge (NaN, ±Inf) maps to nil, which BuildReport REFUSES: a
// capital charge that is not a number must not be filed as one.
func (r FRTBResult) Values() map[string]*big.Rat {
	return map[string]*big.Rat{
		"FRTB_DELTA":     exactOrNil(r.Delta),
		"FRTB_VEGA":      exactOrNil(r.Vega),
		"FRTB_CURVATURE": exactOrNil(r.Curvature),
		"FRTB_DRC":       exactOrNil(r.DRC),
		"FRTB_RRAO":      exactOrNil(r.RRAO),
		"FRTB_TOTAL":     exactOrNil(r.Total),
	}
}

// exactOrNil converts a model float to its exact rational value, or nil if it is
// not a finite number (SetFloat64 returns nil for NaN/±Inf).
func exactOrNil(f float64) *big.Rat {
	return new(big.Rat).SetFloat64(f)
}

// ComputeFRTB runs every FRTB charge over the inputs and totals them. It validates
// the supervisory params first (a mis-calibrated table must not silently produce a
// capital number), so a filing is never assembled on invalid parameters.
func ComputeFRTB(in FRTBInputs) (FRTBResult, error) {
	if err := frtb.ValidateParams(in.Params); err != nil {
		return FRTBResult{}, fmt.Errorf("regulatory: FRTB params invalid: %w", err)
	}
	// EACH CHARGE MAY REFUSE, AND A REFUSAL MUST NOT BECOME A NUMBER (#565).
	//
	// ValidateParams above checks the table's internal shape and never sees the
	// sensitivities, so it cannot answer the question that decides this filing:
	// does the table COVER what arrived. A class or bucket it does not cover used
	// to contribute zero silently, so a filing assembled on a table missing
	// Commodity was indistinguishable from a book with no commodity risk — a
	// smaller FRTB_TOTAL, no warning, and a signed report.
	//
	// The error is wrapped rather than returned bare so the failing LEG is named:
	// delta, vega and curvature take the same params and fail for different
	// reasons, and "which one" is the first thing anyone asks.
	delta, err := frtb.Charge(in.Delta, in.Params)
	if err != nil {
		return FRTBResult{}, fmt.Errorf("regulatory: FRTB delta: %w", err)
	}
	vega, err := frtb.Charge(in.Vega, in.Params)
	if err != nil {
		return FRTBResult{}, fmt.Errorf("regulatory: FRTB vega: %w", err)
	}
	curvature, err := frtb.CurvatureCharge(in.Curvature, in.Params)
	if err != nil {
		return FRTBResult{}, fmt.Errorf("regulatory: FRTB curvature: %w", err)
	}
	res := FRTBResult{
		Delta:     delta,
		Vega:      vega,
		Curvature: curvature,
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
// The comparison is EXACT. Both the regulator's worked example and the tolerance
// are decimal figures, so a rational comparison is strictly stronger than a float
// one: a delta that sits exactly on the tolerance boundary now decides
// deterministically rather than on whichever way the last binary rounding fell.
func (r Report) Reconcile(expectedTotal, tol *big.Rat) (delta *big.Rat, ok bool) {
	if expectedTotal == nil || tol == nil {
		return nil, false
	}
	filed, found := r.Lookup("FRTB_TOTAL")
	if !found || filed == nil {
		return nil, false
	}
	delta = new(big.Rat).Sub(filed, expectedTotal)
	return delta, new(big.Rat).Abs(delta).Cmp(tol) <= 0
}
