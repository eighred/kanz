package main

// SOV-02a — the OMS must not take its own manifest's word for whose money an
// adapter spends.
//
// These drive the REAL dialVenues against a REAL gRPC adapter on a real port: the
// composition root is where the refusal has to happen (before a single order is
// admitted), and a unit test of the comparison alone would not prove the OMS
// actually asks.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/services/oms/internal/config"
)

// adapter is an out-of-process venue adapter that reports the identity it is given.
type adapter struct {
	venuepb.UnimplementedVenueAdapterServiceServer
	id *venuepb.DescribeResponse
}

func (a *adapter) Describe(context.Context, *venuepb.DescribeRequest) (*venuepb.DescribeResponse, error) {
	return a.id, nil
}

// serveAdapter stands the adapter up on a real TCP port and returns its address.
func serveAdapter(t *testing.T, id *venuepb.DescribeResponse) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer()
	venuepb.RegisterVenueAdapterServiceServer(gs, &adapter{id: id})
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func unverifiedCounter() prometheus.Counter {
	return prometheus.NewCounter(prometheus.CounterOpts{Name: "kanz_oms_unverified_venue_account_total"})
}

func undeclaredCounter() prometheus.Counter {
	return prometheus.NewCounter(prometheus.CounterOpts{Name: "kanz_oms_undeclared_venue_order_types_total"})
}

