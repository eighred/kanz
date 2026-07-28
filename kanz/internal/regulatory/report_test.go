package regulatory

import (
	"github.com/eighred/kanz/internal/dec"
	"math/big"
	"testing"
	"time"
)

func frtbValues() map[string]*big.Rat {
	return map[string]*big.Rat{
		"FRTB_DELTA":     dec.Rat("1200000"),
		"FRTB_VEGA":      dec.Rat("300000"),
		"FRTB_CURVATURE": dec.Rat("150000"),
		"FRTB_DRC":       dec.Rat("400000"),
		"FRTB_RRAO":      dec.Rat("50000"),
		"FRTB_TOTAL":     dec.Rat("2100000"),
	}
}

func TestBuildReport_Complete(t *testing.T) {
	asOf := time.Date(2026, 6, 28, 0, 0, 0, 0, time.UTC)
	r, err := BuildReport(FRTB, asOf, frtbValues(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.LineItems) != 6 {
		t.Fatalf("FRTB report should have 6 line items, got %d", len(r.LineItems))
	}
	if r.Signature == "" {
		t.Fatal("a built report must be signed")
	}
	if v, ok := r.Lookup("FRTB_TOTAL"); !ok || v.Cmp(dec.Rat("2100000")) != 0 {
		t.Fatalf("FRTB_TOTAL line item missing/wrong: %v ok=%v", v, ok)
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
	tampered["FRTB_TOTAL"] = dec.Rat("1650001")
	c, _ := BuildReport(FRTB, asOf, tampered, nil)
	if c.Signature == a.Signature {
		t.Fatal("a changed line item must change the signature (tamper-evident)")
	}
}

func TestBuildReport_FormPFAndAIFMD(t *testing.T) {
	pf, err := BuildReport(FormPF, time.Now(), map[string]*big.Rat{
		"FORM_PF_GROSS_NAV": dec.Rat("1000000000"), "FORM_PF_NET_NAV": dec.Rat("800000000"), "FORM_PF_VAR": dec.Rat("20000000"), "FORM_PF_GROSS_EXPOSURE": dec.Rat("3000000000"),
	}, nil)
	if err != nil || len(pf.LineItems) != 4 {
		t.Fatalf("Form PF report build failed: %v", err)
	}
	aifmd, err := BuildReport(AIFMD, time.Now(), map[string]*big.Rat{
		"AIFMD_AUM": dec.Rat("500000000"), "AIFMD_LEVERAGE_GROSS": dec.Rat("2.5"), "AIFMD_LEVERAGE_COMMITMENT": dec.Rat("1.8"),
	}, nil)
	if err != nil || len(aifmd.LineItems) != 3 {
		t.Fatalf("AIFMD report build failed: %v", err)
	}
}
