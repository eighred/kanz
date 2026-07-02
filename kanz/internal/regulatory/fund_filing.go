package regulatory

import (
	"fmt"
	"time"
)

// Form PF + AIFMD fund filings end-to-end (PARITY-06b). Like FileFRTB, these are
// the assemblers that turn the LIVE surfaces a fund already computes — the IBOR
// NAV (gross/net asset value) and the risk engine's VaR + notional exposure —
// into the signed, completeness-gated RegReport a manager files. The regulatory
// package takes those figures as INPUTS (the RISK-02 boundary: it may not import
// internal/risk or the accounting service), and its value-add is the regulatory
// DERIVATIONS the raw surfaces don't give: the two AIFMD leverage ratios.

// FormPFInputs is the SEC Form PF section-1 input set, sourced point-in-time from
// the IBOR (gross/net asset value) and the risk engine (VaR, gross notional
// exposure). All four are required line items; a missing or non-finite figure
// fails the filing rather than reporting a blank.
type FormPFInputs struct {
	GrossAssetValue float64
	NetAssetValue   float64
	VaR             float64
	GrossExposure   float64
}

// FileFormPF assembles the signed, completeness-gated Form PF filing as of asOf.
// It sanity-checks that gross ≥ net asset value (a net above gross is a book
// error, never a valid filing) before signing. A nil signer defaults to
// HashSigner.
func FileFormPF(in FormPFInputs, asOf time.Time, signer Signer) (Report, error) {
	if in.GrossAssetValue < in.NetAssetValue {
		return Report{}, fmt.Errorf("regulatory: Form PF gross NAV %.2f < net NAV %.2f", in.GrossAssetValue, in.NetAssetValue)
	}
	values := map[string]float64{
		"FORM_PF_GROSS_NAV":      in.GrossAssetValue,
		"FORM_PF_NET_NAV":        in.NetAssetValue,
		"FORM_PF_VAR":            in.VaR,
		"FORM_PF_GROSS_EXPOSURE": in.GrossExposure,
	}
	return BuildReport(FormPF, asOf, values, signer)
}

// AIFMDInputs is the AIFMD Annex-IV leverage-reporting input set: the fund NAV
// and its two exposure measures. GrossExposure is the sum of absolute position
// notionals (gross method); CommitmentExposure is the netting/hedging-adjusted
// exposure (commitment method). The two leverage RATIOS are derived here.
type AIFMDInputs struct {
	NAV                float64
	GrossExposure      float64
	CommitmentExposure float64
}

// AIFMDResult is the derived leverage breakdown behind the filing.
type AIFMDResult struct {
	AUM                float64
	GrossLeverage      float64 // GrossExposure / NAV
	CommitmentLeverage float64 // CommitmentExposure / NAV
}

// ComputeAIFMD derives the two AIFMD leverage ratios from exposure ÷ NAV. NAV
// must be positive — leverage is undefined for a zero/negative NAV, so the filing
// is refused rather than dividing by zero into an infinity.
func ComputeAIFMD(in AIFMDInputs) (AIFMDResult, error) {
	if in.NAV <= 0 {
		return AIFMDResult{}, fmt.Errorf("regulatory: AIFMD leverage undefined for NAV %.2f (must be > 0)", in.NAV)
	}
	return AIFMDResult{
		AUM:                in.NAV,
		GrossLeverage:      in.GrossExposure / in.NAV,
		CommitmentLeverage: in.CommitmentExposure / in.NAV,
	}, nil
}

// FileAIFMD computes the leverage ratios and assembles the signed,
// completeness-gated AIFMD filing as of asOf. Returns the Report and the derived
// AIFMDResult breakdown. A nil signer defaults to HashSigner.
func FileAIFMD(in AIFMDInputs, asOf time.Time, signer Signer) (Report, AIFMDResult, error) {
	res, err := ComputeAIFMD(in)
	if err != nil {
		return Report{}, AIFMDResult{}, err
	}
	values := map[string]float64{
		"AIFMD_AUM":                 res.AUM,
		"AIFMD_LEVERAGE_GROSS":      res.GrossLeverage,
		"AIFMD_LEVERAGE_COMMITMENT": res.CommitmentLeverage,
	}
	rep, err := BuildReport(AIFMD, asOf, values, signer)
	if err != nil {
		return Report{}, AIFMDResult{}, err
	}
	return rep, res, nil
}
