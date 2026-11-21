package execution

import (
	"context"
	"math/big"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/instrument"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ValidSpotTakerFees admits only rates the spot scenario can fund in the two
// traded assets. A missing rate cannot be treated as an explicitly free leg.
func ValidSpotTakerFees(schedule *venuepb.DemoSpotTakerSchedule, pair instrument.Pair) bool {
	for _, fee := range []*commonpb.SpotTakerFee{schedule.GetBuy(), schedule.GetSell()} {
		if fee.GetBps() == nil || !dec.InDomain(fee.Bps) ||
			(fee.Currency != pair.Base && fee.Currency != pair.Quote) ||
			new(big.Rat).Abs(dec.FromProto(fee.Bps)).Cmp(big.NewRat(10000, 1)) >= 0 {
			return false
		}
	}
	return true
}

// ReadBoundDemoSpotFunds addresses the exact adapter selected by the OMS binding.
func ReadBoundDemoSpotFunds(ctx context.Context, venues []Venue, req *venuepb.ReadPortfolioDemoSpotFundsRequest, bindings *AccountBindings) (*venuepb.ReadDemoSpotFundsResponse, error) {
	tenant, portfolio, market, id := req.GetTenantId(), req.GetPortfolioId(), req.GetMarketVenue(), req.GetInstrumentId()
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
			IncludeTakerSchedule: req.IncludeTakerSchedule,
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
		schedule := funds.TakerSchedule
		if schedule != nil && (!req.IncludeTakerSchedule || !ValidSpotTakerFees(schedule, pair) ||
			schedule.RequestStartedAt.CheckValid() != nil || schedule.ReceivedAt.CheckValid() != nil ||
			schedule.RequestStartedAt.AsTime().Before(end) || schedule.ReceivedAt.AsTime().Before(schedule.RequestStartedAt.AsTime()) ||
			schedule.ReceivedAt.AsTime().After(now)) {
			funds.TakerSchedule = nil
		}
		return funds, nil
	}
	return nil, status.Error(codes.NotFound, "spot funds unavailable")
}
