package regulatory

import (
	"github.com/eighred/kanz/internal/dec"
	"math/big"
	"testing"
	"time"
)

func TestFileFormPF_SignedComplete(t *testing.T) {
	asOf := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	rep, err := FileFormPF(FormPFInputs{
		GrossAssetValue: big.NewRat(500000000, 1),
		NetAssetValue:   big.NewRat(450000000, 1),
		VaR:             big.NewRat(12000000, 1),
		GrossExposure:   big.NewRat(1200000000, 1),
	}, asOf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Signature == "" || len(rep.LineItems) != 4 {
		t.Fatalf("Form PF filing malformed: sig=%q items=%d", rep.Signature, len(rep.LineItems))
	}
	// Exact: the filed NAV must equal the ledger figure to the cent, not "close".
	if v, _ := rep.Lookup("FORM_PF_NET_NAV"); v.Cmp(big.NewRat(450_000_000, 1)) != 0 {
		t.Fatalf("net NAV line item = %v", v)
	}
}

// A net asset value above gross is a book error — the filing is refused.
func TestFileFormPF_NetAboveGrossRejected(t *testing.T) {
	_, err := FileFormPF(FormPFInputs{GrossAssetValue: big.NewRat(100, 1), NetAssetValue: big.NewRat(200, 1)}, time.Now(), nil)
	if err == nil {
		t.Fatal("expected rejection when net NAV exceeds gross NAV")
	}
}

// AIFMD leverage is exposure ÷ NAV, both ratios derived and filed.
func TestFileAIFMD_DerivesLeverageRatios(t *testing.T) {
	asOf := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	rep, res, err := FileAIFMD(AIFMDInputs{
		NAV:                big.NewRat(200000000, 1),
		GrossExposure:      big.NewRat(600000000, 1), // 3.0x gross
		CommitmentExposure: big.NewRat(300000000, 1), // 1.5x commitment
	}, asOf, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly 3 and exactly 3/2 — a quotient of rationals is a rational, so the
	// leverage ratios lose nothing. As float64 these were rounded quotients of
	// rounded inputs, on a number a regulator reads as a compliance threshold.
	if res.GrossLeverage.Cmp(big.NewRat(3, 1)) != 0 || res.CommitmentLeverage.Cmp(big.NewRat(3, 2)) != 0 {
		t.Fatalf("leverage = gross %v commitment %v want 3 / 1.5", res.GrossLeverage, res.CommitmentLeverage)
	}
	if v, _ := rep.Lookup("AIFMD_LEVERAGE_GROSS"); v.Cmp(big.NewRat(3, 1)) != 0 {
		t.Fatalf("filed gross leverage = %v want 3", v)
	}
	if v, _ := rep.Lookup("AIFMD_AUM"); v.Cmp(dec.Rat("200000000")) != 0 {
		t.Fatalf("filed AUM = %v want 200000000", v)
	}
}

// Leverage is undefined for a non-positive NAV — the filing is refused rather
// than dividing into an infinity.
func TestComputeAIFMD_ZeroNAVRejected(t *testing.T) {
	if _, err := ComputeAIFMD(AIFMDInputs{NAV: big.NewRat(0, 1), GrossExposure: big.NewRat(100, 1)}); err == nil {
		t.Fatal("expected rejection for zero NAV")
	}
}
