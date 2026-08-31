package execution

import (
	"context"
	"errors"
	"fmt"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GRPCVenue is an exchange that lives in ANOTHER PROCESS (INFRA-M7a).
//
// It is deliberately just one more implementation of the existing Venue and
// Closer interfaces — not a new routing layer beside them. The Router, the
// command handler, and the aggregate do not know or care that this venue is
// across a network: that is the whole point of the seam they were built on
// (OMS-01c, the DEBT-02 inject-the-side-effect stance).
//
// What changes is the blast radius. Today the connectors compile INTO the OMS
// behind build tags, so vendor client code, request signing, and exchange
// websocket loops share an address space with the process that owns order state.
// Behind this type they do not: the adapter is a separate deployment, reached
// over mTLS, and the OMS links no vendor code at all.
//
// The wire carries order.v1 types (OrderState out, Fill back), so prices and
// sizes stay common.v1.Decimal end to end. No float, and no second encoding of a
// price, can enter through this boundary — there is nowhere to put one.
type GRPCVenue struct {
	mic     string
	account string
	tenant  string
	client  venuepb.VenueAdapterServiceClient
}

// NewGRPCVenue returns a Venue backed by an out-of-process adapter on conn. The
// caller owns the connection (and its mTLS credentials); tenant is stamped on
// every request so the adapter can scope its credentials and anything it emits.
func NewGRPCVenue(mic, account string, conn *grpc.ClientConn, tenant string) *GRPCVenue {
	return &GRPCVenue{
		mic:     mic,
		account: account,
		tenant:  tenant,
		client:  venuepb.NewVenueAdapterServiceClient(conn),
	}
}

// MIC returns the venue code stamped on routing + fills.
func (v *GRPCVenue) MIC() string { return v.mic }

// Account is the exchange account behind this adapter's API credential, as declared
// in OMS_VENUE_ENDPOINTS — and CHECKED against the adapter itself at dial time
// (SOV-02a).
//
// It used to be taken on trust: the adapter held the key and had no way to report
// which account the key belonged to, so a mis-declared endpoint posted fills to the
// wrong account's ledger rows while the exchange debited the right one. The OMS now
// asks (Describe) and refuses to start when the answer disagrees, so by the time this
// value is readable it is the account the ADAPTER says it holds, not merely the one a
// manifest claimed.
func (v *GRPCVenue) Account() string { return v.account }

// Execute works st at the remote venue and returns the fills it produced.
//
// Two outcomes are carefully NOT conflated:
//
//   - no fills, no error — a resting order that did not trade. Valid. The order
//     stays working.
//   - an error — the adapter could not work the order (unreachable, rejected,
//     timed out). NO fills are returned, ever. Fabricating a fill here would book
//     a trade that never happened, which is the one thing this system must never
//     do (the "never fabricate — degrade or emit a correcting FACT" rule).
func (v *GRPCVenue) Execute(ctx context.Context, st *orderpb.OrderState) ([]*orderpb.Fill, error) {
	if st == nil {
		return nil, errors.New("execution: nil order state")
	}
	resp, err := v.client.Execute(ctx, &venuepb.ExecuteRequest{
		TenantId: v.tenant,
		State:    st,
	})
	if err != nil {
		return nil, fmt.Errorf("venue %s: execute order %s: %w", v.mic, st.GetOrderId(), err)
	}
	return resp.GetFills(), nil
}

// CancelOrder withdraws st at the remote venue, addressed by st.order_id — the
// deterministic clOrdId the submit used, so the cancel is idempotent there.
//
// A nil return means the venue CONFIRMED the withdrawal. Any error — including an
// ambiguous deadline — means the close is STILL IN FLIGHT and belongs to the
// healing watchdog (EXEC-M4c). Swallowing a timeout into a nil here would tell
// the OMS an order was withdrawn while it is still live at the exchange, which is
// how a "cancelled" order fills anyway.
func (v *GRPCVenue) CancelOrder(ctx context.Context, st *orderpb.OrderState) error {
	if st == nil {
		return errors.New("execution: nil order state")
	}
	if _, err := v.client.CancelOrder(ctx, &venuepb.CancelOrderRequest{
		TenantId: v.tenant,
		State:    st,
	}); err != nil {
		return fmt.Errorf("venue %s: cancel order %s: %w", v.mic, st.GetOrderId(), err)
	}
	return nil
}

// QueryOrder asks the adapter what the exchange did with st — the Querier
// capability, over the wire (#920).
//
// BEFORE THIS, SimVenue WAS THE ONLY Querier IN THE PLATFORM. venue.v1 carried
// no query RPC, so `Service.resume`'s `venue.(execution.Querier)` assertion
// failed for every real deployment and EVERY interrupted ROUTED order
// quarantined — the platform's answer to "what happened to the orders in flight
// when the OMS restarted" was "freeze them all and fetch a human". Most of
// order.Reconcile's policy table was reachable only against a simulator.
//
// # THE THREE ANSWERS, AND WHY A FOURTH WOULD BE A DUPLICATE TRADE
//
// This method returns an error for exactly one thing: THE QUESTION COULD NOT BE
// ASKED. The adapter was unreachable, the deadline expired, the exchange
// refused for weight. resume() propagates that, the delivery is NAKed, and the
// broker asks again later.
//
// It returns OrderViewUnknown only when the far end said, positively, that the
// VENUE HAS NO SUCH ORDER. Reconcile turns that into ActionRedrive when the OMS
// holds no venue ack, which places the order at a real exchange. Every failure
// mode above must therefore stay on the error return or land on
// OrderViewIndeterminate, and the mapping below is written as an explicit
// switch with a fail-closed default rather than an integer conversion for that
// reason: a value this build has not been taught must freeze the order, not
// take the meaning of whatever Go constant shares its number.
//
// # Unimplemented IS NOT A TRANSIENT FAULT
//
// An adapter that predates this RPC, or a connector that cannot ask its
// exchange, answers codes.Unimplemented. Retrying can never fix that, so
// returning it as an error would replace a quarantine — a terminal, visible,
// operator-resolvable state — with an endless redelivery loop ending in a DLQ.
// It maps to INDETERMINATE, which reproduces EXACTLY the behaviour the OMS had
// when GRPCVenue implemented no Querier at all. That is what makes it safe to
// upgrade the OMS ahead of its adapters.
func (v *GRPCVenue) QueryOrder(ctx context.Context, st *orderpb.OrderState) (OrderView, error) {
	if st == nil {
		return OrderView{}, errors.New("execution: nil order state")
	}
	resp, err := v.client.QueryOrder(ctx, &venuepb.QueryOrderRequest{
		TenantId: v.tenant,
		State:    st,
	})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			return OrderView{
				State: OrderViewIndeterminate,
				Reason: fmt.Sprintf(
					"venue adapter %s serves no QueryOrder, so nothing can establish whether it holds "+
						"order %s. Re-driving might trade the fund twice and abandoning might strand a "+
						"live exchange order; neither is a guess this platform will make",
					v.mic, st.GetOrderId()),
			}, nil
		}
		// The question could not be asked. NOT an answer, and emphatically not
		// OrderViewUnknown — collapsing the two turns a network blip into a
		// re-driven order.
		return OrderView{}, fmt.Errorf("venue %s: query order %s: %w", v.mic, st.GetOrderId(), err)
	}
	return OrderView{
		State:  orderViewState(resp.GetState()),
		Fills:  resp.GetFills(),
		Reason: resp.GetReason(),
	}, nil
}

