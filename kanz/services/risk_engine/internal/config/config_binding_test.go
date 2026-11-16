package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/execution"
)

// THE RISK ENGINE REFUSES TO START ON A SHARED EXCHANGE ACCOUNT TOO (#408).
//
// The OMS has refused this since #68, and the reason given there was about
// SPENDING: two portfolios bound to one account sit in one collateral pool, so a
// drawdown in the first consumes the second's margin while each ledger still
// shows its own cash.
//
// The engine reads the same bindings for a different purpose — which account a
// portfolio is LIQUIDATED in — and the same spec is wrong here in its own way. A
// liquidation proximity measured on a shared account attributes one portfolio's
// distance-to-liquidation to another, and the number is not imprecise but WRONG,
// in the direction that reports a portfolio safer than it is. A mandate would
// then pass an order that the collateral cannot support.
//
// VALIDATED AT Load, so this is both the first thing that fails and something a
// test can exercise with no broker, no database and no I/O at all — the same
// argument services/oms/internal/config/config_binding_test.go records.
func TestLoadRefusesASharedVenueAccount(t *testing.T) {
	t.Setenv("RISK_ENGINE_DATABASE_URL", "postgres://x")
	t.Setenv("RISK_ENGINE_VENUE_ACCOUNTS",
		"acme/fund-alpha@XNAS=okx-sub-1,acme/fund-beta@XNAS=okx-sub-1")

	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted a binding set giving ONE exchange account to TWO portfolios. The " +
			"engine would report each of them a liquidation distance measured against collateral " +
			"the other can consume")
	}
	if !errors.Is(err, execution.ErrAccountShared) {
		t.Fatalf("err = %v, want ErrAccountShared — the refusal must be the segregation one rather "+
			"than a parse failure that happens to look like it", err)
	}
	if !strings.Contains(err.Error(), "RISK_ENGINE_VENUE_ACCOUNTS") {
		t.Errorf("error does not name the variable an operator has to fix: %v", err)
	}
}

// A MALFORMED SPEC IS AN ERROR, NOT AN EMPTY BINDING SET. Falling back to "no
// accounts bound" would silently unregister the margin measure — so a typo in
// this variable would take a liquidation control offline and report the same
// posture as a deployment that never wanted one.
func TestLoadRefusesAMalformedBindingSpec(t *testing.T) {
	t.Setenv("RISK_ENGINE_DATABASE_URL", "postgres://x")
	t.Setenv("RISK_ENGINE_VENUE_ACCOUNTS", "acme/fund-alpha=okx-sub-1")

	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a binding that names no venue — the margin measure would silently " +
			"go unregistered on a typo")
	}
}

// AN EMPTY SPEC LOADS. It means nothing is bound, which is a posture rather than
// an error: the composition root registers no margin measure and says so at WARN.
// This is the non-vacuity arm for both refusals above — without it, a Load that
// rejected everything would pass them.
func TestLoadAcceptsNoBindingsAtAll(t *testing.T) {
	t.Setenv("RISK_ENGINE_DATABASE_URL", "postgres://x")
	t.Setenv("RISK_ENGINE_VENUE_ACCOUNTS", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.VenueAccounts != "" {
		t.Errorf("VenueAccounts = %q, want empty", cfg.VenueAccounts)
	}
}

// THE SPEC IS CARRIED THROUGH VERBATIM, whitespace trimmed. The composition root
// re-parses it, so a Load that validated one string and stored another would
// wire the engine to bindings nobody checked.
func TestLoadCarriesTheBindingSpecItValidated(t *testing.T) {
	t.Setenv("RISK_ENGINE_DATABASE_URL", "postgres://x")
	t.Setenv("RISK_ENGINE_VENUE_ACCOUNTS", "  acme/fund-alpha@XNAS=okx-sub-1  ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.VenueAccounts != "acme/fund-alpha@XNAS=okx-sub-1" {
		t.Fatalf("VenueAccounts = %q, want the trimmed spec", cfg.VenueAccounts)
	}
	b, err := execution.ParseBindings(cfg.VenueAccounts)
	if err != nil {
		t.Fatalf("the carried spec no longer parses: %v", err)
	}
	if got, ok := b.Account("acme", "fund-alpha", "XNAS"); !ok || got != "okx-sub-1" {
		t.Errorf("Account = %q, %v — the spec Load validated is not the one the engine would wire",
			got, ok)
	}
}
