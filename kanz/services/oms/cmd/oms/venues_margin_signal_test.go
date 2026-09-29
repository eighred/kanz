package main

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/services/oms/internal/config"
)

// AN ADAPTER WITH A SILENTLY OPEN MARGIN GATE MUST NOT LOOK LIKE ONE THAT WAS
// CHECKED (#742).
//
// dialVenues counts and warns for an adapter that declares no order types
// (#405), and separately for one that declares no time-in-force (#486). It did
// neither for margin mode — DeclaresMarginModes() existed with zero callers — so
// margin was the only member of this family with no dial-time signal at all.
//
// That is the AGENTS.md line "nothing configured and checked-and-fine must never
// look the same", and margin is where it costs most: an undeclared order type
// produces an order that does nothing, an inexpressible time-in-force produces
// one that does the wrong thing, and an unrefused collateral regime produces a
// REAL position whose regime the fund's records get wrong.

// An adapter that says nothing about collateral regimes still trades — refusing
// every un-upgraded adapter would turn a schema addition into a trading outage —
// but the open gate is COUNTED.
func TestDialVenuesCountsAnAdapterThatDeclaresNoMarginModes(t *testing.T) {
	addr := serveAdapter(t, &venuepb.DescribeResponse{
		Mic: "XBIN", Account: "binance-main", AccountVerified: true, ExchangeAccountId: "12345678",
	})
	cfg := config.Config{Tenant: "acme", VenueEndpoints: "XBIN/binance-main=" + addr}

	undeclared, undeclaredTIF, undeclaredMargin := undeclaredCounter(), undeclaredCounter(), undeclaredCounter()
	venues, _, closeConns, err := dialVenues(context.Background(), cfg, venueCapabilityCounters{unverifiedAccounts: unverifiedCounter(), undeclaredOrderTypes: undeclared, undeclaredTimeInForce: undeclaredTIF, undeclaredMarginModes: undeclaredMargin}, quietLogger())
	if err != nil {
		t.Fatalf("dialVenues: %v", err)
	}
	t.Cleanup(closeConns)

	if len(venues) != 1 {
		t.Fatalf("venues = %d, want 1 — an adapter that declared nothing still trades by default", len(venues))
	}
	if got := testutil.ToFloat64(undeclaredMargin); got != 1 {
		t.Errorf("kanz_oms_undeclared_venue_margin_modes_total = %v, want 1 — this adapter said "+
			"nothing about collateral regimes, and an operator has no other way to find out", got)
	}
}

// THE THREE COUNTERS MUST BE THREE NUMBERS, not one wearing three names. This
// adapter answers the margin question and neither of the others, which is the
// case that separates them: a shared counter would report all three gaps for an
// adapter that closed one.
func TestDialVenuesCountsTheThreeCapabilityGapsApart(t *testing.T) {
	addr := serveAdapter(t, &venuepb.DescribeResponse{
		Mic: "XBIN", Account: "binance-main", AccountVerified: true, ExchangeAccountId: "12345678",
		SupportedMarginModes: []orderpb.MarginMode{orderpb.MarginMode_MARGIN_MODE_UNSPECIFIED},
	})
	cfg := config.Config{Tenant: "acme", VenueEndpoints: "XBIN/binance-main=" + addr}

	undeclared, undeclaredTIF, undeclaredMargin := undeclaredCounter(), undeclaredCounter(), undeclaredCounter()
	venues, _, closeConns, err := dialVenues(context.Background(), cfg, venueCapabilityCounters{unverifiedAccounts: unverifiedCounter(), undeclaredOrderTypes: undeclared, undeclaredTimeInForce: undeclaredTIF, undeclaredMarginModes: undeclaredMargin}, quietLogger())
	if err != nil {
		t.Fatalf("dialVenues: %v", err)
	}
	t.Cleanup(closeConns)

	if got := testutil.ToFloat64(undeclaredMargin); got != 0 {
		t.Errorf("kanz_oms_undeclared_venue_margin_modes_total = %v, want 0 — this adapter declared", got)
	}
	if got := testutil.ToFloat64(undeclared); got != 1 {
		t.Errorf("kanz_oms_undeclared_venue_order_types_total = %v, want 1 — this adapter declared "+
			"margin modes and NOT order types, so the counters must disagree", got)
	}
	if got := testutil.ToFloat64(undeclaredTIF); got != 1 {
		t.Errorf("kanz_oms_undeclared_venue_time_in_force_total = %v, want 1", got)
	}

	// AND THE DECLARATION MUST REACH THE ROUTER, not merely the log. A Describe
	// that is read and discarded satisfies every counter assertion above while
	// leaving the admission gate open for a venue that actually did speak.
	r := execution.NewRouter(venues)
	if !r.SupportsMarginMode("XBIN", orderpb.MarginMode_MARGIN_MODE_UNSPECIFIED) {
		t.Error("router refuses spot at a venue that declared it — a declared regime would be " +
			"refused at admission")
	}
	if r.SupportsMarginMode("XBIN", orderpb.MarginMode_MARGIN_MODE_CROSS) {
		t.Error("router admits CROSS at a venue that declared only spot — the levered order would " +
			"be admitted, announced, and then refused by the connector or placed as spot with the " +
			"audit root recording margin")
	}
}
