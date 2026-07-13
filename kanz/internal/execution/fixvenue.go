package execution

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// FIX venue adapter (PARITY-04c). A real FIX 4.4/5.0 session is async — a
// NewOrderSingle is answered by a STREAM of ExecutionReports over time — while
// execution.Venue.Execute is synchronous. FIXVenue bridges the two: it submits
// the order and folds the report stream into []*orderpb.Fill until the order
// reaches a terminal state (fully filled, canceled, done-for-day, or rejected).
//
// The FIX engine itself is a seam (FIXSession): the protocol LOGIC here —
// order→NewOrderSingle mapping, ExecutionReport→Fill decoding, partial-fill
// aggregation, cancel/amend, drop-copy reconciliation — is transport-free and
// fully tested against a scripted fake session, exactly the PARITY-01b–d
// "the vendor SDK is itself the seam" stance. The concrete quickfix/go binding
// (session management, wire codec, sequence numbers, heartbeats) implements
// FIXSession at the composition root; SimVenue is retained as the in-process
// test seam.

// RequestKind distinguishes the order-management messages the venue sends.
type RequestKind int

const (
	RequestNew RequestKind = iota
	RequestCancel
	RequestReplace
)

// OrderRequest is the venue's transport-free view of a FIX order message
// (NewOrderSingle / OrderCancelRequest / OrderCancelReplaceRequest). The concrete
// session encodes it onto the wire.
type OrderRequest struct {
	Kind        RequestKind
	ClOrdID     string // client order id assigned by the venue
	OrigClOrdID string // the order being canceled/replaced (RequestCancel/Replace)
	OrderID     string // the platform order_id, for stamping fills
	Instrument  string
	Side        orderpb.Side
	Quantity    *commonpb.Decimal
	OrdType     orderpb.OrderType
	LimitPrice  *commonpb.Decimal
}

// ExecType mirrors the FIX ExecType(150) values the venue branches on.
type ExecType int

const (
	ExecNew         ExecType = iota // 0 — accepted, working
	ExecPartialFill                 // F with leaves > 0
	ExecFill                        // F with leaves == 0 (fully filled)
	ExecCanceled                    // 4
	ExecReplaced                    // 5
	ExecRejected                    // 8
	ExecDoneForDay                  // 3 — resting order expired
)

// ExecutionReport is the venue's view of a FIX ExecutionReport(35=8). LastQty /
// LastPx describe THIS fill (on a fill report); ExecID is the venue's execution
// id, used as the fill_id.
type ExecutionReport struct {
	ClOrdID  string
	ExecType ExecType
	ExecID   string
	LastQty  *commonpb.Decimal
	LastPx   *commonpb.Decimal
	Text     string // reject/cancel reason
}

// Terminal reports whether this report ends the order's working life (no more
// reports follow).
func (r ExecutionReport) Terminal() bool {
	switch r.ExecType {
	case ExecFill, ExecCanceled, ExecRejected, ExecDoneForDay:
		return true
	default:
		return false
	}
}

// FIXSession is the seam to a FIX engine. SubmitNewOrder sends a NewOrderSingle
// and returns the stream of ExecutionReports for it (closed when the order is
// terminal). Cancel/Replace send the corresponding order-management message.
type FIXSession interface {
	SubmitNewOrder(ctx context.Context, req OrderRequest) (<-chan ExecutionReport, error)
	Cancel(ctx context.Context, req OrderRequest) error
	Replace(ctx context.Context, req OrderRequest) error
}

// FIXVenue works orders over a FIXSession.
type FIXVenue struct {
	mic     string
	account string
	session FIXSession
	now     func() time.Time
	newID   func() string
}

// FIXOption customizes a FIXVenue (test clock / id generator).
type FIXOption func(*FIXVenue)

// WithFIXClock overrides the fill timestamp source.
func WithFIXClock(now func() time.Time) FIXOption { return func(v *FIXVenue) { v.now = now } }

// WithFIXIDGen overrides the ClOrdID generator.
func WithFIXIDGen(f func() string) FIXOption { return func(v *FIXVenue) { v.newID = f } }

// NewFIXVenue builds a FIX venue with the given MIC over session.
func NewFIXVenue(mic string, session FIXSession, opts ...FIXOption) *FIXVenue {
	v := &FIXVenue{mic: mic, account: "fix:" + mic, session: session, now: time.Now, newID: uuid.NewString}
	for _, o := range opts {
		o(v)
	}
	return v
}

var _ Venue = (*FIXVenue)(nil)

// MIC returns the venue code.
func (v *FIXVenue) MIC() string { return v.mic }

// Account is the exchange account behind this FIX session's credentials. FIX sessions
// are per-account by construction (the SenderCompID authenticates one account), so
// this defaults to the session's venue and should be set to the real account id when
// this venue is used against a live broker.
func (v *FIXVenue) Account() string { return v.account }

