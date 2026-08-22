package balancerecon

import (
	"context"
	"math"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	"google.golang.org/protobuf/proto"
)

// A DROPPED BALANCE ANNOUNCEMENT MUST LEAVE A TRACE (#622).
//
// View.Handle returned nil on two paths with no log and no metric: a payload that
// did not decode, and one carrying an out-of-domain exponent. The ACK is right on
// both — nacking replays the same bad bytes forever — but the silence was not.
//
// This is the one genuinely silent swallow on the non-OMS surface. Every peer
// taking the same ack decision says something: the compliance monitor logs at
// Error, market-data returns the error. Here the balance simply aged out to
// UNKNOWN and reconciliation skipped the account, with nothing anywhere pointing
// at why — and "unknown" is a state an operator then has to explain.
//
// lineage's own composition root states the rule this broke:
//
//	A DROPPED AUDIT RECORD MUST BE COUNTED, NOT SWALLOWED.

func TestAnUndecodablePayloadIsCountedAndNotSilent(t *testing.T) {
	var reasons []string
	v := NewView("acct-1", WithViewDropObserver(func(reason string) {
		reasons = append(reasons, reason)
	}))

	if err := v.Handle(context.Background(), nil, []byte{0xff, 0xff, 0xff, 0xff}); err != nil {
		t.Fatalf("poison must still be ACKED, got %v", err)
	}

	if len(reasons) != 1 || reasons[0] != DropUndecodable {
		t.Fatalf("observer saw %v, want exactly [%s]", reasons, DropUndecodable)
	}
}

// The #95 exponent refusal is a SEPARATE reason, because it sends an operator
// somewhere else: a decode failure is a corrupt or mis-typed publisher, an
// out-of-domain exponent is a producer emitting a number this platform will not
// materialise.
func TestAnOutOfDomainExponentIsCountedUnderItsOwnReason(t *testing.T) {
	var reasons []string
	v := NewView("acct-1", WithViewDropObserver(func(reason string) {
		reasons = append(reasons, reason)
	}))

	// dec.FromProto materialises 10^abs(exponent); this is the value #95 refuses
	// before any number is read.
	payload, err := proto.Marshal(&accountingpb.PortfolioCashBalance{
		PortfolioId:  "PF1",
		BaseCurrency: "USD",
		ByVenueAccount: []*accountingpb.VenueAccountCash{{
			VenueAccountId: "acct-1",
			Asset:          "USD",
			Amount:         &commonpb.Decimal{Coefficient: 1, Exponent: math.MaxInt32},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := v.Handle(context.Background(), nil, payload); err != nil {
		t.Fatalf("an out-of-domain announcement must still be ACKED, got %v", err)
	}
	if len(reasons) != 1 || reasons[0] != DropOutOfDomain {
		t.Fatalf("observer saw %v, want exactly [%s]", reasons, DropOutOfDomain)
	}
	// And nothing was folded from it.
	if _, ok := v.Balance("USD"); ok {
		t.Fatal("an out-of-domain announcement was folded into the view")
	}
}

// A GOOD ANNOUNCEMENT COUNTS NOTHING. A counter that fires on the healthy path
// is a counter an operator learns to ignore.
func TestAValidAnnouncementDropsNothing(t *testing.T) {
	var reasons []string
	v := NewView("acct-1",
		WithViewClock(func() time.Time { return at0 }),
		WithViewDropObserver(func(reason string) { reasons = append(reasons, reason) }))

	payload := announcement(t, at0, &accountingpb.VenueAccountCash{
		VenueAccountId: "acct-1", Asset: "USD",
		Amount: &commonpb.Decimal{Coefficient: 1000, Exponent: 0},
	})
	if err := v.Handle(context.Background(), nil, payload); err != nil {
		t.Fatal(err)
	}
	if len(reasons) != 0 {
		t.Fatalf("a valid announcement reported drops: %v", reasons)
	}
}

// The seam is OPTIONAL and its absence must not panic — a deployment that
// forgets the counter still acks, still logs, and still folds.
func TestTheDropObserverIsOptional(t *testing.T) {
	v := NewView("acct-1")
	if err := v.Handle(context.Background(), nil, []byte{0xff}); err != nil {
		t.Fatalf("no observer wired must not change the ack: %v", err)
	}
}
