package translate

// A SIGNAL-ORIGINATED ORDER IS ROUTED TO ITS FUND'S TENANT (MT-02, #360).
//
// The gateway already prefixes the wire subject so the broker can deliver an
// order into the issuing tenant's NATS account. This path did not, and the two
// disagreeing is worse than neither working: the SAME order placed through the
// gateway routes correctly while a TradingView-originated one is delivered into
// __system__, where the tenant's OMS — which lives in its own account — never
// sees it. Nothing fails; the tenant simply looks like it is not trading.
//
// The tenant is resolved PER SIGNAL from TenantOf(fund_id). One webhook endpoint
// serves many funds, so a service-level tenant would be the wrong one for all but
// a single fund — which is exactly why the gateway's principal-derived value
// could not just be copied here.

import (
	"context"
	"math/big"
	"strings"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	signalpb "github.com/eighred/kanz/kanz-schemas-go/signal/v1"

	"github.com/eighred/kanz/pkg/bus"
)

func TestASignalOrderCarriesItsFundsTenantOnTheWire(t *testing.T) {
	rec := &recorder{}
	tr, err := New(Options{
		Prices:    StaticPrices{"BTC-USD": big.NewRat(50_000, 1)},
		Equity:    StaticEquity{"fund-alpha": new(big.Rat).SetInt64(1_000_000_000)},
		Positions: StaticPositions{"fund-alpha/BINANCE/BTC-USD": big.NewRat(0, 1)},
		Alloc:     StaticAllocation{"fund-alpha": {{Venue: "BINANCE", Weight: big.NewRat(1, 1)}}},
		Publisher: rec,
		Gate:      OpenGate(nil),
		// The fund→tenant map this endpoint serves. Without an explicit one the
		// fund id IS the tenant, which would make this test agree with itself.
		TenantOf: func(fundID string) string {
			if fundID == "fund-alpha" {
				return "acme"
			}
			return "other"
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := tr.Emit(context.Background(),
		intent(signalpb.SignalAction_SIGNAL_ACTION_BUY, big.NewRat(1, 100), signalpb.SizeType_SIZE_TYPE_PCT_OF_EQUITY)); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	var cmd *bus.Event
	for i := range rec.events {
		if _, ok := rec.events[i].Payload.(*orderpb.SubmitOrder); ok {
			cmd = &rec.events[i]
			break
		}
	}
	if cmd == nil {
		t.Fatal("no SubmitOrder command published — this test asserts on nothing")
	}

	if want := "tenant.acme." + SubjectSubmit; cmd.Subject != want {
		t.Errorf("wire subject = %q, want %q.\n\n"+
			"Unprefixed, this order is delivered into __system__ and oms-acme never sees it — "+
			"while the same order placed through the api-gateway routes correctly. Two paths, "+
			"one of them silently wrong.", cmd.Subject, want)
	}
	if cmd.EventType != SubjectSubmit {
		t.Errorf("event_type = %q, want the unchanged logical name %q — the prefix belongs on "+
			"the wire only", cmd.EventType, SubjectSubmit)
	}
	if cmd.TenantID != "acme" {
		t.Errorf("envelope tenant_id = %q, want acme", cmd.TenantID)
	}
	// The signal FACT is NOT bridged (only the order command is, #358), so it must
	// keep the bare subject — a prefix here would publish to a subject no stream
	// carries and no account imports.
	for _, e := range rec.events {
		if _, ok := e.Payload.(*signalpb.StrategySignal); ok && strings.HasPrefix(e.Subject, "tenant.") {
			t.Errorf("the signal FACT was prefixed (%q). Only the order command is bridged; "+
				"this subject is carried by no stream and imported by no account.", e.Subject)
		}
	}
}
