package execution

import (
	"errors"
	"testing"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
)

func TestRouter_RoutesByTargetVenue(t *testing.T) {
	r := NewRouter(NewSimVenue("BINANCE"), NewSimVenue("OKX"))

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
	// No target → the first-configured venue (SOR default).
	v, err = r.Route(&orderpb.OrderState{})
	if err != nil || v.MIC() != "BINANCE" {
		t.Fatalf("no target → %v (mic %q), want BINANCE", err, mic(v))
	}
	// An unconfigured target is refused — never silently misrouted.
	if _, err := r.Route(&orderpb.OrderState{Venue: "KRAKEN"}); !errors.Is(err, ErrNoVenue) {
		t.Fatalf("unconfigured target = %v, want ErrNoVenue", err)
	}
}

func TestRouter_NoVenues(t *testing.T) {
	if _, err := NewRouter().Route(&orderpb.OrderState{}); !errors.Is(err, ErrNoVenue) {
		t.Fatalf("empty router = %v, want ErrNoVenue", err)
	}
}

func mic(v Venue) string {
	if v == nil {
		return ""
	}
	return v.MIC()
}
