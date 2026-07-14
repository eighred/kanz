package accountproof

// The adapter is the only process holding the API credential, so it is the only one
// that can prove which exchange account that credential actually spends. These pin
// the four answers it can give — and which of them are allowed to boot.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

type exchange struct {
	uid string
	err error
}

func (e exchange) ExchangeAccountID(context.Context) (string, error) { return e.uid, e.err }

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestProvenAccount: the exchange says the key belongs to 12345678, which is exactly
// what the operator bound this adapter to. It boots, and it can prove it.
func TestProvenAccount(t *testing.T) {
	proof, err := Resolve(context.Background(), exchange{uid: "12345678"}, Want{
		Account:     "binance-main",
		ExchangeUID: "12345678",
	}, quiet())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !proof.Verified || proof.ExchangeAccountID != "12345678" {
		t.Errorf("proof = %+v, want verified 12345678", proof)
	}
}

// TestTheKeyBelongsToSomebodyElse is the failure this whole task exists for.
//
// The adapter is deployed as okx-sub-1 and bound to exchange uid 111. The exchange
// says the key it holds is uid 222 — a DIFFERENT collateral pool, quite possibly
// another fund's. Every fill it produced would be booked to okx-sub-1's ledger rows
// while the exchange margined and liquidated 222. It must not start, and NO flag
// forgives it: this is not an unproven claim, it is a false one.
func TestTheKeyBelongsToSomebodyElse(t *testing.T) {
	for _, allow := range []bool{false, true} {
		proof, err := Resolve(context.Background(), exchange{uid: "222"}, Want{
			Account:         "okx-sub-1",
			ExchangeUID:     "111",
			AllowUnverified: allow,
		}, quiet())
		if err == nil {
			t.Fatalf("allow_unverified=%v: adapter booted holding a DIFFERENT account's credential (%+v)", allow, proof)
		}
		if !errors.Is(err, ErrWrongAccount) {
			t.Errorf("allow_unverified=%v: err = %v, want ErrWrongAccount", allow, err)
		}
	}
}

// TestNoUIDBoundRefusesByDefault: nobody told the adapter which exchange account it is
// supposed to be, so it cannot check. Absence of configuration must never mean "trust
// me" — the brain's no-simulator-reachable-by-omission rule, applied to money.
func TestNoUIDBoundRefusesByDefault(t *testing.T) {
	_, err := Resolve(context.Background(), exchange{uid: "12345678"}, Want{Account: "binance-main"}, quiet())
	if err == nil {
		t.Fatal("adapter booted with no exchange account bound and no explicit permission to skip the check")
	}
	if !errors.Is(err, ErrUnverifiable) {
		t.Errorf("err = %v, want ErrUnverifiable", err)
	}
}

// TestNoUIDBoundIsAnExplicitChoice: an operator who has not bound uids yet can still
// run — but they must SAY so, and the adapter reports the account as unverified so the
// OMS counts it rather than trusting it.
func TestNoUIDBoundIsAnExplicitChoice(t *testing.T) {
	proof, err := Resolve(context.Background(), exchange{uid: "12345678"}, Want{
		Account:         "binance-main",
		AllowUnverified: true,
	}, quiet())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if proof.Verified {
		t.Error("proof.Verified = true for an account that was never checked")
	}
	if proof.ExchangeAccountID != "" {
		t.Errorf("exchange account id = %q, want empty — nothing was verified", proof.ExchangeAccountID)
	}
}

// TestExchangeUnreachableRefusesByDefault: the operator DID bind a uid, so they want
// this checked — and we could not check it. Booting anyway would trade on an
// assumption the deployment explicitly asked us not to make.
func TestExchangeUnreachableRefusesByDefault(t *testing.T) {
	_, err := Resolve(context.Background(), exchange{err: errors.New("503 from the exchange")}, Want{
		Account:     "binance-main",
		ExchangeUID: "12345678",
	}, quiet())
	if !errors.Is(err, ErrUnverifiable) {
		t.Errorf("err = %v, want ErrUnverifiable", err)
	}
}
