package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/marketdata/mark"
)

// EVERY TENANT ACCOUNT IMPORTS ITS OWN ORDERS, AND ONLY ITS OWN (MT-02, #358).
//
// The gateway is the sole front door and holds ONE SVID, therefore one NATS
// account. NATS routes on SUBJECT and cannot dispatch on the envelope's
// tenant_id, so a shared publisher cannot reach a per-tenant account at all —
// measured against a real two-account broker, with a same-account control:
//
//	cross-account (gw -> oms): NONE  <- accounts isolate
//	same-account control     : DELIVERED
//
// The bridge is an export on __system__ plus a per-account import that remaps
// `tenant.<t>.order.order.submit` back to the logical `order.order.submit`, so
// the tenant's OMS subscribes the unchanged name.
//
// TWO WAYS TO GET THIS WRONG, BOTH SILENT:
//
//  1. AN ACCOUNT WITH NO IMPORT. Its OMS authenticates, subscribes, and receives
//     nothing. There is no error anywhere — the orders are delivered to
//     __system__ and the tenant's pod simply sits idle, which looks exactly like
//     a tenant that is not trading.
//
//  2. AN ACCOUNT IMPORTING SOMEONE ELSE'S PREFIX — or a wildcard. That is the
//     isolation boundary this whole epic exists to establish, inverted: one
//     tenant receives another's orders, and the receiving OMS has no way to
//     tell, because its RLS pool is scoped by ITS tenant while the envelope
//     carries the other's.
//
// So the check is exact-match, not "has an import".
var (
	// tenantAccountRe finds `  <name> {` at the tenant-template indent inside the
	// accounts block. __system__ and SYS are matched too and filtered below.
	tenantAccountRe = regexp.MustCompile(`(?m)^      ([A-Za-z_][A-Za-z0-9_-]*) \{`)
	// importSubjectRe pulls the subject out of an import line.
	importSubjectRe = regexp.MustCompile(`subject:\s*"([^"]+)"`)
)

// nonTenantAccounts are the accounts in tenancy.yaml that are not a customer.
// SYS is the NATS system account; __system__ is the reserved platform tenant and
// the EXPORTING side of the bridge, so it must not import from itself.
var nonTenantAccounts = map[string]bool{"SYS": true, "__system__": true}

// bridgedSubjects are the logical subjects the bridge carries today. Kept here
// rather than derived, because the SET is a deliberate scope decision (#358):
// only the order write path has moved. When a second domain is bridged, this
// list and the exports in tenancy.yaml move together or this guard fails.
//
// order.order.approve joined them with #539. It is the same write path — the
// SECOND SIGNATURE that releases a held order — and a tenant that could submit
// across the bridge but not approve would have every large order stick.
// Deliberately still no order.order.amend: the gateway does not publish it, so
// bridging it would open a path nothing uses.
var bridgedSubjects = []string{"order.order.submit", "order.order.cancel", "order.order.approve"}

// platformWideImports are the subjects every tenant account imports UNPREFIXED,
// because there is exactly one of them for the whole estate (#635).
//
// platform.mode.changed is the kill switch. cmd/kanz-halt publishes ONE FACT,
// once, and every account that runs an order path must receive THAT one — a
// per-tenant copy would mean an operator's break-glass command reached the
// platform account and no other, which is the failure #635 exists to close.
//
// It is listed separately from bridgedSubjects rather than folded into it
// because the two have opposite prefixing rules, and the exact-match below has
// to know which rule applies to which subject. A tenant missing this import has
// an OMS that cannot resolve a stream for the halt FACT, latches its gate CLOSED
// and refuses every order for that tenant — loud, and a total outage.
var platformWideImports = []string{"platform.mode.changed"}

// priceSpineImports are the PUBLIC PRICE subjects every tenant account imports
// unprefixed (#955), and they are DERIVED FROM THE FOLD rather than retyped.
//
// mark.DefaultSubjects is what OMS_PRICE_SUBJECTS defaults to and what
// mark.Source folds; writing the subjects out here would make this guard a
// second copy of that decision, and a second copy is what has to be kept in
// step by hand. Derived, the export, the import and the subscription move
// together or this test fails.
//
// WHY A PRICE CROSSES A BOUNDARY AN ORDER DOES NOT. Tenant isolation covers what
// a tenant DID — portfolios, accounts, orders, positions, strategies, risk
// state, mandates, credentials, audit. A trade print and a top-of-book quote say
// what the MARKET did: one book, no tenant, published by __system__ workloads
// (market-data, market-ingest) that no tenant account contains. A per-tenant
// price feed would fold the same public book twice and let two funds disagree
// about the mark that values their orders, their exposure and their positions —
// which is a worse failure than the one it would be isolating against.
//
// UNPREFIXED, LIKE THE HALT AND FOR THE SAME REASON: there is exactly one of
// these for the whole estate, so `tenant.<t>.market.*.trade` would be a private
// copy of a public fact. A tenant missing this import has an OMS that folds an
// EMPTY mark source: every MARKET and STOP order refused PRICE_UNAVAILABLE, no
// arrival price stamped on any order, and both coverage gauges at zero — the one
// reading OMSQuoteCoverageAbsent structurally cannot fire on, because it needs
// marks > 0 to tell a dead feed from a cold pod.
//
// NARROWED TO THE PRICE SPINE. `market.>` also carries book snapshots, bars, the
// ingestion-coverage FACT and the volume profile; those are a separate decision
// (#959) and deliberately do not cross here.
var priceSpineImports = append([]string(nil), mark.DefaultSubjects...)

