package execution

import (
	"errors"
	"strings"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// WHERE DOES AN ORDER GO WHEN IT NAMES NO VENUE? (#437)
//
// It went to venues[0] — whichever adapter the config happened to list first.
// The type called itself "the smart-order-router" and its fallback "first
// configured / best venue": two different things, and only the first was
// implemented. Nothing ranked anything.
//
// Almost nothing reaches this path today, because both fan-out producers stamp a
// venue on every leg. That is precisely why it could be wrong for so long — and
// why the day something does arrive untargeted is the wrong day to find out the
// destination was decided by argument order.

// ONE VENUE NEEDS NO CONFIGURATION. There is nothing to choose, so demanding a
// choice would be ceremony — and would refuse every untargeted order on the
// single-adapter deployments this platform actually runs.
func TestDefaultVenue_SingleVenueIsUnambiguous(t *testing.T) {
	r := NewRouter([]Venue{NewSimVenue("BINANCE")})

	v, err := r.Route(&orderpb.OrderState{})
	if err != nil {
		t.Fatalf("untargeted order with one venue → %v, want it routed", err)
	}
	if v.MIC() != "BINANCE" {
		t.Fatalf("routed to %q, want BINANCE", v.MIC())
	}
	if got, ok := r.DefaultVenue(); !ok || got.MIC() != "BINANCE" {
		t.Fatalf("DefaultVenue() = %v/%v, want BINANCE/true", mic(got), ok)
	}
}

// SEVERAL VENUES AND NO CHOICE IS A REFUSAL, NAMING THE CANDIDATES. An order sent
// to an arbitrary venue is worse than one refused with a reason — the ruling
// ErrVenueNotConfigured already encodes one case over.
func TestDefaultVenue_AmbiguousIsRefusedAndNamesTheCandidates(t *testing.T) {
	r := NewRouter([]Venue{NewSimVenue("BINANCE"), NewSimVenue("OKX")})

	_, err := r.Route(&orderpb.OrderState{})
	if !errors.Is(err, ErrNoDefaultVenue) {
		t.Fatalf("err = %v, want ErrNoDefaultVenue", err)
	}
	for _, want := range []string{"BINANCE", "OKX"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name candidate %q — the operator has to go and find "+
				"them, which is the friction that gets solved by picking arbitrarily again", err, want)
		}
	}
	if _, ok := r.DefaultVenue(); ok {
		t.Error("DefaultVenue() resolved one with nothing named — it must not fall back to a position")
	}
}

// A NAMED DEFAULT IS HONOURED, and it is a NAME rather than a position: passing
// OKX second and naming it still routes there.
func TestDefaultVenue_NamedIsHonouredRegardlessOfOrder(t *testing.T) {
	r := NewRouter([]Venue{NewSimVenue("BINANCE"), NewSimVenue("OKX")}, WithDefaultVenue("OKX"))

	v, err := r.Route(&orderpb.OrderState{})
	if err != nil {
		t.Fatalf("untargeted order with a named default → %v", err)
	}
	if v.MIC() != "OKX" {
		t.Fatalf("routed to %q, want OKX — the NAME must win over the position", v.MIC())
	}
}

// A DEFAULT NAMING A VENUE NO ADAPTER HOLDS IS REFUSED, and refused as
// ErrVenueNotConfigured rather than ErrNoDefaultVenue: somebody DID choose, and
// the choice cannot be honoured. Silently falling back to another venue would
// spend a different exchange's collateral than the operator named.
func TestDefaultVenue_AnUnreachableNamedDefaultIsRefused(t *testing.T) {
	r := NewRouter([]Venue{NewSimVenue("BINANCE")}, WithDefaultVenue("KRAKEN"))

	_, err := r.Route(&orderpb.OrderState{})
	if !errors.Is(err, ErrVenueNotConfigured) {
		t.Fatalf("err = %v, want ErrVenueNotConfigured — a named default nobody holds is a "+
			"configuration error, not an absent choice", err)
	}
	if !strings.Contains(err.Error(), "KRAKEN") {
		t.Errorf("refusal %q does not name the configured default", err)
	}
}

// A TARGETED ORDER IS UNAFFECTED by any of this — the allocation-matrix path is
// the one every order from the fan-out producers takes, and it must keep working
// with no default configured at all.
func TestDefaultVenue_TargetedRoutingIsUnaffected(t *testing.T) {
	r := NewRouter([]Venue{NewSimVenue("BINANCE"), NewSimVenue("OKX")})

	v, err := r.Route(&orderpb.OrderState{Venue: "OKX"})
	if err != nil || v.MIC() != "OKX" {
		t.Fatalf("targeted order → %v (mic %q), want OKX", err, mic(v))
	}
}
