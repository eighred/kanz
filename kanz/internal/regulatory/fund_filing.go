package regulatory

import (
	"math/big"

	"fmt"
	"github.com/eighred/kanz/internal/dec"
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
// All four are MONEY, sourced from the book of record, which holds them as exact
// base-10 rationals. They were float64 here, which rounded the ledger's exact
// figure to the nearest binary double on its way into a filing — and a filed NAV
// that does not reconcile to the ledger is a reportable discrepancy.
type FormPFInputs struct {
	GrossAssetValue *big.Rat
	NetAssetValue   *big.Rat
	VaR             *big.Rat
	GrossExposure   *big.Rat
}

// FileFormPF assembles the signed, completeness-gated Form PF filing as of asOf.
// It sanity-checks that gross ≥ net asset value (a net above gross is a book
// error, never a valid filing) before signing. A nil signer defaults to
// HashSigner.
func FileFormPF(in FormPFInputs, asOf time.Time, signer Signer) (Report, error) {
	for code, v := range map[string]*big.Rat{
		"gross asset value": in.GrossAssetValue,
		"net asset value":   in.NetAssetValue,
		"VaR":               in.VaR,
		"gross exposure":    in.GrossExposure,
	} {
		if v == nil {
			return Report{}, fmt.Errorf("regulatory: Form PF %s is missing — a blank is not a filing", code)
		}
	}
	// Exact comparison. As float64 this was a rounded comparison of rounded values:
	// a gross fractionally below net could pass, or a legitimate equality fail.
	if in.GrossAssetValue.Cmp(in.NetAssetValue) < 0 {
		return Report{}, fmt.Errorf("regulatory: Form PF gross NAV %s < net NAV %s",
			dec.Str(in.GrossAssetValue), dec.Str(in.NetAssetValue))
	}
	values := map[string]*big.Rat{
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
	NAV                *big.Rat
	GrossExposure      *big.Rat
	CommitmentExposure *big.Rat
}

// AIFMDResult is the derived leverage breakdown behind the filing.
// The leverage ratios are EXACT: a quotient of two rationals is a rational, so
// exposure/NAV loses nothing. As float64 they were rounded quotients of rounded
// inputs — two roundings on a number a regulator reads as a compliance threshold.
type AIFMDResult struct {
	AUM                *big.Rat
	GrossLeverage      *big.Rat // GrossExposure / NAV
	CommitmentLeverage *big.Rat // CommitmentExposure / NAV
}

// ComputeAIFMD derives the two AIFMD leverage ratios from exposure ÷ NAV. NAV
// must be positive — leverage is undefined for a zero/negative NAV, so the filing
// is refused rather than dividing by zero into an infinity.
func ComputeAIFMD(in AIFMDInputs) (AIFMDResult, error) {
	if in.NAV == nil || in.GrossExposure == nil || in.CommitmentExposure == nil {
		return AIFMDResult{}, fmt.Errorf("regulatory: AIFMD inputs incomplete — a blank is not a filing")
	}
	if in.NAV.Sign() <= 0 {
		return AIFMDResult{}, fmt.Errorf("regulatory: AIFMD leverage undefined for NAV %s (must be > 0)", dec.Str(in.NAV))
	}
	return AIFMDResult{
		AUM:                new(big.Rat).Set(in.NAV),
		GrossLeverage:      new(big.Rat).Quo(in.GrossExposure, in.NAV),
		CommitmentLeverage: new(big.Rat).Quo(in.CommitmentExposure, in.NAV),
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
	values := map[string]*big.Rat{
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