func tenancyText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(moduleRoot(t), "infra", "nats", "tenancy.yaml"))
	if err != nil {
		t.Fatalf("read tenancy.yaml: %v", err)
	}
	return string(b)
}

// accountBlocks splits tenancy.yaml into name -> body for the accounts at the
// template indent.
func accountBlocks(t *testing.T, text string) map[string]string {
	t.Helper()
	locs := tenantAccountRe.FindAllStringSubmatchIndex(text, -1)
	out := map[string]string{}
	for i, loc := range locs {
		name := text[loc[2]:loc[3]]
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		out[name] = text[loc[1]:end]
	}
	return out
}

func TestEveryTenantAccountImportsExactlyItsOwnOrders(t *testing.T) {
	text := tenancyText(t)
	blocks := accountBlocks(t, text)

	var tenants []string
	for name := range blocks {
		if !nonTenantAccounts[name] {
			tenants = append(tenants, name)
		}
	}
	sort.Strings(tenants)

	// NON-VACUITY: with no tenant accounts this guard asserts nothing, and the
	// estate would look bridged because nothing contradicted it.
	if len(tenants) == 0 {
		t.Fatal("no tenant accounts parsed from tenancy.yaml — either the file was " +
			"restructured or the template indent changed, and this guard is now blind")
	}

	for _, tenant := range tenants {
		body := blocks[tenant]
		var found []string
		for _, m := range importSubjectRe.FindAllStringSubmatch(body, -1) {
			found = append(found, m[1])
		}
		var want []string
		for _, subj := range bridgedSubjects {
			want = append(want, fmt.Sprintf("tenant.%s.%s", tenant, subj))
		}
		want = append(want, platformWideImports...)
		want = append(want, priceSpineImports...)
		sort.Strings(found)
		sort.Strings(want)

		if strings.Join(found, ",") != strings.Join(want, ",") {
			t.Errorf("tenant account %q imports %v, want exactly %v.\n\n"+
				"An account with no import has an OMS that authenticates, subscribes and receives "+
				"NOTHING — silently, because the orders are delivered to __system__ and the pod "+
				"just looks idle. An account importing another tenant's prefix is the isolation "+
				"boundary inverted, and the receiving OMS cannot detect it: its RLS pool is scoped "+
				"by ITS tenant while the envelope carries the other's.\n\n"+
				"A MISSING PRICE-SPINE IMPORT (%v) FAILS THE SAME WAY ONE DOMAIN OVER (#955): the "+
				"OMS holds a market.*.trade/quote SUBSCRIBE grant, subscribes it, and folds an "+
				"empty mark source forever — every MARKET and STOP order refused PRICE_UNAVAILABLE "+
				"while every probe reads Ready.",
				tenant, found, want, priceSpineImports)
		}
	}
}

// TestSystemAccountCarriesTheBridge pins the exporting half.
//
// Without the export, no tenant import resolves and NATS accepts the config
// anyway — every tenant silently receives nothing. Without the __system__
// self-mapping, the CURRENT single-tenant path breaks the moment the gateway
// starts prefixing: the running OMS subscribes `order.order.submit` and would
// stop receiving orders, with no error on either side.
func TestSystemAccountCarriesTheBridge(t *testing.T) {
	text := tenancyText(t)
	sys, ok := accountBlocks(t, text)["__system__"]
	if !ok {
		t.Fatal("no __system__ account block found in tenancy.yaml — this guard is blind")
	}

	for _, subj := range platformWideImports {
		export := fmt.Sprintf(`stream: "%s"`, subj)
		if !strings.Contains(sys, export) {
			t.Errorf("__system__ does not export %s.\n\n"+
				"Every tenant's import of the platform kill switch resolves to nothing, the broker "+
				"accepts the config, and that tenant's OMS cannot hear a declared halt — or, because "+
				"the gate is deny-by-default, refuses every order instead.", export)
		}
	}

	// THE PRICE SPINE'S EXPORTING HALF (#955). Without it every tenant's import
	// resolves to nothing, nats-server accepts the file, and each tenant OMS
	// subscribes a subject space no message ever lands in — which is not an error
	// anywhere, just a pod that folds no marks and refuses every MARKET order.
	for _, subj := range priceSpineImports {
		export := fmt.Sprintf(`stream: "%s"`, subj)
		if !strings.Contains(sys, export) {
			t.Errorf("__system__ does not export %s.\n\n"+
				"market-data and market-ingest publish the price spine in THIS account, and no "+
				"workload in a tenant account publishes a market subject at all. Without the "+
				"export, a tenant-dedicated OMS folds an empty mark source: every MARKET and STOP "+
				"order refused PRICE_UNAVAILABLE, no arrival price on any order, and the coverage "+
				"gauges pinned at zero — the one reading OMSQuoteCoverageAbsent cannot fire on.", export)
		}
	}

	for _, subj := range bridgedSubjects {
		export := fmt.Sprintf(`stream: "tenant.*.%s"`, subj)
		if !strings.Contains(sys, export) {
			t.Errorf("__system__ does not export %s.\n\n"+
				"Every tenant's import of it resolves to nothing, the broker accepts the config, "+
				"and each tenant OMS receives no orders at all.", export)
		}
		mapping := fmt.Sprintf(`"tenant.__system__.%s": "%s"`, subj, subj)
		if !strings.Contains(sys, mapping) {
			t.Errorf("__system__ does not map %s back to itself.\n\n"+
				"The gateway prefixes unconditionally, so without this the platform tenant's own "+
				"orders are published to a subject nothing subscribes — and the OMS running today "+
				"stops receiving orders the moment this ships.", mapping)
		}
	}
}
