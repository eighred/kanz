package regulatory

import (
	"testing"
	"time"
)

func TestFileFormPF_SignedComplete(t *testing.T) {
	asOf := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	rep, err := FileFormPF(FormPFInputs{
		GrossAssetValue: 500_000_000,
		NetAssetValue:   450_000_000,
		VaR:             12_000_000,
		GrossExposure:   1_200_000_000,
	}, asOf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Signature == "" || len(rep.LineItems) != 4 {
		t.Fatalf("Form PF filing malformed: sig=%q items=%d", rep.Signature, len(rep.LineItems))
	}
	if v, _ := rep.Lookup("FORM_PF_NET_NAV"); v != 450_000_000 {
		t.Fatalf("net NAV line item = %.0f", v)
	}
}

// A net asset value above gross is a book error — the filing is refused.
func TestFileFormPF_NetAboveGrossRejected(t *testing.T) {
	_, err := FileFormPF(FormPFInputs{GrossAssetValue: 100, NetAssetValue: 200}, time.Now(), nil)
	if err == nil {
		t.Fatal("expected rejection when net NAV exceeds gross NAV")
	}
}

// AIFMD leverage is exposure ÷ NAV, both ratios derived and filed.
func TestFileAIFMD_DerivesLeverageRatios(t *testing.T) {
	asOf := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	rep, res, err := FileAIFMD(AIFMDInputs{
		NAV:                200_000_000,
		GrossExposure:      600_000_000, // 3.0x gross
		CommitmentExposure: 300_000_000, // 1.5x commitment
	}, asOf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.GrossLeverage != 3.0 || res.CommitmentLeverage != 1.5 {
		t.Fatalf("leverage = gross %.2f commitment %.2f want 3.0 / 1.5", res.GrossLeverage, res.CommitmentLeverage)
	}
	if v, _ := rep.Lookup("AIFMD_LEVERAGE_GROSS"); v != 3.0 {
		t.Fatalf("filed gross leverage = %.2f want 3.0", v)
	}
	if v, _ := rep.Lookup("AIFMD_AUM"); v != 200_000_000 {
		t.Fatalf("filed AUM = %.0f want 200000000", v)
	}
}

// Leverage is undefined for a non-positive NAV — the filing is refused rather
// than dividing into an infinity.
func TestComputeAIFMD_ZeroNAVRejected(t *testing.T) {
	if _, err := ComputeAIFMD(AIFMDInputs{NAV: 0, GrossExposure: 100}); err == nil {
		t.Fatal("expected rejection for zero NAV")
	}
}
