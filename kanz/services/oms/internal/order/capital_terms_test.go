package order

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/refdata"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

type capitalReferenceSource struct{ rec refdata.Record }

func (s capitalReferenceSource) Fetch(_ context.Context, id string) (refdata.Record, bool, error) {
	return s.rec, id == s.rec.InstrumentID, nil
}

type unboundedVenue struct{}

func (unboundedVenue) MIC() string     { return "XREAL" }
func (unboundedVenue) Account() string { return "account" }
func (unboundedVenue) Execute(context.Context, *orderpb.OrderState) ([]*orderpb.Fill, error) {
	return nil, nil
}

func TestCapitalTermsRequireCompletePairAndFeeAuthority(t *testing.T) {
	now := time.Now().UTC()
	st := &orderpb.OrderState{OrderId: "one", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, OrderedQuantity: d(1, 0), LimitPrice: d(100, 0)}
	cache, err := refdata.NewCache(capitalReferenceSource{refdata.Record{InstrumentID: "BTC-USD", AssetClass: "CRYPTO", BaseAsset: "BTC", QuoteAsset: "USD", AsOf: now}}, refdata.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sim := SimPhysicalTerms{References: cache, Router: execution.NewRouter([]execution.Venue{execution.NewSimVenue("XNAS")})}
	if _, err := sim.Debits(context.Background(), st, now); !errors.Is(err, ErrCapitalTermsUnknown) {
		t.Fatalf("cold reference cache: %v", err)
	}
	if _, err := cache.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	debits, err := sim.Debits(context.Background(), st, now)
	if err != nil || len(debits) != 1 || debits[0].CurrencyCode != "USD" || dec.FromProto(debits[0].Amount).RatString() != "100" {
		t.Fatalf("simulator debits=%v err=%v", debits, err)
	}
	real := SimPhysicalTerms{References: cache, Router: execution.NewRouter([]execution.Venue{unboundedVenue{}})}
	if _, err := real.Debits(context.Background(), st, now); !errors.Is(err, ErrCapitalTermsUnknown) {
		t.Fatalf("unbounded real venue: %v", err)
	}
	st.OrderType = orderpb.OrderType_ORDER_TYPE_MARKET
	if _, err := sim.Debits(context.Background(), st, now); !errors.Is(err, ErrCapitalTermsUnknown) {
		t.Fatalf("unbounded market price: %v", err)
	}
}
