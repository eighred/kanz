package structured

import (
	"errors"
	"math"
	"testing"
)

func testDeal() Deal {
	return Deal{
		Pool: Pool{Balance: 1_000_000, GrossCoupon: 0.06, ServicingFee: 0.005, TermMonths: 360},
		Tranches: []Tranche{
			{Name: "A", Balance: 800_000, Coupon: 0.04},
			{Name: "B", Balance: 200_000, Coupon: 0.06},
		},
	}
}

// TestWaterfall_ConservesCash: every month, the pool's distributable cash equals
// the sum of the tranche interest+principal plus the residual — no cash is
// created or destroyed in the waterfall.
func TestWaterfall_ConservesCash(t *testing.T) {
	d := testDeal()
	proj := d.Project(ConstantCPR{CPR: 0.08, CDR: 0.01, Sev: 0.35}, RateEnv{})
	for m, pc := range proj.Pool {
		poolCash := pc.Interest + pc.Principal
		var dist float64
		for _, tr := range proj.Tranches {
			dist += tr.Interest[m] + tr.Principal[m]
		}
		dist += proj.Residual[m]
		if math.Abs(dist-poolCash) > 1e-6 {
			t.Fatalf("month %d: distributed %.6f != pool cash %.6f", m+1, dist, poolCash)
		}
	}
}

// TestProject_PrincipalRetiresStructure: the total principal paid across tranches
// (plus residual principal) equals the pool principal collected over the life,
// and the senior tranche retires before the junior (sequential pay).
func TestProject_PrincipalRetiresStructure(t *testing.T) {
	d := testDeal()
	proj := d.Project(ConstantCPR{CPR: 0.10}, RateEnv{})
	// Senior A retires (balance →0) no later than junior B.
	a := proj.Tranches[0].Balance
	b := proj.Tranches[1].Balance
	aRetire := retireMonth(a)
	bRetire := retireMonth(b)
	if aRetire > bRetire {
		t.Fatalf("senior A must retire no later than junior B: A@%d B@%d", aRetire, bRetire)
	}
}

func retireMonth(bal []float64) int {
	for i, b := range bal {
		if b < 1e-6 {
			return i
		}
	}
	return len(bal)
}

// TestWAL_FasterPrepayShortens: a higher CPR shortens the senior tranche's
// weighted-average life.
func TestWAL_FasterPrepayShortens(t *testing.T) {
	d := testDeal()
	slow := walOf(t, d.Project(ConstantCPR{CPR: 0.06}, RateEnv{}), 0)
	fast := walOf(t, d.Project(ConstantCPR{CPR: 0.40}, RateEnv{}), 0)
	if fast >= slow {
		t.Fatalf("faster prepay must shorten WAL: fast=%.3f !< slow=%.3f", fast, slow)
	}
}

func walOf(t *testing.T, proj Projection, idx int) float64 {
	t.Helper()
	tr, err := proj.Tranche(idx)
	if err != nil {
		t.Fatalf("tranche %d: %v", idx, err)
	}
	w, err := tr.WAL()
	if err != nil {
		t.Fatalf("WAL of tranche %d: %v", idx, err)
	}
	return w
}

// A TRANCHE INDEX THE DEAL DOES NOT HAVE MUST NOT COME BACK AS A CASHFLOW (#572).
// Projection.Tranche exists because two callers indexed Tranches directly: one
// bounds-checked into a zero price, the other did not bounds-check at all and
// panicked. Both are one malformed StructuredSpec away in a provider nobody has
// written yet.
func TestTranche_RefusesAnIndexTheProjectionDoesNotHold(t *testing.T) {
	proj := testDeal().Project(ConstantCPR{CPR: 0.06}, RateEnv{})
	for _, idx := range []int{-1, len(proj.Tranches), len(proj.Tranches) + 7} {
		if _, err := proj.Tranche(idx); !errors.Is(err, ErrUnpriceable) {
			t.Errorf("Tranche(%d) returned err=%v, want ErrUnpriceable — an out-of-range tranche "+
				"must refuse, not answer", idx, err)
		}
	}
}

// A WAL OF ZERO SAYS THE PRINCIPAL COMES BACK IMMEDIATELY (#572). A tranche that
// receives no principal has no weighted-average life; answering 0 is the most
// flattering number available for the worst case (a tranche written down to
// nothing).
func TestWAL_RefusesATrancheThatReceivesNoPrincipal(t *testing.T) {
	empty := TrancheCashflow{Name: "Z", Principal: []float64{0, 0, 0}}
	w, err := empty.WAL()
	if !errors.Is(err, ErrUnpriceable) {
		t.Fatalf("WAL over no principal returned (%v, %v), want ErrUnpriceable", w, err)
	}
}
