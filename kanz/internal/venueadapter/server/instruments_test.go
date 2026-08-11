package server

import (
	"context"
	"io"
	"log/slog"
	"testing"

	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"

	"github.com/eighred/kanz/internal/execution"
)

// listingVenue is a connector that can enumerate its symbol map.
type listingVenue struct {
	execution.Venue
	mic  string
	syms execution.StaticSymbolMap
}

func (v listingVenue) MIC() string { return v.mic }
func (v listingVenue) Instruments() []execution.InstrumentSymbol {
	return v.syms.Instruments()
}

// silentVenue implements Venue and nothing else — an adapter whose connector
// cannot enumerate.
type silentVenue struct {
	execution.Venue
	mic string
}

func (v silentVenue) MIC() string { return v.mic }

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The adapter reports the pairs it is CONFIGURED for, ordered, with both names
// (#406). The order matters: Go randomises map iteration, and a picker whose
// list reshuffles on every load is a list nobody can scan.
func TestListInstrumentsReportsTheConfiguredSetInOrder(t *testing.T) {
	s := &Server{
		venue: listingVenue{mic: "XBIN", syms: execution.StaticSymbolMap{
			"ETH-USD": "ETHUSDT",
			"BTC-USD": "BTCUSDT",
			"SOL-USD": "SOLUSDT",
		}},
		logger: quiet(),
	}

	resp, err := s.ListInstruments(context.Background(), &venuepb.ListInstrumentsRequest{})
	if err != nil {
		t.Fatalf("ListInstruments: %v", err)
	}
	if resp.GetMic() != "XBIN" {
		t.Errorf("mic = %q, want XBIN — an aggregated list must not lose which venue answered", resp.GetMic())
	}

	var ids []string
	for _, in := range resp.GetInstruments() {
		ids = append(ids, in.GetInstrumentId())
		if in.GetVenueSymbol() == "" {
			t.Errorf("%s carries no venue symbol; the canonical id alone does not say what is being bought",
				in.GetInstrumentId())
		}
	}
	want := []string{"BTC-USD", "ETH-USD", "SOL-USD"}
	if len(ids) != len(want) {
		t.Fatalf("instruments = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("instruments = %v, want %v (ordered, so the list is stable between calls)", ids, want)
		}
	}
}

// A CONNECTOR THAT CANNOT ENUMERATE IS NOT AN ERROR. It answers with its MIC and
// no instruments, so the OMS records the venue as present-and-empty rather than
// failing the whole catalogue — one old adapter must not blank out every other
// venue's pairs.
func TestListInstrumentsIsEmptyNotAnErrorForASilentConnector(t *testing.T) {
	s := &Server{venue: silentVenue{mic: "XOKX"}, logger: quiet()}

	resp, err := s.ListInstruments(context.Background(), &venuepb.ListInstrumentsRequest{})
	if err != nil {
		t.Fatalf("a connector that cannot enumerate produced an error (%v); it must answer empty, "+
			"or one un-upgraded adapter takes down the whole catalogue", err)
	}
	if resp.GetMic() != "XOKX" {
		t.Errorf("mic = %q, want XOKX — the caller must still be told which venue said nothing", resp.GetMic())
	}
	if len(resp.GetInstruments()) != 0 {
		t.Errorf("instruments = %d, want 0", len(resp.GetInstruments()))
	}
}
