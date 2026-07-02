package execution

import (
	"context"
	"errors"
	"testing"
	"time"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
)

// scriptSession is a fake FIXSession: SubmitNewOrder returns a channel preloaded
// with a scripted report sequence (closed after the last), so the venue's
// async→sync fold is driven deterministically with no FIX engine.
type scriptSession struct {
	script  []ExecutionReport
	block   bool // if true, return a never-closing stream (ctx-cancel test)
	cancels int
	amends  int
}

func (s *scriptSession) SubmitNewOrder(_ context.Context, req OrderRequest) (<-chan ExecutionReport, error) {
	ch := make(chan ExecutionReport, len(s.script))
	for _, r := range s.script {
		r.ClOrdID = req.ClOrdID
		ch <- r
	}
	if !s.block {
		close(ch)
	}
	return ch, nil
}
func (s *scriptSession) Cancel(context.Context, OrderRequest) error  { s.cancels++; return nil }
func (s *scriptSession) Replace(context.Context, OrderRequest) error { s.amends++; return nil }

func limitOrder() *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId:        "o1",
		InstrumentId:   "AAPL",
		Side:           orderpb.Side_SIDE_BUY,
		OrderType:      orderpb.OrderType_ORDER_TYPE_LIMIT,
		LimitPrice:     d(15000, -2),
		LeavesQuantity: d(100, 0),
	}
}

func fixVenue(sess FIXSession) *FIXVenue {
	return NewFIXVenue("XNAS", sess,
		WithFIXClock(func() time.Time { return time.Unix(0, 0) }),
		WithFIXIDGen(func() string { return "clord-1" }))
}

func TestFIXVenueAggregatesPartialFills(t *testing.T) {
	sess := &scriptSession{script: []ExecutionReport{
		{ExecType: ExecNew},
		{ExecType: ExecPartialFill, ExecID: "e1", LastQty: d(40, 0), LastPx: d(15000, -2)},
		{ExecType: ExecFill, ExecID: "e2", LastQty: d(60, 0), LastPx: d(15000, -2)},
	}}
	fills, err := fixVenue(sess).Execute(context.Background(), limitOrder())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(fills) != 2 {
		t.Fatalf("got %d fills want 2 (partial + fill)", len(fills))
	}
	if fills[0].GetFillId() != "e1" || fills[1].GetFillId() != "e2" {
		t.Errorf("fill ids = %s,%s want e1,e2", fills[0].GetFillId(), fills[1].GetFillId())
	}
	if fills[0].GetVenue() != "XNAS" {
		t.Errorf("venue = %s want XNAS", fills[0].GetVenue())
	}
}

func TestFIXVenueRejectIsError(t *testing.T) {
	sess := &scriptSession{script: []ExecutionReport{
		{ExecType: ExecNew},
		{ExecType: ExecRejected, Text: "risk limit breached"},
	}}
	_, err := fixVenue(sess).Execute(context.Background(), limitOrder())
	if err == nil {
		t.Fatal("expected an error on a FIX reject")
	}
}

// A canceled/done-for-day order returns the fills gathered so far without error
// (it rested and stopped working).
func TestFIXVenueCanceledReturnsPartial(t *testing.T) {
	sess := &scriptSession{script: []ExecutionReport{
		{ExecType: ExecPartialFill, ExecID: "e1", LastQty: d(30, 0), LastPx: d(15000, -2)},
		{ExecType: ExecCanceled, Text: "user cancel"},
	}}
	fills, err := fixVenue(sess).Execute(context.Background(), limitOrder())
	if err != nil {
		t.Fatalf("cancel after partial should not error: %v", err)
	}
	if len(fills) != 1 || fills[0].GetFillId() != "e1" {
		t.Fatalf("got %v want the single partial fill e1", fills)
	}
}

func TestFIXVenueContextCancel(t *testing.T) {
	sess := &scriptSession{block: true, script: []ExecutionReport{{ExecType: ExecNew}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := fixVenue(sess).Execute(ctx, limitOrder())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v want context.Canceled", err)
	}
}

func TestFIXVenueCancelAndReplace(t *testing.T) {
	sess := &scriptSession{}
	v := fixVenue(sess)
	if err := v.Cancel(context.Background(), "o1", "clord-0"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := v.Replace(context.Background(), "o1", "clord-0", d(50, 0), d(14900, -2)); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if sess.cancels != 1 || sess.amends != 1 {
		t.Errorf("session calls = cancel:%d amend:%d want 1,1", sess.cancels, sess.amends)
	}
}

func TestReconcileDropCopy(t *testing.T) {
	ours := []*orderpb.Fill{{FillId: "e1"}, {FillId: "e2"}}
	// e2 confirmed by drop-copy; e1 is ours-only; e3 is drop-copy-only.
	breaks := ReconcileDropCopy(ours, []string{"e2", "e3"})
	if len(breaks) != 2 {
		t.Fatalf("got %d breaks want 2", len(breaks))
	}
	seen := map[string]bool{}
	for _, b := range breaks {
		seen[b.ExecID] = b.OnlyDropCopy
	}
	if v, ok := seen["e1"]; !ok || v {
		t.Errorf("e1 should be an ours-only break (OnlyDropCopy=false)")
	}
	if v, ok := seen["e3"]; !ok || !v {
		t.Errorf("e3 should be a drop-copy-only break (OnlyDropCopy=true)")
	}
	// A clean reconciliation returns nil.
	if b := ReconcileDropCopy(ours, []string{"e1", "e2"}); b != nil {
		t.Errorf("clean reconcile should be nil, got %v", b)
	}
}
