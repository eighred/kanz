package order

import (
	"context"
	"strings"
	"testing"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
)

// ONE DISAGREEMENT, ONE ANSWER (#808).
//
// # What was wrong
//
// An ApplyFill refusal reached the platform on two paths and got two different
// answers. In adopt() — the RECOVERY path — it quarantines: "the venue's record
// and ours describe different orders under one id", which is the correct answer,
// because re-driving cannot resolve a disagreement about what the order IS.
//
// In work() — the LIVE path — the identical refusal produced an ERROR log line
// and a `break`. work() then returned nil, and admission published an ACCEPTED
// CommandOutcome.
//
// # What that cost
//
// A venue over-fill on the live path is a position THE FUND HOLDS with no
// ORDER_FILLED FACT, no projection, no ledger entry and no alertable counter —
// discoverable only by reading logs, while the order reports ACCEPTED. Risk,
// compliance and the IBOR are all measuring a book that is missing the
// execution, and the first thing that notices is reconciliation against the
// venue, if it runs.
//
// The `break` was also the wrong stop taken on its own terms: it abandoned every
// REMAINING fill in the same view, so a refusal on the first silently dropped
// the second and third as well.
//
// # Latent, and under a named condition
//
// SimVenue fills full-leaves-or-nothing and cannot produce this. A real
// connector re-executing after a partial can — which is the population this
// exists for, and why the fixture below is a venue that over-fills deliberately.

// overfillingVenue returns a fill LARGER than the order's open quantity — what a
// connector does when it re-executes an order the exchange has already partly
// filled, and the one thing the aggregate refuses outright.
type overfillingVenue struct {
	mic   string
	fills []*orderpb.Fill
}

func (v *overfillingVenue) MIC() string     { return v.mic }
func (v *overfillingVenue) Account() string { return "acct-808" }

func (v *overfillingVenue) Execute(_ context.Context, st *orderpb.OrderState) ([]*orderpb.Fill, error) {
	if len(v.fills) > 0 {
		return v.fills, nil
	}
	// Twice the order's open quantity, at the limit price.
	over := &commonpb.Decimal{
		Coefficient: st.GetLeavesQuantity().GetCoefficient() * 2,
		Exponent:    st.GetLeavesQuantity().GetExponent(),
	}
	return []*orderpb.Fill{{
		FillId: "f-808-over", OrderId: st.GetOrderId(), InstrumentId: st.GetInstrumentId(),
		Side: st.GetSide(), Quantity: over, Price: st.GetLimitPrice(), Venue: v.mic,
		ExecutedAt: timestamppb.New(t0),
	}}, nil
}

// overfillService admits one order through a venue that over-fills it.
func overfillService(t *testing.T, fb *fakeBus, venue execution.Venue) (*Service, *MemoryStore) {
	t.Helper()
	store := NewMemoryStore()
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter([]execution.Venue{venue}), nil, nil, WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, store
}

// THE ISSUE'S OWN "VERIFIED WHEN": a venue returning a fill larger than leaves on
// the LIVE path leaves the order QUARANTINED, not ACCEPTED.
func TestALiveOverfillQuarantinesRatherThanBeingLogged(t *testing.T) {
	fb := &fakeBus{}
	svc, store := overfillService(t, fb, &overfillingVenue{mic: "BINANCE"})

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, limitOrder(d(100, 0), d(1025, -2)))); err != nil {
		t.Fatalf("submit: %v", err)
	}

	st, _, err := store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	q := st.GetQuarantine()
	if q == nil {
		t.Fatalf("the order is NOT quarantined (status=%v).\n\n"+
			"This is #808. The venue reported a fill this order cannot accept, and the live path "+
			"answered with a log line where the recovery path freezes. The fund holds a position "+
			"with no ORDER_FILLED FACT, no projection, no ledger entry and no alertable counter — "+
			"discoverable only by reading logs — while risk, compliance and the IBOR all measure "+
			"a book missing the execution.", st.GetStatus())
	}
	// The wording is the recovery path's, because the disagreement is the same
	// one: only a human comparing against the venue's own order history can say
	// which record is right.
	if !strings.Contains(q.GetReason(), "describe different orders under one id") {
		t.Errorf("quarantine reason = %q, want the disagreement wording adopt() uses — the two "+
			"paths differ in how they were reached, not in what the refusal MEANS", q.GetReason())
	}

	// AND NO FILL FACT WENT OUT. A fill the aggregate refused must not be
	// announced as one that happened.
	if fb.last(EventTypeFilled) != nil || fb.last(EventTypePartiallyFilled) != nil {
		t.Errorf("a fill FACT was published for a fill the aggregate refused (emitted: %v)", fb.types())
	}
}

// AND THE ORDER IS NOT REPORTED ACCEPTED.
//
// The outcome is what the CALLER sees. An ACCEPTED CommandOutcome over a
// quarantined order tells a strategy its order is working while the platform has
// frozen it for a human — the two answers cannot both be given.
func TestALiveOverfillIsNotAnsweredAccepted(t *testing.T) {
	fb := &fakeBus{}
	svc, _ := overfillService(t, fb, &overfillingVenue{mic: "BINANCE"})

	err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, limitOrder(d(100, 0), d(1025, -2))))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	if oc, ok := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome); ok {
		if oc.GetStatus() == commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED {
			t.Fatalf("the caller was told the order EXECUTED while it is quarantined — a strategy "+
				"reading this believes its order is working, and the platform has frozen it for a "+
				"human (outcome=%v/%q)", oc.GetStatus(), oc.GetErrorCode())
		}
	}
}

// NON-VACUITY. A venue whose fills FIT is unaffected — the freeze is about the
// disagreement, not about the live path being switched off. Without this, both
// cases above pass on a work() that quarantines every order it routes.
func TestAFillThatFitsIsStillFoldedOnTheLivePath(t *testing.T) {
	fb := &fakeBus{}
	fitting := &overfillingVenue{mic: "BINANCE", fills: []*orderpb.Fill{{
		FillId: "f-808-ok", OrderId: "o1", InstrumentId: "AAPL",
		Side: orderpb.Side_SIDE_BUY, Quantity: d(100, 0), Price: d(1025, -2), Venue: "BINANCE",
		ExecutedAt: timestamppb.New(t0),
	}}}
	svc, store := overfillService(t, fb, fitting)

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, limitOrder(d(100, 0), d(1025, -2)))); err != nil {
		t.Fatalf("submit: %v", err)
	}

	st, _, err := store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.GetQuarantine() != nil {
		t.Fatalf("an order whose venue fill FITS was quarantined (%q) — the live path is now "+
			"freezing orders that are working correctly", st.GetQuarantine().GetReason())
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("status = %v, want FILLED", st.GetStatus())
	}
	if fb.last(EventTypeFilled) == nil {
		t.Fatalf("no fill FACT for a fill that fitted (emitted: %v)", fb.types())
	}
}
