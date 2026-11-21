package venueconformance

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/execution"
	adapterserver "github.com/eighred/kanz/internal/venueadapter/server"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/oms/internal/venuesrv"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type cashFixture struct{ execution.Venue }

func (cashFixture) MIC() string     { return "XOKX" }
func (cashFixture) Account() string { return "okx-sub-1" }
func (cashFixture) Instruments() []execution.InstrumentSymbol {
	return execution.StaticSymbolMap{"BTC-USDT": "BTC-USDT"}.Instruments()
}
func (cashFixture) DemoSpotFunds(context.Context, execution.InstrumentSymbol, string) (string, string, error) {
	return "0", "9007199254740993.01", nil
}

func (cashFixture) DemoSpotTakerSchedule(context.Context, execution.InstrumentSymbol, string) *venuepb.DemoSpotTakerSchedule {
	return &venuepb.DemoSpotTakerSchedule{
		Buy:  &commonpb.SpotTakerFee{Bps: &commonpb.Decimal{}, Currency: "BTC"},
		Sell: &commonpb.SpotTakerFee{Bps: &commonpb.Decimal{Coefficient: 2}, Currency: "USDT"},
	}
}

// Exercise both production gRPC services and the peer identity supplied by an
// actual TLS handshake, including a mesh member without portfolio authority.
func TestPortfolioDemoFundsOverMutualTLS(t *testing.T) {
	const gatewayID = "spiffe://kanz.internal/ns/kanz-services/sa/api-gateway"
	ca := newCA(t)
	oms := identity(ca, ca.issue(t, clientID, false))
	adapter := identity(ca, ca.issue(t, serverID, false))
	gateway := identity(ca, ca.issue(t, gatewayID, false))
	serve := func(s *grpc.Server) string {
		t.Helper()
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = s.Serve(lis) }()
		t.Cleanup(s.Stop)
		return lis.Addr().String()
	}
	dial := func(addr string, source staticIdentity, expected string) *grpc.ClientConn {
		t.Helper()
		conn, err := grpc.NewClient(addr, transport.ClientDialOption(source, transport.AuthorizeServices(spiffeid.RequireFromString(expected))))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	as := grpc.NewServer(transport.ServerOption(adapter, transport.AuthorizeServices(oms.svid.ID)))
	venuepb.RegisterVenueAdapterServiceServer(as, adapterserver.New(cashFixture{}, nil, nil,
		execution.AccountProof{Verified: true, ExchangeAccountID: "42"}, nil, nil).WithDemoSpotFunds("__system__"))
	remote := execution.NewGRPCVenue("XOKX", "okx-sub-1", dial(serve(as), oms, serverID), "__system__")
	bindings, err := execution.ParseBindings("__system__/PF1@XOKX=okx-sub-1,__system__/PF1@XBIN=binance-main,__system__/other@XOKX=another-account")
	if err != nil {
		t.Fatal(err)
	}
	os := grpc.NewServer(transport.ServerOption(oms, transport.AuthorizeMesh()))
	venuesrv.New(nil, "__system__").WithDemoSpotFunds([]execution.Venue{remote}, bindings).Register(os)
	addr := serve(os)
	client := venuepb.NewVenueQueryServiceClient(dial(addr, gateway, clientID))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := &venuepb.ReadPortfolioDemoSpotFundsRequest{TenantId: "__system__", PortfolioId: "PF1", MarketVenue: "OKX", InstrumentId: "BTC-USDT"}
	result, err := client.ReadPortfolioDemoSpotFunds(ctx, req)
	if err != nil || result.GetPortfolioId() != "PF1" || result.GetMarketVenue() != "OKX" ||
		result.GetFunds().GetMic() != "XOKX" || result.GetFunds().GetVenueAccountId() != "okx-sub-1" ||
		result.GetFunds().GetAvailableBase() == nil || result.Funds.AvailableQuote.GetCoefficient() != 900719925474099301 {
		t.Fatalf("scoped mTLS read: %v, %v", result, err)
	}
	if result.Funds.TakerSchedule != nil {
		t.Fatal("unexpected private schedule")
	}
	req.IncludeTakerSchedule = true
	result, err = client.ReadPortfolioDemoSpotFunds(ctx, req)
	if err != nil || result.GetFunds().GetTakerSchedule().GetSell().GetBps().GetCoefficient() != 2 ||
		result.Funds.TakerSchedule.GetBuy().GetCurrency() != "BTC" {
		t.Fatal(result, err)
	}
	untrusted := venuepb.NewVenueQueryServiceClient(dial(addr, adapter, clientID))
	if result, err := untrusted.ReadPortfolioDemoSpotFunds(ctx, req); result != nil || status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unexpected non-gateway result: %v, %v", result, err)
	}
	req.PortfolioId = "other"
	if result, err := client.ReadPortfolioDemoSpotFunds(ctx, req); result != nil || status.Code(err) != codes.NotFound {
		t.Fatalf("unexpected unbound adapter result: %v, %v", result, err)
	}
}