// orderViewState maps the wire verdict onto the in-process one.
//
// EXHAUSTIVE, AND FAIL-CLOSED BY DEFAULT. The two enums agree value for value
// today, so `OrderViewState(resp.GetState())` would compile and pass every test
// — and it would silently give a NEW wire value the meaning of whatever Go
// constant happens to share its number. If a future venue.v1 adds a value at 7
// and this package later adds an unrelated constant at 7, an unrecognised
// exchange verdict becomes an authoritative one with no code change anywhere.
// The switch cannot do that: anything unrecognised is INDETERMINATE, which
// quarantines.
func orderViewState(s venuepb.OrderViewState) OrderViewState {
	switch s {
	case venuepb.OrderViewState_ORDER_VIEW_STATE_UNKNOWN:
		return OrderViewUnknown
	case venuepb.OrderViewState_ORDER_VIEW_STATE_WORKING:
		return OrderViewWorking
	case venuepb.OrderViewState_ORDER_VIEW_STATE_PARTIALLY_FILLED:
		return OrderViewPartiallyFilled
	case venuepb.OrderViewState_ORDER_VIEW_STATE_FILLED:
		return OrderViewFilled
	case venuepb.OrderViewState_ORDER_VIEW_STATE_REJECTED:
		return OrderViewRejected
	default:
		// ORDER_VIEW_STATE_INDETERMINATE, ORDER_VIEW_STATE_UNSPECIFIED (the
		// adapter set no field) and anything this build has not been taught.
		return OrderViewIndeterminate
	}
}

// OrderViewStateProto is the same mapping the other way round, for a venue
// adapter serving QueryOrder. It lives here rather than in the adapter's own
// package so the two directions cannot drift into disagreeing about which wire
// value means "the venue positively has no such order" — the one value that
// authorizes a second placement.
func OrderViewStateProto(s OrderViewState) venuepb.OrderViewState {
	switch s {
	case OrderViewUnknown:
		return venuepb.OrderViewState_ORDER_VIEW_STATE_UNKNOWN
	case OrderViewWorking:
		return venuepb.OrderViewState_ORDER_VIEW_STATE_WORKING
	case OrderViewPartiallyFilled:
		return venuepb.OrderViewState_ORDER_VIEW_STATE_PARTIALLY_FILLED
	case OrderViewFilled:
		return venuepb.OrderViewState_ORDER_VIEW_STATE_FILLED
	case OrderViewRejected:
		return venuepb.OrderViewState_ORDER_VIEW_STATE_REJECTED
	default:
		// OrderViewIndeterminate and any state this build has not been taught.
		// Never UNSPECIFIED: an adapter that DID establish "I cannot tell" has
		// said something, and an operator reading the quarantine record must be
		// able to tell that from a field nobody set.
		return venuepb.OrderViewState_ORDER_VIEW_STATE_INDETERMINATE
	}
}

// OwnsCloseTracking marks this venue as SelfHealing: the adapter on the far end
// tracks the close in its OWN registry the moment its CancelOrder handler runs,
// and its OWN reconciler heals it. The OMS must not track it too — see SelfHealing.
func (v *GRPCVenue) OwnsCloseTracking() {}

// Compile-time assertions. GRPCVenue must be BOTH: a venue that is not a Closer
// silently downgrades every cancel to a ledger-only entry while the order stays
// live at the exchange.
var (
	_ Venue       = (*GRPCVenue)(nil)
	_ Closer      = (*GRPCVenue)(nil)
	_ SelfHealing = (*GRPCVenue)(nil)
	// AND A Querier (#920). Without it, Service.resume quarantines every
	// interrupted ROUTED order at every real venue, and order.Reconcile's policy
	// table is exercised only against the simulator.
	_ Querier = (*GRPCVenue)(nil)
)
