package execution

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// COLLATERAL IS SEGREGATED BY EXCHANGE ACCOUNT, NOT BY PORTFOLIO.
//
// The ledger has always kept cash per PORTFOLIO. The exchange does not. An exchange
// margins, nets and LIQUIDATES per ACCOUNT — the OKX sub-account, the Binance account
// behind one API credential. Two portfolios settling into one account share one
// collateral pool, and there is no ledger entry that can undo that: when a drawdown in
// the first triggers a liquidation, the exchange sells whatever is in the account, and
// the second portfolio's margin is gone. Its books would still show the cash. That is
// not an imprecise number; it is a WRONG one, and it is wrong in the direction that
// loses money.
//
// So the platform has to be able to say two things it could not say before:
//
//	WHICH account did this order execute against?   (a FACT — venue_account_id)
//	WHICH accounts is this portfolio ALLOWED to use? (a CONTROL — the bindings below)
//
// The account is a physical fact already: one venue adapter deployment holds one API
// credential and therefore IS one exchange account. It was simply never NAMED, so
// nothing could be bound to it and nothing could be refused.
//
// # The bindings are a DEPLOY-TIME contract. There is no runtime CRUD (#68)
//
// Owner ruling, 2026-07-27: a basket change is a deployment. Edit
// OMS_VENUE_ACCOUNTS, redeploy, and the OMS re-reads the whole set at startup.
// There is deliberately no endpoint that rebinds a portfolio in a running
// process, and adding one is not a feature this is missing.
//
// THE REASON IS THE REFUSAL BELOW. ParseBindings rejects an account with two
// owners and the OMS exits 2 rather than starting (services/oms/cmd/oms:257).
// That check is only worth something while the binding set is FIXED for the life
// of the process. A runtime rebind moves it to a moment nobody is watching: the
// process is already up, already holding orders, and the configuration it was
// checked against at boot no longer exists. The window between "rebound" and
// "noticed" is one where the platform reports segregated books over a shared
// collateral pool — which is the exact state this whole file exists to make
// impossible.
//
// Changing a binding by redeploying is not a workaround for a missing API. It is
// what makes the guarantee checkable: every binding set that has ever been live
// passed the same refusal, at a moment when refusing cost nothing.
//
// test/arch/basket_contract_test.go enforces it against the SCHEMA — the gateway
// is grpc-gateway, so a runtime CRUD surface would arrive as a proto RPC, and it
// fails there before an implementation exists to argue about.
//
// AccountBindings is the control. Its one hard invariant is exclusivity: an account
// belongs to AT MOST ONE portfolio. Bind two portfolios to one account and their
// collateral is shared — the exact thing this type exists to prevent — so that is not
// a warning, it is a construction error, and the OMS refuses to start on it.

// ErrAccountShared is returned when a binding spec gives one exchange account to two
// different portfolios. It is a capital-safety violation, not a typo: the platform
// would report segregated books over a shared collateral pool.
var ErrAccountShared = errors.New("execution: venue account is bound to more than one portfolio (their collateral would be shared)")

// bindingKey is the routing dimension: a portfolio's account AT a given venue. One
// portfolio holds at most one account per venue; it may hold accounts at many venues.
type bindingKey struct {
	tenant    string
	portfolio string
	mic       string
}

// AccountBindings maps (tenant, portfolio, venue) → exchange account. It is
// immutable once built.
type AccountBindings struct {
	byKey   map[bindingKey]string
	byOwner map[string]bindingKey // account → the ONE portfolio that owns it
}

// ParseBindings reads the binding spec:
//
//	tenant/portfolio@MIC=account,tenant/portfolio@MIC=account
//	acme/fund-alpha@XNAS=okx-sub-1,acme/fund-beta@XLON=binance-main
//
// An empty spec yields empty bindings — meaning NOTHING is bound, not that everything
// is permitted. What that implies is the caller's decision (see the OMS's
// OMS_REQUIRE_VENUE_ACCOUNT posture), because refusing every order for every portfolio
// nobody has bound yet is a trading outage, and it must be chosen deliberately.
func ParseBindings(spec string) (*AccountBindings, error) {
	b := &AccountBindings{
		byKey:   map[bindingKey]string{},
		byOwner: map[string]bindingKey{},
	}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lhs, account, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("execution: malformed binding %q, want tenant/portfolio@MIC=account", part)
		}
		scope, mic, ok := strings.Cut(lhs, "@")
		if !ok {
			return nil, fmt.Errorf("execution: binding %q names no venue, want tenant/portfolio@MIC=account", part)
		}
		tenant, portfolio, ok := strings.Cut(scope, "/")
		if !ok {
			return nil, fmt.Errorf("execution: binding %q names no tenant, want tenant/portfolio@MIC=account", part)
		}
		key := bindingKey{
			tenant:    strings.TrimSpace(tenant),
			portfolio: strings.TrimSpace(portfolio),
			mic:       strings.TrimSpace(mic),
		}
		account = strings.TrimSpace(account)
		if key.tenant == "" || key.portfolio == "" || key.mic == "" || account == "" {
			return nil, fmt.Errorf("execution: binding %q has an empty field", part)
		}
		if existing, dup := b.byKey[key]; dup && existing != account {
			return nil, fmt.Errorf("execution: portfolio %s/%s is bound to two accounts at %s (%q and %q)",
				key.tenant, key.portfolio, key.mic, existing, account)
		}

		// THE INVARIANT. An account may have exactly one owner. Anything else is a
		// shared collateral pool with a segregated ledger on top of it.
		if owner, taken := b.byOwner[account]; taken && owner != key {
			return nil, fmt.Errorf("%w: account %q is bound to %s/%s and to %s/%s",
				ErrAccountShared, account, owner.tenant, owner.portfolio, key.tenant, key.portfolio)
		}
		b.byKey[key] = account
		b.byOwner[account] = key
	}
	return b, nil
}

// Account returns the exchange account this portfolio may use at this venue.
func (b *AccountBindings) Account(tenant, portfolio, mic string) (string, bool) {
	if b == nil {
		return "", false
	}
	a, ok := b.byKey[bindingKey{tenant: tenant, portfolio: portfolio, mic: mic}]
	return a, ok
}

// Empty reports whether nothing is bound at all.
func (b *AccountBindings) Empty() bool { return b == nil || len(b.byKey) == 0 }

// Len is the number of bindings.
func (b *AccountBindings) Len() int {
	if b == nil {
		return 0
	}
	return len(b.byKey)
}

// Accounts lists every bound account, sorted. Startup logs the list, because "which
// accounts can this OMS spend from" should be answerable from the boot log of the
// process that spends from them.
func (b *AccountBindings) Accounts() []string {
	if b == nil {
		return nil
	}
	out := make([]string, 0, len(b.byOwner))
	for a := range b.byOwner {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}
