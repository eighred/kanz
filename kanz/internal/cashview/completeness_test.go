package cashview

import (
	"context"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// WHAT THE ANNOUNCED NUMBER CONTAINS TRAVELS WITH THE NUMBER (#614).
//
// The book of record folds six kinds of journal entry and nothing on this
// platform produces two of them (#588), so the level this view remembers is
// missing every dividend, coupon and merger payment. The view must not repair
// that — it may not compute a balance at all — but it must not swallow the
// producer's own statement of it either, because the pre-trade gate downstream
// is the thing that turns the gap into a refused order.

func announcementWith(t *testing.T, total int64, cp *accountingpb.BalanceCompleteness,
	asOf time.Time) []byte {
	t.Helper()
	b, err := proto.Marshal(&accountingpb.PortfolioCashBalance{
		PortfolioId:   "PF1",
		BaseCurrency:  "USD",
		Total:         &commonpb.Decimal{Coefficient: total},
		AsOf:          timestamppb.New(asOf),
		KnowledgeTime: timestamppb.New(asOf),
		Completeness:  cp,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestSpendable_CarriesTheProducersCompleteness(t *testing.T) {
	v := New(WithClock(func() time.Time { return t0 }))
	payload := announcementWith(t, 750, &accountingpb.BalanceCompleteness{
		ProducedEntryTypes:   []string{"cash", "fee", "trade"},
		UnproducedEntryTypes: []string{"accrual", "corporate_action"},
	}, t0)
	if err := v.Handle(context.Background(), nil, payload); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	_, _, cc, ok := v.Spendable("PF1")
	if !ok {
		t.Fatal("the balance was not folded")
	}
	if !cc.Stated() {
		t.Fatal("the producer stated a posture and Spendable reported none — the gate downstream " +
			"then cannot tell a spending limit from a missing feed (#614)")
	}
	if !cc.Incomplete() {
		t.Fatal("the producer named two unproduced entry types and the view called the balance " +
			"complete")
	}
	want := []string{"accrual", "corporate_action"}
	if got := cc.OmittedEntryTypes; len(got) != len(want) {
		t.Fatalf("OmittedEntryTypes = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("OmittedEntryTypes = %v, want %v", got, want)
			}
		}
	}
}

// A PRODUCER THAT SAYS NOTHING MUST REACH THE GATE AS "UNSTATED" (#614).
//
// Two shapes land here: an accounting old enough to predate the field, and one
// that sent a BalanceCompleteness naming neither produced nor unproduced types.
// proto3 cannot tell an empty repeated field from an absent one, so the second
// is not a statement — it is a caller that forgot — and treating it as "nothing
// is missing" would publish exactly the false assurance the field exists to
// abolish.
func TestSpendable_AnEmptyOrAbsentStatementIsUnstated(t *testing.T) {
	cases := map[string]*accountingpb.BalanceCompleteness{
		"absent":            nil,
		"present but empty": {},
	}
	for name, cp := range cases {
		v := New(WithClock(func() time.Time { return t0 }))
		if err := v.Handle(context.Background(), nil, announcementWith(t, 750, cp, t0)); err != nil {
			t.Fatalf("%s: Handle: %v", name, err)
		}
		total, _, cc, ok := v.Spendable("PF1")
		if !ok {
			t.Fatalf("%s: the balance was not folded", name)
		}
		if total.GetCoefficient() != 750 {
			t.Fatalf("%s: total = %d, want 750 — the completeness handling must not disturb the "+
				"number", name, total.GetCoefficient())
		}
		if cc.Stated() {
			t.Fatalf("%s: a completeness statement that names nothing was reported as a statement. "+
				"Silence and \"nothing is missing\" must not be the same answer (#614)", name)
		}
	}
}

// AND A PRODUCER THAT VOUCHES FOR ITS NUMBER CAN SAY SO. Without this arm the
// view could satisfy the test above by always reporting "unstated", which would
// make the gate permanently unable to say "checked, and fine".
func TestSpendable_AFullyFedProducerIsStatedAndComplete(t *testing.T) {
	v := New(WithClock(func() time.Time { return t0 }))
	payload := announcementWith(t, 750, &accountingpb.BalanceCompleteness{
		ProducedEntryTypes: []string{"accrual", "cash", "corporate_action", "fee", "trade"},
	}, t0)
	if err := v.Handle(context.Background(), nil, payload); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	_, _, cc, ok := v.Spendable("PF1")
	if !ok {
		t.Fatal("the balance was not folded")
	}
	if !cc.Stated() {
		t.Fatal("a producer that listed every entry type it feeds was reported as saying nothing")
	}
	if cc.Incomplete() {
		t.Fatalf("a fully fed producer was reported incomplete, omitting %v", cc.OmittedEntryTypes)
	}
}
