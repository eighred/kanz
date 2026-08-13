package execution

import (
	"errors"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

func TestRouter_RoutesByTargetVenue(t *testing.T) {
	r := NewRouter([]Venue{NewSimVenue("BINANCE"), NewSimVenue("OKX")})

	// An order targeting OKX routes to the OKX venue (the allocation matrix).
	v, err := r.Route(&orderpb.OrderState{Venue: "OKX"})
	if err != nil || v.MIC() != "OKX" {
		t.Fatalf("target OKX → %v (mic %q)", err, mic(v))
	}
	// A different target routes to its own venue.
	v, err = r.Route(&orderpb.OrderState{Venue: "BINANCE"})
	if err != nil || v.MIC() != "BINANCE" {
		t.Fatalf("target BINANCE → %v (mic %q)", err, mic(v))
	}
	// No target, TWO venues, no default named → REFUSED (#437). This used to
	// answer BINANCE because BINANCE was passed first — a trading destination
	// chosen by argument order, under a type that called itself a smart order
	// router. The candidates are named so the operator picks once, in config,
	// instead of the slice picking on every order.
	if _, err := r.Route(&orderpb.OrderState{}); !errors.Is(err, ErrNoDefaultVenue) {
		t.Fatalf("no target with two venues and no default → %v, want ErrNoDefaultVenue", err)
	}
	// An unconfigured target is refused — never silently misrouted, and never
	// confused with "no venues at all".
	//
	// This used to return ErrNoVenue, and the OMS treats ErrNoVenue as "rest the
	// order". So an order naming a venue this OMS could never reach was ADMITTED
	// and left resting: the strategy was told its order was working while nothing
	// on the platform would ever route it anywhere (EXEC-M8). The two errors are
	// opposite situations and must not answer to the same errors.Is.
	_, err = r.Route(&orderpb.OrderState{Venue: "KRAKEN"})
	if !errors.Is(err, ErrVenueNotConfigured) {
		t.Fatalf("unconfigured target = %v, want ErrVenueNotConfigured", err)
	}
	if errors.Is(err, ErrNoVenue) {
		t.Fatal("a named-but-unconfigured venue answered to ErrNoVenue — the OMS would rest an order it can never execute")
	}
}

func TestRouter_NoVenues(t *testing.T) {
	if _, err := NewRouter(nil).Route(&orderpb.OrderState{}); !errors.Is(err, ErrNoVenue) {
		t.Fatalf("empty router = %v, want ErrNoVenue", err)
	}
}

func mic(v Venue) string {
	if v == nil {
		return ""
	}
	return v.MIC()
}
