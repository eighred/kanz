package execution

import (
	"context"
	"errors"
	"fmt"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"google.golang.org/grpc"
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
)
