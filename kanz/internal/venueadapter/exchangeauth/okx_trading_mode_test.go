package exchangeauth

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// THE HEADER IS THE ONLY THING THAT SEPARATES DEMO FROM REAL MONEY AT OKX.
//
// OKX publishes no demo hostname: demo and production are both www.okx.com and
// are told apart per-request by `x-simulated-trading: 1` (#147). So these tests
// are not testing a convenience flag — they are testing the entire boundary
// between a simulated fill and a real one, and there is no second control behind
// them.
//
// They assert on the REQUEST HEADERS rather than on config, deliberately. #147's
// defect was that config and comments described a demo path the wire never had;
// asserting the thing that actually leaves the process is what makes that class
// of gap impossible to repeat here.
func TestSignOKXSendsSimulatedTradingHeaderOnlyInDemo(t *testing.T) {
	cred := Credential{APIKey: "k", APISecret: "s", Passphrase: "p"}
	ts := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	t.Run("demo sets the header", func(t *testing.T) {
		h := http.Header{}
		if err := SignOKX(h, cred, ts, http.MethodPost, "/api/v5/trade/order", `{"a":1}`, OKXDemo); err != nil {
			t.Fatalf("SignOKX(demo): %v", err)
		}
		if got := h.Get("x-simulated-trading"); got != "1" {
			t.Errorf("x-simulated-trading = %q, want \"1\" — without it OKX routes this order to the "+
				"LIVE book regardless of the URL, which is exactly the #147 defect", got)
		}
	})

	t.Run("live does not set the header", func(t *testing.T) {
		h := http.Header{}
		if err := SignOKX(h, cred, ts, http.MethodPost, "/api/v5/trade/order", `{"a":1}`, OKXLive); err != nil {
			t.Fatalf("SignOKX(live): %v", err)
		}
		if got, ok := h["X-Simulated-Trading"]; ok {
			t.Errorf("live request carries x-simulated-trading = %v — a live order silently placed on "+
				"the demo book is the opposite failure, and just as wrong", got)
		}
	})

	// A REUSED HEADER MAP MUST NOT CARRY DEMO INTO A LIVE REQUEST. This is why
	// SignOKX Deletes rather than merely not-Setting: the demo→live direction is
	// the one nobody would report, because the operator believes orders are real
	// and they are not.
	t.Run("live clears a stale demo header", func(t *testing.T) {
		h := http.Header{}
		if err := SignOKX(h, cred, ts, http.MethodGet, "/api/v5/account/config", "", OKXDemo); err != nil {
			t.Fatalf("SignOKX(demo): %v", err)
		}
		if err := SignOKX(h, cred, ts, http.MethodGet, "/api/v5/account/config", "", OKXLive); err != nil {
			t.Fatalf("SignOKX(live): %v", err)
		}
		if got, ok := h["X-Simulated-Trading"]; ok {
			t.Errorf("x-simulated-trading survived a re-sign as live: %v", got)
		}
	})

	// The signature must not depend on the mode — OKX signs timestamp+method+path+
	// body, and nothing else. If a mode change moved the signature, demo and live
	// would fail for a reason that looks like a credential problem.
	t.Run("mode does not alter the signature", func(t *testing.T) {
		demo, live := http.Header{}, http.Header{}
		if err := SignOKX(demo, cred, ts, http.MethodPost, "/p", "b", OKXDemo); err != nil {
			t.Fatal(err)
		}
		if err := SignOKX(live, cred, ts, http.MethodPost, "/p", "b", OKXLive); err != nil {
			t.Fatal(err)
		}
		if demo.Get("OK-ACCESS-SIGN") != live.Get("OK-ACCESS-SIGN") {
			t.Error("OK-ACCESS-SIGN differs between demo and live for identical inputs — the mode is a " +
				"routing header, not part of the signed prehash")
		}
	})
}

// AN UNSTATED MODE IS REFUSED, NOT DEFAULTED. This is the assertion that keeps
// #147 from returning by omission: the failure mode was never a wrong value, it
// was the absence of the question. A future caller that forgets the argument
// cannot compile; one that passes a zero value gets an error rather than a live
// order.
func TestSignOKXRefusesAnUnstatedMode(t *testing.T) {
	cred := Credential{APIKey: "k", APISecret: "s", Passphrase: "p"}
	ts := time.Now()

	for _, mode := range []OKXTradingMode{"", "LIVE", "Demo", "simulated", "testnet", "true"} {
		t.Run(string("mode="+mode), func(t *testing.T) {
			h := http.Header{}
			err := SignOKX(h, cred, ts, http.MethodPost, "/api/v5/trade/order", "{}", mode)
			if err == nil {
				t.Fatalf("SignOKX accepted %q — anything that is not exactly %q or %q must be refused, "+
					"because the alternative is guessing whether an order is real",
					string(mode), string(OKXLive), string(OKXDemo))
			}
			if !errors.Is(err, ErrUnknownOKXTradingMode) {
				t.Errorf("error = %v, want ErrUnknownOKXTradingMode so callers can classify it", err)
			}
			// Nothing may be written to the header on refusal: a partially-signed
			// request that a caller ignores the error from must not be sendable.
			if len(h) != 0 {
				t.Errorf("refused sign still wrote %d header(s): %v", len(h), h)
			}
		})
	}
}
