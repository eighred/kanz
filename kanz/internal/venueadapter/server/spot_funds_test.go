package server

import (
	"context"
	"testing"

	"github.com/eighred/kanz/internal/execution"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fundsVenue struct {
	listingVenue
	reads int
}

func (*fundsVenue) Account() string { return "account" }
func (v *fundsVenue) DemoSpotFunds(context.Context, execution.InstrumentSymbol, string) (string, string, error) {
	v.reads++
	return "0", "0", nil
}

func TestDemoSpotFundsRefusesBeforePrivateRead(t *testing.T) {
	v := &fundsVenue{listingVenue: listingVenue{mic: "OKX", syms: execution.StaticSymbolMap{"BTC-USDT": "BTC-USDT", "ETH-USD": "ETH-USDT"}}}
	proof := execution.AccountProof{Verified: true, ExchangeAccountID: "42"}
	for _, tc := range []struct {
		readTenant, requestTenant, mic, account, instrument string
		verified                                            bool
		want                                                codes.Code
	}{
		{"", "owner", "OKX", "account", "BTC-USDT", true, codes.NotFound},
		{"__system__", "different", "OKX", "account", "BTC-USDT", true, codes.NotFound},
		{"owner", "other", "OKX", "account", "BTC-USDT", true, codes.NotFound},
		{"owner", "owner", "BINANCE", "account", "BTC-USDT", true, codes.NotFound},
		{"owner", "owner", "OKX", "other", "BTC-USDT", true, codes.NotFound},
		{"owner", "owner", "OKX", "account", "BTC-USDT", false, codes.FailedPrecondition},
		{"owner", "owner", "OKX", "account", "ETH-USD", true, codes.NotFound},
	} {
		proof.Verified = tc.verified
		srv := New(v, nil, nil, proof, nil, nil).WithDemoSpotFunds(tc.readTenant)
		got, err := srv.ReadDemoSpotFunds(context.Background(), &venuepb.ReadDemoSpotFundsRequest{
			TenantId: tc.requestTenant, VenueAccountId: tc.account, Mic: tc.mic, InstrumentId: tc.instrument,
		})
		if got != nil || status.Code(err) != tc.want || v.reads != 0 {
			t.Fatalf("%+v: got %v, %v, reads %d", tc, got, err, v.reads)
		}
	}
}
