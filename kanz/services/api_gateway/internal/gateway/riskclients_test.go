package gateway

import (
	"testing"

	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
)

// A TENANT WITH ITS OWN RISK ENGINE IS QUERIED ON IT (#668).
//
// The failure is not an outage: answering a tenant from the platform engine
// returns ANOTHER book's exposure and VaR, with a 200, to an authenticated
// caller — and a risk number is what a limit is checked against.

// stubRisk is a distinguishable RiskQueryServiceClient. Only identity matters
// here, so the interface is satisfied by embedding and never called.
type stubRisk struct {
	querypb.RiskQueryServiceClient
	name string
}

func TestATenantWithItsOwnEngineIsNotAnsweredFromThePlatform(t *testing.T) {
	platform := &stubRisk{name: "platform"}
	acme := &stubRisk{name: "acme"}

	r, err := newRiskClients(platform).withPerTenant(map[string]querypb.RiskQueryServiceClient{"acme": acme})
	if err != nil {
		t.Fatalf("withPerTenant: %v", err)
	}
	got, ok := r.clientFor("acme").(*stubRisk)
	if !ok || got.name != "acme" {
		t.Fatalf("acme was answered from %v — that is the platform's positions returned as the "+
			"tenant's own risk", got)
	}
}

// EVERYTHING ELSE FALLS BACK TO THE PLATFORM ENGINE, and that is correct rather
// than a compromise: a tenant with no rendered engine has no book of its own,
// and __system__'s engine is the one holding its positions.
func TestTenantsWithoutTheirOwnEngineUseThePlatform(t *testing.T) {
	platform := &stubRisk{name: "platform"}
	r, err := newRiskClients(platform).withPerTenant(map[string]querypb.RiskQueryServiceClient{"acme": &stubRisk{name: "acme"}})
	if err != nil {
		t.Fatalf("withPerTenant: %v", err)
	}
	for _, tenant := range []string{"__system__", "globex", ""} {
		got, ok := r.clientFor(tenant).(*stubRisk)
		if !ok || got.name != "platform" {
			t.Errorf("tenant %q resolved to %v, want the platform engine", tenant, got)
		}
	}
}

// A NIL PLATFORM CLIENT IS ACCEPTED, and this pins the contract rather than
// merely recording current behaviour. gateway.New has always tolerated one —
// several tests build a Handler with no risk upstream to exercise the approval
// and order routes — and the risk routes then panic only if exercised, exactly
// as they did when the Handler held the client directly. Refusing it here would
// break every such caller, which is a different change from adding per-tenant
// routing.
func TestANilPlatformClientIsTolerated(t *testing.T) {
	r := newRiskClients(nil)
	if r.clientFor("acme") != nil {
		t.Fatal("a nil platform client resolved to something")
	}
}

// A NIL PER-TENANT CLIENT IS REFUSED, and this is the sharper case: it would
// panic on the first query FOR THAT TENANT ONLY — a crash reachable by one
// tenant's traffic and by no test that does not use that tenant.
func TestANilPerTenantClientIsRefused(t *testing.T) {
	_, err := newRiskClients(&stubRisk{name: "platform"}).
		withPerTenant(map[string]querypb.RiskQueryServiceClient{"acme": nil})
	if err == nil {
		t.Fatal("a nil per-tenant risk client was accepted")
	}
}

// AN EMPTY MAP IS THE CORRECT DEFAULT — a deployment that has onboarded no
// tenant — and must not be confused with a broken registry.
func TestNoPerTenantEnginesIsAValidRegistry(t *testing.T) {
	r, err := newRiskClients(&stubRisk{name: "platform"}).withPerTenant(nil)
	if err != nil {
		t.Fatalf("an empty registry was refused: %v", err)
	}
	if got, ok := r.clientFor("acme").(*stubRisk); !ok || got.name != "platform" {
		t.Fatalf("clientFor returned %v on an empty registry", got)
	}
}

// The registry copies the map it is given, so a caller mutating its own map
// afterwards cannot change which engine a tenant is routed to at runtime. That
// is the property that makes the lock-free concurrent reads safe.
func TestTheRegistryDoesNotAliasTheCallersMap(t *testing.T) {
	src := map[string]querypb.RiskQueryServiceClient{"acme": &stubRisk{name: "acme"}}
	r, err := newRiskClients(&stubRisk{name: "platform"}).withPerTenant(src)
	if err != nil {
		t.Fatalf("withPerTenant: %v", err)
	}
	src["acme"] = &stubRisk{name: "hijacked"}
	if got, ok := r.clientFor("acme").(*stubRisk); !ok || got.name != "acme" {
		t.Fatalf("clientFor returned %v after the caller's map was mutated — the registry aliases "+
			"it, so the routing table is writable after construction", got)
	}
}
