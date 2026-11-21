package server

import (
	"context"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type demoSpotFundsReader interface {
	InstrumentLister
	DemoSpotFunds(context.Context, execution.InstrumentSymbol, string) (base, quote string, err error)
}

// WithDemoSpotFunds enables the private read on the tenant's authenticated OMS listener.
func (s *Server) WithDemoSpotFunds(tenant string) *Server {
	s.spotFundsTenant = tenant
	return s
}

func (s *Server) ReadDemoSpotFunds(ctx context.Context, req *venuepb.ReadDemoSpotFundsRequest) (*venuepb.ReadDemoSpotFundsResponse, error) {
	if s.spotFundsTenant == "" || req.GetTenantId() != s.spotFundsTenant ||
		req.GetMic() != s.venue.MIC() || req.GetVenueAccountId() == "" || req.GetVenueAccountId() != s.venue.Account() {
		return nil, status.Error(codes.NotFound, "spot account unavailable")
	}
	reader, ok := s.venue.(demoSpotFundsReader)
	if !ok || !s.proof.Verified || s.proof.ExchangeAccountID == "" {
		return nil, status.Error(codes.FailedPrecondition, "verified demo spot account required")
	}
	for _, in := range reader.Instruments() {
		p := in.Pair
		if in.InstrumentID != req.GetInstrumentId() || in.QuoteMismatch || p.Base == "" || p.Quote == "" || p.Base == p.Quote ||
			in.InstrumentID != p.Base+"-"+p.Quote {
			continue
		}
		start := time.Now()
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		base, quote, err := reader.DemoSpotFunds(readCtx, in, s.proof.ExchangeAccountID)
		expired := readCtx.Err()
		cancel()
		if expired != nil {
			return nil, status.FromContextError(expired).Err()
		}
		if err != nil {
			return nil, status.Error(codes.Unavailable, "could not verify demo spot funds")
		}
		b, bOK := spotAmount(base)
		q, qOK := spotAmount(quote)
		if !bOK || !qOK {
			return nil, status.Error(codes.FailedPrecondition, "incomplete or unrepresentable available balance")
		}
		return &venuepb.ReadDemoSpotFundsResponse{
			OwnerTenant: s.spotFundsTenant, VenueAccountId: s.venue.Account(), Mic: s.venue.MIC(),
			InstrumentId: in.InstrumentID, VenueSymbol: in.VenueSymbol, BaseCurrency: p.Base, QuoteCurrency: p.Quote,
			AvailableBase: b, AvailableQuote: q, RequestStartedAt: timestamppb.New(start), ReceivedAt: timestamppb.Now(),
		}, nil
	}
	return nil, status.Error(codes.NotFound, "spot instrument unavailable")
}

func spotAmount(raw string) (*commonpb.Decimal, bool) {
	if strings.ContainsAny(raw, "-/") {
		return nil, false
	}
	return dec.ParseProtoExact(raw)
}
