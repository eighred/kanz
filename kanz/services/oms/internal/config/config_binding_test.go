package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/execution"
)

// THE OMS REFUSES TO START WHEN TWO PORTFOLIOS SHARE AN EXCHANGE ACCOUNT (#68).
//
// This is the half of #68's "Verified when" that does not need a cluster, and
// until now nothing ran it.
//
// WHY IT MATTERS. An exchange margins, nets and LIQUIDATES per account. Two
// portfolios bound to one exchange account sit in one collateral pool, so a
// drawdown in the first consumes the second's margin — while each portfolio's
// ledger still shows its own cash sitting there. Segregated books over a shared
// pool is the failure the deploy-time basket contract exists to prevent, and the
// refusal to start is the entire mechanism: there is no runtime API to rebind a
// portfolio (test/arch/basket_contract_test.go keeps that true), so startup is
// the only place this can be caught.
//
// WHAT WAS ACTUALLY UNTESTED. internal/execution's own test proves ParseBindings
// RETURNS ErrAccountShared. That is the parser. Nothing proved the OMS refuses to
// START — which needs the composition root to call it, treat the error as fatal,
// and exit. That call sat ~200 lines into startup, after the OMS had connected to
// NATS and opened Postgres, so reaching it at all required a live broker and
// database. The guarantee the basket contract rests on had no test that ran it.
//
// It is validated at Load now — ParseBindings is a pure function of a string
// already in hand — so the refusal is both the FIRST thing that fails and
// something a test can exercise with no I/O whatsoever.

// bindingEnv sets the minimum an OMS needs to Load, plus the binding under test.
// Everything here is a value; nothing opens a socket or a file.
func bindingEnv(t *testing.T, bindings string) {
	t.Helper()
	t.Setenv("OMS_VENUE_ACCOUNTS", bindings)
}

// ONE ACCOUNT, TWO PORTFOLIOS ⇒ Load REFUSES, and names the account.
func TestLoad_RefusesAnExchangeAccountSharedByTwoPortfolios(t *testing.T) {
	bindingEnv(t, "acme/fund-alpha@XNAS=okx-sub-1,acme/fund-beta@XNAS=okx-sub-1")

	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted a config binding ONE exchange account to TWO portfolios.\n" +
			"They would margin against one collateral pool while each portfolio's ledger " +
			"reported its own cash — a drawdown in one silently consuming the other's margin. " +
			"The OMS must refuse to start (#68)")
	}
	if !errors.Is(err, execution.ErrAccountShared) {
		t.Fatalf("err = %v, want ErrAccountShared — the refusal must be attributable to the "+
			"shared account rather than to some other config fault", err)
	}
	// AND IT NAMES THE ACCOUNT. An operator handed "config invalid" has to go
	// looking; the whole value of failing at startup is that it says which line.
	if !strings.Contains(err.Error(), "okx-sub-1") {
		t.Errorf("refusal %q does not name the shared account", err)
	}
}

// AN ACCOUNT NAME IS GLOBAL, NOT VENUE-SCOPED, and the same name at two venues is
// REFUSED. I expected the opposite when writing this and was wrong; the invariant
// is deliberate and stated at internal/execution/account.go — "An account may
// have exactly one owner."
//
// It is the conservative reading and it is the right one. A real exchange account
// belongs to exactly one exchange, so the same identifier appearing at two venues
// is a naming collision rather than two genuine pools — and the two possible
// mistakes are not symmetric:
//
//   - refuse a safe config ⇒ an operator renames an account and redeploys;
//   - accept an unsafe one ⇒ two portfolios margin against one pool while both
//     ledgers report their own cash, which is the failure the whole binding
//     mechanism exists to prevent and which nothing downstream would notice.
//
// Pinned so nobody "fixes" this into venue-scoping to make a generically-named
// estate ("main" at both Binance and OKX) start without renaming anything.
func TestLoad_RefusesTheSameAccountNameAcrossVenues(t *testing.T) {
	bindingEnv(t, "acme/fund-alpha@XNAS=okx-sub-1,acme/fund-beta@XLON=okx-sub-1")

	_, err := Load()
	if !errors.Is(err, execution.ErrAccountShared) {
		t.Fatalf("err = %v, want ErrAccountShared. An account identifier is global: the same name "+
			"at two venues is a collision, and admitting it risks two portfolios sharing one "+
			"collateral pool. Rename the account rather than scoping the check by venue", err)
	}
}

// A CLEAN BINDING SET LOADS. Required as the companion to the refusal above: a
// check that refuses everything is a trading outage wearing the shape of a
// control, and it would pass a test that only asserted the refusal.
func TestLoad_AcceptsSegregatedBindings(t *testing.T) {
	bindingEnv(t, "acme/fund-alpha@XNAS=okx-sub-1,acme/fund-beta@XNAS=okx-sub-2")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load refused a properly segregated binding set: %v", err)
	}
	if cfg.VenueAccounts == "" {
		t.Error("VenueAccounts did not survive Load")
	}
}

// EMPTY IS ALLOWED, AND IS NOT THE SAME AS SAFE. Nothing bound means every
// portfolio trades whatever account its adapter holds — one shared pool per
// venue. That is a real deployment posture (it is what the estate ships), so
// Load must not refuse it; the OMS WARNs and counts every such order instead.
//
// Pinned here because the tempting "fix" for the refusal above is to require
// bindings, which would refuse every deployment that has not been bound yet.
func TestLoad_AcceptsNoBindingsBecauseThatIsAPosture(t *testing.T) {
	bindingEnv(t, "")

	if _, err := Load(); err != nil {
		t.Fatalf("Load refused an EMPTY binding set: %v.\nNothing bound is the shipped posture — "+
			"loud and counted, not fatal", err)
	}
}

// A MALFORMED BINDING IS ALSO FATAL, not silently skipped. A typo that drops one
// binding leaves that portfolio unbound — trading a shared pool — while the
// config looks like it segregated it.
func TestLoad_RefusesAMalformedBinding(t *testing.T) {
	bindingEnv(t, "this-is-not-a-binding")

	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a malformed binding. A dropped binding leaves its portfolio " +
			"trading a shared collateral pool while the config reads as though it did not")
	}
}
