package execution

import (
	"context"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

type describedTestVenue struct{ Venue }

func (describedTestVenue) QueryOrder(context.Context, *orderpb.OrderState) (OrderView, error) {
	return OrderView{State: OrderViewCancelled}, nil
}
func (describedTestVenue) CancelOrder(context.Context, *orderpb.OrderState) error { return nil }
func (describedTestVenue) OwnsCloseTracking()                                     {}
func (v describedTestVenue) Describe(context.Context) (VenueIdentity, error) {
	return VenueIdentity{MIC: v.MIC(), Account: v.Account(), Proof: AccountProof{Verified: true, ExchangeAccountID: "exchange-account"}}, nil
}

func declaredTestAdapter(v Venue) Venue {
	return WithMarginModes(WithTimeInForce(WithOrderTypes(v,
		[]orderpb.OrderType{orderpb.OrderType_ORDER_TYPE_LIMIT}),
		[]orderpb.TimeInForce{orderpb.TimeInForce_TIME_IN_FORCE_GTC}),
		[]orderpb.MarginMode{orderpb.MarginMode_MARGIN_MODE_CROSS})
}

func TestProductionDeclarationStackPreservesAdmissionAndOperations(t *testing.T) {
	base := describedTestVenue{NewSimVenue("XSIM")}
	router := NewRouter([]Venue{declaredTestAdapter(base)})
	if router.SupportsOrderType("XSIM", orderpb.OrderType_ORDER_TYPE_MARKET) || router.SupportsTimeInForce("XSIM", orderpb.TimeInForce_TIME_IN_FORCE_IOC) {
		t.Fatal("outer declaration erased an inner admission refusal")
	}
	if !router.SupportsOrderType("XSIM", orderpb.OrderType_ORDER_TYPE_LIMIT) || !router.SupportsTimeInForce("XSIM", orderpb.TimeInForce_TIME_IN_FORCE_GTC) {
		t.Fatal("declared supported operation refused")
	}
	for _, targeted := range []bool{false, true} {
		st := &orderpb.OrderState{OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, TimeInForce: orderpb.TimeInForce_TIME_IN_FORCE_GTC, MarginMode: orderpb.MarginMode_MARGIN_MODE_CROSS}
		if targeted {
			st.Venue = "XSIM"
		}
		v, err := router.Route(st)
		if err != nil {
			t.Fatal(err)
		}
		q, ok := v.(Querier)
		if !ok {
			t.Fatal("declaration hid query capability")
		}
		view, err := q.QueryOrder(context.Background(), st)
		if err != nil || view.State != OrderViewCancelled {
			t.Fatalf("query: %v %v", view, err)
		}
		if _, ok := v.(Closer); !ok {
			t.Fatal("declaration hid cancellation capability")
		}
		if _, ok := v.(SelfHealing); !ok {
			t.Fatal("declaration erased close ownership")
		}
		identity, ok := v.(interface {
			Describe(context.Context) (VenueIdentity, error)
		})
		if !ok {
			t.Fatal("declaration hid account proof")
		}
		got, err := identity.Describe(context.Background())
		if err != nil || !got.Proof.Verified {
			t.Fatalf("proof: %v %v", got, err)
		}
	}
}

func TestDeclarationsDoNotInventOptionalOperations(t *testing.T) {
	// Embedding only Venue deliberately strips SimVenue's optional query method.
	base := struct{ Venue }{NewSimVenue("XSIM")}
	router := NewRouter([]Venue{declaredTestAdapter(base)})
	v, err := router.Route(&orderpb.OrderState{Venue: "XSIM"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v.(Querier); ok {
		t.Fatal("query capability invented")
	}
	if _, ok := v.(Closer); ok {
		t.Fatal("cancel capability invented")
	}
	if _, ok := v.(SelfHealing); ok {
		t.Fatal("close ownership invented")
	}
}
