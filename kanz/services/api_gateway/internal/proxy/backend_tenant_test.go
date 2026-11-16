package proxy

import (
	"strings"
	"testing"
)

// A TENANT WITH ITS OWN BOOK IS READ FROM ITS OWN BOOK (#668).
//
// The failure this guards is not an outage. The gateway dials one address per
// service, and for a tenant with its own rendered instance that address is the
// __system__ one — so a read of acme's NAV would answer 200 with the PLATFORM
// book's numbers. Not empty: another tenant's fills, returned to an
// authenticated acme caller as their own.

const sharedAccounting = "http://accounting.kanz-services.svc:8101"

func mustUpstreams(t *testing.T, tenants ...string) *tenantUpstreams {
	t.Helper()
	u, err := newTenantUpstreams(tenants)
	if err != nil {
		t.Fatalf("newTenantUpstreams(%v): %v", tenants, err)
	}
	return u
}

func TestAPerTenantServiceIsDialledAtTheTenantsOwnInstance(t *testing.T) {
	u := mustUpstreams(t, "acme")
	got, rewrote := u.resolve(sharedAccounting, ServiceAccounting, "acme")
	if !rewrote {
		t.Fatal("acme has its own accounting instance and the shared address was used — that read " +
			"answers from the __system__ book")
	}
	const want = "http://accounting-acme.kanz-services.svc:8101"
	if got != want {
		t.Fatalf("resolve = %q, want %q", got, want)
	}
}

// THE PORT SURVIVES. accounting's API is on :8101 (its metrics listener is
// :8080, and the split is #447's) — a rewrite that dropped the port would dial
// :80 and fail, or worse, reach a different listener.
func TestTheRewriteKeepsThePort(t *testing.T) {
	u := mustUpstreams(t, "acme")
	got, _ := u.resolve(sharedAccounting, ServiceAccounting, "acme")
	if !strings.HasSuffix(got, ":8101") {
		t.Fatalf("resolve = %q, which lost the port", got)
	}
}

// ONLY THE FIRST HOSTNAME LABEL IS REWRITTEN. A substring replace would rewrite
// every occurrence of the service name — including one inside the namespace —
// and the result resolves to nothing at all.
func TestTheRewriteTouchesOnlyTheFirstLabel(t *testing.T) {
	u := mustUpstreams(t, "acme")
	got, _ := u.resolve("http://accounting.accounting-ns.svc:8101", ServiceAccounting, "acme")
	const want = "http://accounting-acme.accounting-ns.svc:8101"
	if got != want {
		t.Fatalf("resolve = %q, want %q — only the first label may be renamed", got, want)
	}
}

// EVERYTHING ELSE KEEPS THE SHARED ADDRESS, and each of these is a live case:
// __system__ has no rendered compute, an un-onboarded tenant has none either,
// and most services are not rendered per tenant at all.
func TestTheSharedAddressIsKeptForEverythingElse(t *testing.T) {
	u := mustUpstreams(t, "acme")
	for _, tc := range []struct {
		name   string
		svc    Service
		tenant string
	}{
		{"the platform tenant", ServiceAccounting, "__system__"},
		{"a tenant with no rendered compute", ServiceAccounting, "globex"},
		{"a service not rendered per tenant", ServiceDataMaster, "acme"},
		{"no principal tenant at all", ServiceAccounting, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, rewrote := u.resolve(sharedAccounting, tc.svc, tc.tenant)
			if rewrote || got != sharedAccounting {
				t.Fatalf("resolve = %q (rewrote=%v), want the shared address unchanged", got, rewrote)
			}
		})
	}
}

// A NIL RESOLVER IS THE ZERO VALUE and must behave as "no tenant has its own
// instance" rather than panic: MeshBackend is constructed without one by every
// caller that does not opt in, and Forward calls through it unconditionally.
func TestANilResolverKeepsTheSharedAddress(t *testing.T) {
	var u *tenantUpstreams
	got, rewrote := u.resolve(sharedAccounting, ServiceAccounting, "acme")
	if rewrote || got != sharedAccounting {
		t.Fatalf("nil resolver returned %q (rewrote=%v)", got, rewrote)
	}
}

// A TENANT ID tenantgen WOULD REFUSE IS REFUSED HERE. It renders no manifest, so
// routing to it could only ever 503 — and the operator's actual mistake is a typo
// in an env var, which the boot error can name and a 503 cannot.
func TestAnInvalidTenantIsRefusedAtConstruction(t *testing.T) {
	for _, bad := range []string{"ACME", "acme_1", "-acme", "acme."} {
		if _, err := newTenantUpstreams([]string{bad}); err == nil {
			t.Errorf("tenant %q was accepted; tenantgen would refuse to render it", bad)
		}
	}
}

// Blank entries are ignored rather than refused: env.SplitList over a trailing
// comma is an ordinary way to write the list, not an operator error.
func TestBlankEntriesAreIgnored(t *testing.T) {
	u := mustUpstreams(t, "acme", "", "  ")
	if len(u.tenants) != 1 || !u.tenants["acme"] {
		t.Fatalf("tenants = %v, want just acme", u.tenants)
	}
}

// THE PER-TENANT SERVICE SET IS DERIVED, NOT RESTATED. It comes from
// internal/tenantgen.Services — the package that actually renders them — so a
// service added there is routed per tenant without a second edit here. This
// pins that the derivation is live rather than an empty map that would silently
// route everything to the shared address.
func TestThePerTenantServiceSetComesFromTenantgen(t *testing.T) {
	u := mustUpstreams(t, "acme")
	if len(u.perTenant) == 0 {
		t.Fatal("no per-tenant services derived — the tenantgen list is not being read, and every " +
			"tenant would silently read the platform book")
	}
	if !u.perTenant[ServiceAccounting] {
		t.Error("accounting is rendered per tenant (internal/tenantgen.Services) and is not in the " +
			"derived set")
	}
}
