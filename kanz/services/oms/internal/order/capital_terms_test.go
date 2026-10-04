package order

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/refdata"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/services/oms/internal/capital"
)

type capitalReferenceSource struct{ rec refdata.Record }

func (s capitalReferenceSource) Fetch(_ context.Context, id string) (refdata.Record, bool, error) {
	return s.rec, id == s.rec.InstrumentID, nil
}

type unboundedVenue struct{}

type capitalEvidenceVenue struct {
	unboundedVenue
	evidence *venuepb.ReadCapitalEvidenceResponse
}

func (v capitalEvidenceVenue) ReadCapitalEvidence(context.Context, string) (*venuepb.ReadCapitalEvidenceResponse, error) {
	return v.evidence, nil
}

type approvedFeeFixture struct{ policy ApprovedMaximumFee }

func (f approvedFeeFixture) MaximumFee(context.Context, *orderpb.OrderState, string) (ApprovedMaximumFee, error) {
	return f.policy, nil
}

func (unboundedVenue) MIC() string     { return "XREAL" }
func (unboundedVenue) Account() string { return "account" }
func (unboundedVenue) Execute(context.Context, *orderpb.OrderState) ([]*orderpb.Fill, error) {
	return nil, nil
}

func TestCurrentVenueFeeCannotAuthorizeAdmission(t *testing.T) {
	now := time.Now().UTC()
	st := &orderpb.OrderState{OrderId: "real", Venue: "XREAL", VenueAccountId: "account", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, OrderedQuantity: d(1, 0), LimitPrice: d(100, 0)}
	cache, err := refdata.NewCache(capitalReferenceSource{refdata.Record{InstrumentID: "BTC-USD", AssetClass: "CRYPTO", BaseAsset: "BTC", QuoteAsset: "USD", AsOf: now}}, refdata.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cache.Lookup(st.InstrumentId, now)
	if _, err := cache.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	evidence := &venuepb.ReadCapitalEvidenceResponse{VenueSymbol: "BTCUSD", BaseAsset: "BTC", QuoteAsset: "USD", TradingState: "TRADING",
		SettlementSourceVersion: &venuepb.SourceVersion{Source: "venue:catalogue", ContentSha256: "catalogue-digest"},
		CurrentFee:              &venuepb.CurrentVenueFee{Components: []*venuepb.FeeRateComponent{{Name: "spot", TakerRate: "0.001"}}, SourceVersion: &venuepb.SourceVersion{Source: "venue:commission", ContentSha256: "fee-digest"}}}
	terms := SimPhysicalTerms{References: cache, Router: execution.NewRouter([]execution.Venue{capitalEvidenceVenue{evidence: evidence}})}
	if _, err := terms.Debits(context.Background(), st, now); !errors.Is(err, ErrCapitalTermsUnknown) {
		t.Fatalf("current fee authorized order: %v", err)
	}
	policy := ApprovedMaximumFee{Venue: "XREAL", Account: "account", InstrumentID: "BTC-USD", VenueSymbol: "BTCUSD", Side: st.Side,
		OrderType: st.OrderType, TimeInForce: st.TimeInForce, Source: "approved:fee-policy", Version: "v1", ApprovedBy: "risk-control",
		Fees: []*commonpb.Money{{CurrencyCode: "USD", Amount: d(2, 0)}}}
	terms.ApprovedFees = approvedFeeFixture{policy: policy}
	if _, known := cache.Lookup(st.InstrumentId, now); !known {
		t.Fatal("reference unavailable")
	}
	if _, directErr := capital.PhysicalDebits(st, "BTC", "USD", policy.Fees); directErr != nil {
		t.Fatalf("physical: %v", directErr)
	}
	debits, err := terms.Debits(context.Background(), st, now)
	if err != nil || len(debits) != 1 || dec.FromProto(debits[0].Amount).RatString() != "102" {
		t.Fatalf("approved maximum debit=%v err=%v", debits, err)
	}
	policy.ApprovedBy = ""
	terms.ApprovedFees = approvedFeeFixture{policy: policy}
	if _, err := terms.Debits(context.Background(), st, now); !errors.Is(err, ErrCapitalTermsUnknown) {
		t.Fatalf("unapproved policy authorized order: %v", err)
	}
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