// WithFIXAccount names the exchange account this FIX session trades.
func WithFIXAccount(a string) FIXOption { return func(v *FIXVenue) { v.account = a } }

// Execute submits st as a NewOrderSingle and aggregates the ExecutionReport
// stream into fills until the order is terminal. Partial fills accumulate; a
// full fill closes the order; a cancel/done-for-day returns the fills gathered so
// far (the order rested and stopped working); a reject returns an error with the
// venue text. Honors ctx cancellation, returning fills gathered so far.
func (v *FIXVenue) Execute(ctx context.Context, st *orderpb.OrderState) ([]*orderpb.Fill, error) {
	if st == nil {
		return nil, errors.New("execution: nil order state")
	}
	req := OrderRequest{
		Kind:       RequestNew,
		ClOrdID:    v.newID(),
		OrderID:    st.GetOrderId(),
		Instrument: st.GetInstrumentId(),
		Side:       st.GetSide(),
		Quantity:   st.GetLeavesQuantity(),
		OrdType:    st.GetOrderType(),
		LimitPrice: st.GetLimitPrice(),
	}
	stream, err := v.session.SubmitNewOrder(ctx, req)
	if err != nil {
		return nil, err
	}
	var fills []*orderpb.Fill
	for {
		select {
		case <-ctx.Done():
			return fills, ctx.Err()
		case rep, ok := <-stream:
			if !ok {
				return fills, nil // stream closed ⇒ terminal
			}
			switch rep.ExecType {
			case ExecPartialFill, ExecFill:
				fills = append(fills, v.fill(st, rep))
				if rep.ExecType == ExecFill {
					return fills, nil
				}
			case ExecRejected:
				return fills, fmt.Errorf("execution: FIX order rejected on %s: %s", v.mic, rep.Text)
			case ExecCanceled, ExecDoneForDay:
				return fills, nil
			}
		}
	}
}

func (v *FIXVenue) fill(st *orderpb.OrderState, rep ExecutionReport) *orderpb.Fill {
	return &orderpb.Fill{
		FillId:       rep.ExecID,
		OrderId:      st.GetOrderId(),
		InstrumentId: st.GetInstrumentId(),
		Side:         st.GetSide(),
		Quantity:     rep.LastQty,
		Price:        rep.LastPx,
		Venue:        v.mic,
		// The account this fill settled against — where the collateral actually moved.
		VenueAccountId: v.account,
		ExecutedAt:     timestamppb.New(v.now().UTC()),
	}
}

// Cancel sends an OrderCancelRequest for the working order origClOrdID.
func (v *FIXVenue) Cancel(ctx context.Context, orderID, origClOrdID string) error {
	return v.session.Cancel(ctx, OrderRequest{Kind: RequestCancel, ClOrdID: v.newID(), OrigClOrdID: origClOrdID, OrderID: orderID})
}

// Replace sends an OrderCancelReplaceRequest (amend) changing qty/price on the
// working order origClOrdID.
func (v *FIXVenue) Replace(ctx context.Context, orderID, origClOrdID string, newQty, newPrice *commonpb.Decimal) error {
	return v.session.Replace(ctx, OrderRequest{
		Kind:        RequestReplace,
		ClOrdID:     v.newID(),
		OrigClOrdID: origClOrdID,
		OrderID:     orderID,
		Quantity:    newQty,
		LimitPrice:  newPrice,
	})
}

// DropCopyBreak is a discrepancy between the fills the venue reported to us and
// the venue's independent drop-copy feed — an execution the two sides disagree
// on, the reconciliation signal a real OMS must surface.
type DropCopyBreak struct {
	ExecID string
	// OnlyDropCopy is true when the exec id appears only in the drop-copy feed
	// (we never saw the fill), false when it appears only in our fills (the
	// drop-copy did not confirm it).
	OnlyDropCopy bool
}

// ReconcileDropCopy compares our recorded fills against the venue's drop-copy
// exec ids and returns every exec id the two sides disagree on — a fill we hold
// that drop-copy never confirmed, or a drop-copy execution we never received.
// A clean reconciliation returns nil.
func ReconcileDropCopy(ourFills []*orderpb.Fill, dropCopyExecIDs []string) []DropCopyBreak {
	ours := make(map[string]struct{}, len(ourFills))
	for _, f := range ourFills {
		ours[f.GetFillId()] = struct{}{}
	}
	dc := make(map[string]struct{}, len(dropCopyExecIDs))
	for _, id := range dropCopyExecIDs {
		dc[id] = struct{}{}
	}
	var breaks []DropCopyBreak
	for id := range ours {
		if _, ok := dc[id]; !ok {
			breaks = append(breaks, DropCopyBreak{ExecID: id, OnlyDropCopy: false})
		}
	}
	for id := range dc {
		if _, ok := ours[id]; !ok {
			breaks = append(breaks, DropCopyBreak{ExecID: id, OnlyDropCopy: true})
		}
	}
	return breaks
}
