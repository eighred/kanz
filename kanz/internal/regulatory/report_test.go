package regulatory

import (
	"testing"
	"time"
)

func frtbValues() map[string]float64 {
	return map[string]float64{
		"FRTB_DELTA":     1_200_000,
		"FRTB_VEGA":      300_000,
		"FRTB_CURVATURE": 150_000,
		"FRTB_TOTAL":     1_650_000,
	}
}

func TestBuildReport_Complete(t *testing.T) {
	asOf := time.Date(2026, 6, 28, 0, 0, 0, 0, time.UTC)
	r, err := BuildReport(FRTB, asOf, frtbValues(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.LineItems) != 4 {
		t.Fatalf("FRTB report should have 4 line items, got %d", len(r.LineItems))
	}
	if r.Signature == "" {
		t.Fatal("a built report must be signed")
	}
	if v, ok := r.Lookup("FRTB_TOTAL"); !ok || v != 1_650_000 {
		t.Fatalf("FRTB_TOTAL line item missing/wrong: %.0f ok=%v", v, ok)
	}
}

func TestBuildReport_MissingLineItemFails(t *testing.T) {
	vals := frtbValues()
	delete(vals, "FRTB_CURVATURE")
	if _, err := BuildReport(FRTB, time.Now(), vals, nil); err == nil {
		t.Fatal("an incomplete report must error (completeness), not sign")
	}
}

func TestBuildReport_UnknownFramework(t *testing.T) {
	if _, err := BuildReport(Framework("BOGUS"), time.Now(), nil, nil); err == nil {
		t.Fatal("an unknown framework must error")
	}
}

func TestBuildReport_SignatureDeterministicAndTamperEvident(t *testing.T) {
	asOf := time.Date(2026, 6, 28, 0, 0, 0, 0, time.UTC)
	a, _ := BuildReport(FRTB, asOf, frtbValues(), nil)
	b, _ := BuildReport(FRTB, asOf, frtbValues(), nil)
	if a.Signature != b.Signature {
		t.Fatal("the same inputs must produce the same signature")
	}
	tampered := frtbValues()
	tampered["FRTB_TOTAL"] = 1_650_001
	c, _ := BuildReport(FRTB, asOf, tampered, nil)
	if c.Signature == a.Signature {
		t.Fatal("a changed line item must change the signature (tamper-evident)")
	}
}

func TestBuildReport_FormPFAndAIFMD(t *testing.T) {
	pf, err := BuildReport(FormPF, time.Now(), map[string]float64{
		"FORM_PF_GROSS_NAV": 1e9, "FORM_PF_NET_NAV": 8e8, "FORM_PF_VAR": 2e7, "FORM_PF_GROSS_EXPOSURE": 3e9,
	}, nil)
	if err != nil || len(pf.LineItems) != 4 {
		t.Fatalf("Form PF report build failed: %v", err)
	}
	aifmd, err := BuildReport(AIFMD, time.Now(), map[string]float64{
		"AIFMD_AUM": 5e8, "AIFMD_LEVERAGE_GROSS": 2.5, "AIFMD_LEVERAGE_COMMITMENT": 1.8,
	}, nil)
	if err != nil || len(aifmd.LineItems) != 3 {
		t.Fatalf("AIFMD report build failed: %v", err)
	}
}
