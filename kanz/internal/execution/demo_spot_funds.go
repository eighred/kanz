package execution

import (
	"context"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/instrument"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ReadBoundDemoSpotFunds addresses the exact adapter selected by the OMS binding.
func ReadBoundDemoSpotFunds(ctx context.Context, venues []Venue, tenant, portfolio, market, id string, bindings *AccountBindings) (*venuepb.ReadDemoSpotFundsResponse, error) {
	binding, bound := bindings.SpotAccount(tenant, portfolio, market)
	account, mic := binding.Account, binding.MIC
	pair, ok := instrument.ParseID(id)
	if !bound || !ok || id != pair.Base+"-"+pair.Quote || pair.Base == pair.Quote {
		return nil, status.Error(codes.NotFound, "spot funds unavailable")
	}
	for _, venue := range venues {
		if venue.MIC() != mic || venue.Account() != account {
			continue
		}
		adapter, ok := venueCapability[*GRPCVenue](venue)
		if !ok || adapter.tenant != tenant {
			break
		}
		readCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
		defer cancel()
		funds, err := adapter.client.ReadDemoSpotFunds(readCtx, &venuepb.ReadDemoSpotFundsRequest{
			TenantId: tenant, VenueAccountId: account, Mic: mic, InstrumentId: id,
		})
		if err != nil {
			return nil, status.Error(codes.Unavailable, "demo venue funds unavailable")
		}
		symbol := pair.Base + "-" + pair.Quote
		if market == "BINANCE" {
			symbol = pair.Base + pair.Quote
		}
		if funds.GetOwnerTenant() != tenant || funds.GetVenueAccountId() != account || funds.GetMic() != mic ||
			funds.GetInstrumentId() != id || funds.GetVenueSymbol() != symbol ||
			funds.GetBaseCurrency() != pair.Base || funds.GetQuoteCurrency() != pair.Quote ||
			funds.GetAvailableBase() == nil || funds.GetAvailableQuote() == nil ||
			!dec.InDomain(funds.AvailableBase) || !dec.InDomain(funds.AvailableQuote) ||
			funds.AvailableBase.Coefficient < 0 || funds.AvailableQuote.Coefficient < 0 ||
			funds.RequestStartedAt.CheckValid() != nil || funds.ReceivedAt.CheckValid() != nil {
			return nil, status.Error(codes.FailedPrecondition, "invalid demo venue observation")
		}
		start, end, now := funds.RequestStartedAt.AsTime(), funds.ReceivedAt.AsTime(), time.Now()
		if end.Before(start) || end.After(now) || now.Sub(start) > 6*time.Second {
			return nil, status.Error(codes.FailedPrecondition, "demo venue observation outside capture window")
		}
		return funds, nil
	}
	return nil, status.Error(codes.NotFound, "spot funds unavailable")
}