// TestDialVenuesRefusesAnAdapterHoldingAnotherAccount is the whole point of SOV-02a.
//
// OMS_VENUE_ENDPOINTS says this adapter is okx-sub-1. The adapter — the only process
// holding the API key — says it is okx-sub-2. Before this check the OMS registered the
// venue under the DECLARED account, the router matched okx-sub-1's portfolio to it, and
// every fill posted to that portfolio's ledger rows while the exchange debited okx-sub-2.
// Segregated books over a pool that is not the one they name.
func TestDialVenuesRefusesAnAdapterHoldingAnotherAccount(t *testing.T) {
	addr := serveAdapter(t, &venuepb.DescribeResponse{
		Mic: "XOKX", Account: "okx-sub-2", AccountVerified: true, ExchangeAccountId: "99999",
	})
	cfg := config.Config{
		Tenant:         "acme",
		VenueEndpoints: "XOKX/okx-sub-1=" + addr,
	}

	_, _, err := dialVenues(context.Background(), cfg, unverifiedCounter(), undeclaredCounter(), quietLogger())
	if err == nil {
		t.Fatal("dialVenues accepted an adapter that holds a DIFFERENT account's credential")
	}
	if !errors.Is(err, execution.ErrVenueIdentityMismatch) {
		t.Fatalf("err = %v, want ErrVenueIdentityMismatch", err)
	}
	for _, want := range []string{"okx-sub-1", "okx-sub-2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}
}

// TestDialVenuesAcceptsAnAgreeingAdapter: the adapter is who the manifest says, and
// the exchange confirmed it. The venue registers under that account.
func TestDialVenuesAcceptsAnAgreeingAdapter(t *testing.T) {
	addr := serveAdapter(t, &venuepb.DescribeResponse{
		Mic: "XBIN", Account: "binance-main", AccountVerified: true, ExchangeAccountId: "12345678",
	})
	cfg := config.Config{Tenant: "acme", VenueEndpoints: "XBIN/binance-main=" + addr}

	venues, closeConns, err := dialVenues(context.Background(), cfg, unverifiedCounter(), undeclaredCounter(), quietLogger())
	if err != nil {
		t.Fatalf("dialVenues: %v", err)
	}
	t.Cleanup(closeConns)

	if len(venues) != 1 {
		t.Fatalf("venues = %d, want 1", len(venues))
	}
	if venues[0].MIC() != "XBIN" || venues[0].Account() != "binance-main" {
		t.Errorf("venue = %s/%s", venues[0].MIC(), venues[0].Account())
	}
}

// TestDialVenuesRefusesAnUnverifiedAccountWhenRequired: the adapter agrees with the
// manifest but never proved itself against the exchange. With the control armed, that
// is a refusal — nobody has confirmed this key belongs to the pool it names.
func TestDialVenuesRefusesAnUnverifiedAccountWhenRequired(t *testing.T) {
	addr := serveAdapter(t, &venuepb.DescribeResponse{Mic: "XBIN", Account: "binance-main"})
	cfg := config.Config{
		Tenant:                 "acme",
		VenueEndpoints:         "XBIN/binance-main=" + addr,
		RequireVerifiedAccount: true,
	}

	if _, _, err := dialVenues(context.Background(), cfg, unverifiedCounter(), undeclaredCounter(), quietLogger()); err == nil {
		t.Fatal("dialVenues accepted an UNVERIFIED account with OMS_REQUIRE_VERIFIED_ACCOUNT=true")
	}
}

// TestDialVenuesCountsAnUnverifiedAccountByDefault: with the control OFF (as shipped),
// an unproven adapter still trades — refusing every adapter nobody has bound a uid to
// yet is a trading outage dressed as a control. But it must not be SILENT: the state is
// counted, so "how much of the book trades against an unproven account" is a number on a
// dashboard rather than a question nobody asked.
func TestDialVenuesCountsAnUnverifiedAccountByDefault(t *testing.T) {
	addr := serveAdapter(t, &venuepb.DescribeResponse{Mic: "XBIN", Account: "binance-main"})
	cfg := config.Config{Tenant: "acme", VenueEndpoints: "XBIN/binance-main=" + addr}

	unverified := unverifiedCounter()
	venues, closeConns, err := dialVenues(context.Background(), cfg, unverified, undeclaredCounter(), quietLogger())
	if err != nil {
		t.Fatalf("dialVenues: %v", err)
	}
	t.Cleanup(closeConns)

	if len(venues) != 1 {
		t.Fatalf("venues = %d, want 1 (an unverified adapter still trades by default)", len(venues))
	}
	if got := testutil.ToFloat64(unverified); got != 1 {
		t.Errorf("kanz_oms_unverified_venue_account_total = %v, want 1", got)
	}
}

// TestDialVenuesRefusesAnAdapterThatDeclaresNoOrderTypes (#405), with the control
// armed. An adapter that does not say which order types it can place leaves the
// OMS admission gate OPEN for its MIC: a stop is admitted, stored, announced —
// and refused only at the exchange, after the estate has been told it exists.
func TestDialVenuesRefusesAnAdapterThatDeclaresNoOrderTypes(t *testing.T) {
	addr := serveAdapter(t, &venuepb.DescribeResponse{
		Mic: "XBIN", Account: "binance-main", AccountVerified: true, ExchangeAccountId: "12345678",
	})
	cfg := config.Config{
		Tenant:                  "acme",
		VenueEndpoints:          "XBIN/binance-main=" + addr,
		RequireOrderTypeSupport: true,
	}

	_, _, err := dialVenues(context.Background(), cfg, unverifiedCounter(), undeclaredCounter(), quietLogger())
	if err == nil {
		t.Fatal("dialVenues accepted an adapter that declared NO order types with OMS_REQUIRE_ORDER_TYPE_SUPPORT=true")
	}
	if !strings.Contains(err.Error(), "order types") {
		t.Errorf("error does not name the cause: %v", err)
	}
}

// TestDialVenuesCountsAnUndeclaredAdapterByDefault: with the control OFF (as
// shipped), an adapter that predates venue.v1's supported_order_types still
// trades — refusing every un-upgraded adapter would turn a schema addition into a
// trading outage. But "did not say" must not look like "checked, and fine": it is
// counted, so the number of venues whose admission gate is open is on a dashboard
// rather than a question nobody asked.
func TestDialVenuesCountsAnUndeclaredAdapterByDefault(t *testing.T) {
	addr := serveAdapter(t, &venuepb.DescribeResponse{
		Mic: "XBIN", Account: "binance-main", AccountVerified: true, ExchangeAccountId: "12345678",
	})
	cfg := config.Config{Tenant: "acme", VenueEndpoints: "XBIN/binance-main=" + addr}

	undeclared := undeclaredCounter()
	venues, closeConns, err := dialVenues(context.Background(), cfg, unverifiedCounter(), undeclared, quietLogger())
	if err != nil {
		t.Fatalf("dialVenues: %v", err)
	}
	t.Cleanup(closeConns)

	if len(venues) != 1 {
		t.Fatalf("venues = %d, want 1 (an adapter that declared nothing still trades by default)", len(venues))
	}
	if got := testutil.ToFloat64(undeclared); got != 1 {
		t.Errorf("kanz_oms_undeclared_venue_order_types_total = %v, want 1", got)
	}
}

// TestDialVenuesArmsTheGateForADeclaringAdapter is the half that proves the
// declaration is not merely logged. The venue that reaches the router must carry
// the adapter's answer — a Describe that is read and discarded would satisfy every
// assertion above while leaving the admission gate open for a venue that did speak.
func TestDialVenuesArmsTheGateForADeclaringAdapter(t *testing.T) {
	addr := serveAdapter(t, &venuepb.DescribeResponse{
		Mic: "XBIN", Account: "binance-main", AccountVerified: true, ExchangeAccountId: "12345678",
		SupportedOrderTypes: []orderpb.OrderType{
			orderpb.OrderType_ORDER_TYPE_MARKET,
			orderpb.OrderType_ORDER_TYPE_LIMIT,
		},
	})
	cfg := config.Config{Tenant: "acme", VenueEndpoints: "XBIN/binance-main=" + addr}

	undeclared := undeclaredCounter()
	venues, closeConns, err := dialVenues(context.Background(), cfg, unverifiedCounter(), undeclared, quietLogger())
	if err != nil {
		t.Fatalf("dialVenues: %v", err)
	}
	t.Cleanup(closeConns)

	if got := testutil.ToFloat64(undeclared); got != 0 {
		t.Errorf("kanz_oms_undeclared_venue_order_types_total = %v, want 0 — this adapter declared", got)
	}
	r := execution.NewRouter(venues...)
	if !r.SupportsOrderType("XBIN", orderpb.OrderType_ORDER_TYPE_LIMIT) {
		t.Error("router refuses LIMIT at a venue that declared it — a declared type would be refused at admission")
	}
	if r.SupportsOrderType("XBIN", orderpb.OrderType_ORDER_TYPE_STOP) {
		t.Fatal("router permits STOP at a venue that declared only MARKET and LIMIT.\n" +
			"The adapter's answer never reached the router, so the order is admitted, stored and announced, " +
			"and only the exchange refuses it.")
	}
}
