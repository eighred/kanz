package venuesrv

import (
	"context"
	"testing"

	"github.com/eighred/kanz/internal/execution"
)

func catalogue() []execution.VenueInstrument {
	return []execution.VenueInstrument{
		{MIC: "XBIN", InstrumentSymbol: execution.InstrumentSymbol{InstrumentID: "BTC-USD", VenueSymbol: "BTCUSDT"}},
		{MIC: "XOKX", InstrumentSymbol: execution.InstrumentSymbol{InstrumentID: "BTC-USD", VenueSymbol: "BTC-USDT"}},
		{MIC: "XBIN", InstrumentSymbol: execution.InstrumentSymbol{InstrumentID: "ETH-USD", VenueSymbol: "ETHUSDT"}},
	}
}

// THE VENUE SYMBOL IS REPORTED, NOT DROPPED (#406/#407).
//
// Canonical "BTC-USD" maps to a USDT-quoted symbol on both live venues: the
// platform already trades a stablecoin-quoted instrument while calling it USD,
// and nothing in the data model says so. Until instruments carry base and quote
// explicitly, this field is the ONLY place a caller can see what it is actually
// buying — a picker that showed the id alone would tell a user they were buying
// dollars.
func TestVenueSymbolSurvivesToTheCaller(t *testing.T) {
	resp, err := New(catalogue(), "acme").ListTradeableInstruments(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTradeableInstruments: %v", err)
	}
	if len(resp.GetInstruments()) != 3 {
		t.Fatalf("instruments = %d, want 3", len(resp.GetInstruments()))
	}
	for _, in := range resp.GetInstruments() {
		if in.GetVenueSymbol() == "" {
			t.Errorf("%s at %s carries no venue symbol — the caller cannot tell a USD-quoted "+
				"pair from a USDT-quoted one, which is a different credit exposure",
				in.GetInstrumentId(), in.GetMic())
		}
		if in.GetMic() == "" {
			t.Errorf("%s carries no MIC — an order names a venue, so a pair without one cannot be acted on",
				in.GetInstrumentId())
		}
	}
	// The same canonical id at two venues must survive as TWO entries: they are
	// different symbols on different exchanges, and collapsing them would make
	// the picker offer a pair without saying where it trades.
	var btc int
	for _, in := range resp.GetInstruments() {
		if in.GetInstrumentId() == "BTC-USD" {
			btc++
		}
	}
	if btc != 2 {
		t.Errorf("BTC-USD appears %d time(s), want 2 (XBIN and XOKX)", btc)
	}
}

// The owner tenant is the deny-by-default authz input the gateway checks the
// caller against. An OMS that was never given one must stamp EMPTY, which fails
// closed — never a value that happens to match.
func TestOwnerTenantIsStampedAndEmptyFailsClosed(t *testing.T) {
	resp, _ := New(catalogue(), "acme").ListTradeableInstruments(context.Background(), nil)
	if resp.GetOwnerTenant() != "acme" {
		t.Errorf("owner_tenant = %q, want acme", resp.GetOwnerTenant())
	}

	bare, _ := New(catalogue(), "").ListTradeableInstruments(context.Background(), nil)
	if bare.GetOwnerTenant() != "" {
		t.Errorf("owner_tenant = %q for an unconfigured tenant, want empty so the gate fails CLOSED",
			bare.GetOwnerTenant())
	}
}

// AN EMPTY CATALOGUE IS AN ANSWER, NOT AN ERROR: this deployment can trade
// nothing. Returning an error would let a caller render a real, misconfigured
// estate as a transient failure and retry forever.
func TestAnEmptyCatalogueIsAnAnswer(t *testing.T) {
	resp, err := New(nil, "acme").ListTradeableInstruments(context.Background(), nil)
	if err != nil {
		t.Fatalf("an empty catalogue returned an error (%v) — a deployment that can trade "+
			"nothing is a fact about it, not a failed read", err)
	}
	if len(resp.GetInstruments()) != 0 {
		t.Errorf("instruments = %d, want 0", len(resp.GetInstruments()))
	}
	if resp.GetOwnerTenant() != "acme" {
		t.Errorf("owner_tenant = %q — an empty catalogue must still be attributable", resp.GetOwnerTenant())
	}
}
