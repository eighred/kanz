package collateral

import (
	"math"
	"testing"
)

func TestVariationMargin_TracksMtM(t *testing.T) {
	terms := CSATerms{Threshold: 100, MinTransferAmount: 10}
	// Below the threshold ⇒ no required collateral, no call.
	if call, req := VariationMargin(80, 0, terms); call != 0 || req != 0 {
		t.Fatalf("below threshold: call=%.2f req=%.2f want 0,0", call, req)
	}
	// Above the threshold the required collateral tracks MtM one-for-one.
	_, req1 := VariationMargin(500, 0, terms)
	_, req2 := VariationMargin(600, 0, terms)
	if math.Abs((req2-req1)-100) > 1e-9 {
		t.Fatalf("required must rise with MtM one-for-one above threshold: %.2f→%.2f", req1, req2)
	}
	// A rising mark with nothing held ⇒ a positive call to us = required.
	call, req := VariationMargin(500, 0, terms)
	if call != req || call != 400 {
		t.Fatalf("call should be required 400, got call=%.2f req=%.2f", call, req)
	}
}

func TestVariationMargin_MTAandRounding(t *testing.T) {
	terms := CSATerms{Threshold: 0, MinTransferAmount: 50, Rounding: 25}
	// Gap below the MTA ⇒ suppressed.
	if call, _ := VariationMargin(140, 100, terms); call != 0 {
		t.Fatalf("a sub-MTA gap must be suppressed, got %.2f", call)
	}
	// A call to us (positive) rounds up to the increment.
	if call, _ := VariationMargin(260, 0, terms); call != 275 {
		t.Fatalf("call should round up to 275, got %.2f", call)
	}
	// A call we owe (negative) rounds down (toward a larger return to us... away
	// from the holder) — magnitude rounds up.
	if call, _ := VariationMargin(0, 260, terms); call != -275 {
		t.Fatalf("a negative call should round to -275, got %.2f", call)
	}
}
