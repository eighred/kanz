package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// THE FUNDER'S SENTENCE, END TO END (#415).
//
// The owner's example is "1,000 USDT sits on the exchange; allocate 100 to one
// portfolio". The half that was missing is WHERE: a subscription could say "PF
// has 100 USDT" and nothing recorded which exchange account it landed in.
//
// This drives the HTTP surface a funder actually calls, because the field is
// only useful if it survives every hop — request body → CashMovement →
// LedgerEntry FACT → decodeCash → ledger.Event → ledger_entries.venue_account_id.
// The producer and consumer halves are pinned in their own packages; this is the
// one that fails if the handler forgets to thread it, which is the easiest of
// the three to forget and the least visible.

func TestCashMovementCarriesTheVenueAccountToThePublisher(t *testing.T) {
	pub := &fakeCashPublisher{}
	r := &Readiness{}
	r.Set(true)
	s := New(r, nil, ledger.NewMemoryStore(), "USD", WithTenant(testTenant), WithCashPublisher(pub))

	body := `{"movement_id":"S1","kind":"subscription","amount":"100","currency":"USDT",` +
		`"venue_account_id":"okx-sub-1"}`
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, postV1(http.MethodPost, "/v1/portfolios/PF1/cash-movements", strings.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202 got %d (%s)", rec.Code, rec.Body.String())
	}
	if got := pub.got.VenueAccountID; got != "okx-sub-1" {
		t.Fatalf("published movement VenueAccountID = %q, want %q.\n"+
			"The handler dropped the account between the request body and the FACT, so the "+
			"ledger records the cash and not where it landed — and '' is read downstream as "+
			"the positive claim that it touched no exchange account.", got, "okx-sub-1")
	}
}

// AN OMITTED ACCOUNT STAYS OMITTED. Migration 0003 reads the EMPTY STRING as
// "this entry touched no exchange account", which is the truth for an investor subscription
// into the fund's own bank — so the handler must not default it to anything,
// including the portfolio id or a configured venue.
func TestCashMovementWithoutAVenueAccountStaysUnscoped(t *testing.T) {
	pub := &fakeCashPublisher{}
	r := &Readiness{}
	r.Set(true)
	s := New(r, nil, ledger.NewMemoryStore(), "USD", WithTenant(testTenant), WithCashPublisher(pub))

	body := `{"movement_id":"S2","kind":"subscription","amount":"100"}`
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, postV1(http.MethodPost, "/v1/portfolios/PF1/cash-movements", strings.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202 got %d (%s)", rec.Code, rec.Body.String())
	}
	if got := pub.got.VenueAccountID; got != "" {
		t.Fatalf("VenueAccountID = %q, want empty — an unscoped movement must declare that it "+
			"touched no exchange account, never a guessed one", got)
	}
}
