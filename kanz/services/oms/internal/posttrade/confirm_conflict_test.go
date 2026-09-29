package posttrade

import (
	"math/big"
	"reflect"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

func TestReconcileConflictsArePermutationInvariant(t *testing.T) {
	good := confFixture("F1")
	bad := confFixture("F1")
	bad.Price = big.NewRat(151, 1)
	other := confFixture("F1")
	other.ConfirmationID = "amendment-without-authorized-supersession"
	evidence := []Confirmation{good, bad, good, other}
	want, _, _ := Reconcile([]*orderpb.Fill{fillFixture("F1")}, evidence, MatchTolerance{})
	if len(want) != 3 {
		t.Fatalf("must retain three distinct disputed records: %+v", want)
	}
	for _, b := range want {
		if !reflect.DeepEqual(b.Fields, []string{"conflicting_confirmations"}) {
			t.Fatalf("must dispute all candidates: %+v", b)
		}
	}
	var permutations func(int)
	permutations = func(i int) {
		if i == len(evidence) {
			got, missing, orphans := Reconcile([]*orderpb.Fill{fillFixture("F1")}, evidence, MatchTolerance{})
			if !reflect.DeepEqual(got, want) || len(missing)+len(orphans) != 0 {
				t.Fatalf("arrival order changed dispute: %+v, %+v", got, want)
			}
			return
		}
		for j := i; j < len(evidence); j++ {
			evidence[i], evidence[j] = evidence[j], evidence[i]
			permutations(i + 1)
			evidence[i], evidence[j] = evidence[j], evidence[i]
		}
	}
	permutations(0)
}

func TestReconcileIdenticalRedelivery(t *testing.T) {
	c := confFixture("F1")
	duplicate := confFixture("F1")
	duplicate.Quantity = big.NewRat(200, 2)
	duplicate.SettlementDate = c.SettlementDate.In(time.FixedZone("offset", 3600))
	f := fillFixture("F1")
	breaks, missing, orphans := Reconcile([]*orderpb.Fill{f, f}, []Confirmation{c, duplicate}, MatchTolerance{})
	if len(breaks)+len(missing)+len(orphans) != 0 {
		t.Fatalf("identical delivery is not a dispute: %+v %v %v", breaks, missing, orphans)
	}
}

func TestReconcileOrphanRedelivery(t *testing.T) {
	c := confFixture("F1")
	breaks, missing, orphans := Reconcile(nil, []Confirmation{c, c}, MatchTolerance{})
	if len(breaks)+len(missing) != 0 || !reflect.DeepEqual(orphans, []string{c.ConfirmationID}) {
		t.Fatalf("orphan retry changed result: %+v %v %v", breaks, missing, orphans)
	}
}

func TestReconcileToleranceDoesNotEraseConflict(t *testing.T) {
	a, b := confFixture("F1"), confFixture("F1")
	b.Price = big.NewRat(15001, 100)
	breaks, _, _ := Reconcile([]*orderpb.Fill{fillFixture("F1")}, []Confirmation{a, b}, MatchTolerance{Price: big.NewRat(1, 1)})
	if len(breaks) != 2 || breaks[0].Fields[0] != "conflicting_confirmations" {
		t.Fatalf("tolerance erased conflicting source evidence: %+v", breaks)
	}
}

func TestMatchFillSignedPrice(t *testing.T) {
	f, c := fillFixture("F1"), confFixture("F1")
	// Negative execution prices can be real; missing prices must not become zero.
	f.Price, c.Price = dnum(-1), big.NewRat(-1, 1)
	if result := MatchFill(f, c, MatchTolerance{}); !result.Matched {
		t.Fatalf("signed financial value rejected: %+v", result.Break)
	}
}

func TestReconcileRefusesIdentityRebinding(t *testing.T) {
	a, b := confFixture("F1"), confFixture("F2")
	b.ConfirmationID = a.ConfirmationID
	for _, evidence := range [][]Confirmation{{a, b}, {b, a}} {
		breaks, _, _ := Reconcile([]*orderpb.Fill{fillFixture("F1"), fillFixture("F2")}, evidence, MatchTolerance{})
		if len(breaks) != 2 {
			t.Fatalf("identity rebound to another fill: %+v", breaks)
		}
	}
}

func TestReconcileRefusesCollidingAccountFills(t *testing.T) {
	a, b := fillFixture("F1"), fillFixture("F1")
	b.VenueAccountId = "another-account"
	for _, fills := range [][]*orderpb.Fill{{a, b}, {b, a}} {
		breaks, _, _ := Reconcile(fills, []Confirmation{confFixture("F1")}, MatchTolerance{})
		if len(breaks) != 1 || breaks[0].Fields[0] != "conflicting_fills" {
			t.Fatalf("colliding fills affirmed: %+v", breaks)
		}
	}
}

func TestMatchFillRefusesInvalidEvidence(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(**orderpb.Fill, *Confirmation, *MatchTolerance)
	}{
		{"nil fill", func(f **orderpb.Fill, _ *Confirmation, _ *MatchTolerance) { *f = nil }},
		{"fill binding", func(_ **orderpb.Fill, c *Confirmation, _ *MatchTolerance) { c.FillID = "other" }},
		{"account binding", func(_ **orderpb.Fill, c *Confirmation, _ *MatchTolerance) { c.VenueAccountID = "other" }},
		{"missing account", func(f **orderpb.Fill, c *Confirmation, _ *MatchTolerance) {
			(*f).VenueAccountId = ""
			c.VenueAccountID = ""
		}},
		{"nil quantity", func(_ **orderpb.Fill, c *Confirmation, _ *MatchTolerance) { c.Quantity = nil }},
		{"nil price", func(_ **orderpb.Fill, c *Confirmation, _ *MatchTolerance) { c.Price = nil }},
		{"nil fill quantity", func(f **orderpb.Fill, _ *Confirmation, _ *MatchTolerance) { (*f).Quantity = nil }},
		{"nil fill price", func(f **orderpb.Fill, _ *Confirmation, _ *MatchTolerance) { (*f).Price = nil }},
		{"unsafe exponent", func(f **orderpb.Fill, _ *Confirmation, _ *MatchTolerance) { (*f).Price.Exponent = 2147483647 }},
		{"empty id", func(_ **orderpb.Fill, c *Confirmation, _ *MatchTolerance) { c.ConfirmationID = " " }},
		{"empty counterparty", func(_ **orderpb.Fill, c *Confirmation, _ *MatchTolerance) { c.Counterparty = "" }},
		{"unknown side", func(f **orderpb.Fill, c *Confirmation, _ *MatchTolerance) { (*f).Side = 42; c.Side = 42 }},
		{"zero quantity", func(f **orderpb.Fill, c *Confirmation, _ *MatchTolerance) {
			(*f).Quantity = dnum(0)
			c.Quantity = new(big.Rat)
		}},
		{"negative tolerance", func(_ **orderpb.Fill, _ *Confirmation, tol *MatchTolerance) { tol.Price = big.NewRat(-1, 1) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, c, tol := fillFixture("F1"), confFixture("F1"), MatchTolerance{}
			tc.mutate(&f, &c, &tol)
			if result := MatchFill(f, c, tol); result.Matched || result.Break == nil {
				t.Fatal("invalid evidence affirmed")
			}
		})
	}
}
