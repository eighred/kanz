package arch

import (
	"sort"
	"strings"
	"testing"
)

// A TENANT LEFT OUT OF THE GATEWAY'S LIST READS THE PLATFORM'S BOOK (#668).
//
// # The asymmetry
//
// MT-02 renders some services per tenant (internal/tenantgen.Services), each
// with its own Service object, database scope and NATS account. The gateway
// dials ONE address per service, so it needs to know which tenants have their
// own instance — and it cannot read infra/deploy/tenants/ at runtime, so the
// list arrives as API_GATEWAY_PER_TENANT_UPSTREAMS.
//
// The two ways to get it wrong are not equally bad:
//
//	TENANT WRONGLY INCLUDED  the gateway dials <service>-<tenant>, no such
//	                         Service exists, the read 503s. Loud, traceable,
//	                         fixed in a minute.
//	TENANT WRONGLY OMITTED   the gateway dials the SHARED address, which is the
//	                         __system__ instance. The read succeeds. A query for
//	                         that tenant's NAV, cash or ledger returns 200
//	                         carrying the PLATFORM book's numbers — another
//	                         tenant's fills, presented to an authenticated
//	                         caller as their own.
//
// The second is the one this guard exists for. Nothing at runtime can catch it:
// the address resolves, the upstream is healthy, the response is well-formed,
// and the only thing wrong with it is whose money it describes.
//
// # Why the comparison lives here
//
// The two facts are in different places — a rendered directory under
// infra/deploy/tenants/ and an env var in infra/deploy/api-gateway-deploy.yaml —
// and no process reads both. This is where they are read together.
func TestGatewayTenantUpstreamsMatchTheRenderedTenants(t *testing.T) {
	rendered := renderedTenants(t, moduleRoot(t))
	// NON-VACUITY. acme is rendered today; a scan that finds none is comparing
	// two empty sets and passes having checked nothing.
	if len(rendered) == 0 {
		t.Fatal("found no rendered tenants under infra/deploy/tenants/ — the scan is broken, or " +
			"MT-02 per-tenant compute has been withdrawn. If it was withdrawn, this guard and the " +
			"gateway's per-tenant routing should go with it.")
	}

	configured, found := workloadEnv(t, "api-gateway", "API_GATEWAY_PER_TENANT_UPSTREAMS")
	if !found {
		t.Fatalf("api-gateway's manifest does not set API_GATEWAY_PER_TENANT_UPSTREAMS, and %d "+
			"tenant(s) have rendered compute: %v.\n\n"+
			"Every one of them is being read from the __system__ instance. A query for that "+
			"tenant's NAV, cash or ledger answers 200 with the PLATFORM book's numbers — another "+
			"tenant's fills, returned to an authenticated caller as their own. Set the variable to "+
			"the rendered tenants.", len(rendered), rendered)
	}

	var listed []string
	for _, v := range strings.Split(configured, ",") {
		if v = strings.TrimSpace(v); v != "" {
			listed = append(listed, v)
		}
	}
	sort.Strings(listed)
	sorted := append([]string(nil), rendered...)
	sort.Strings(sorted)

	missing := setDifference(sorted, listed)
	extra := setDifference(listed, sorted)

	if len(missing) > 0 {
		t.Errorf("%d tenant(s) have rendered per-tenant compute and are NOT in "+
			"API_GATEWAY_PER_TENANT_UPSTREAMS: %v\n\n"+
			"Their reads are dialled at the shared address, which is the __system__ instance, so "+
			"they answer 200 with the platform book's numbers rather than the tenant's own. This "+
			"is the failure that cannot be caught at runtime: the address resolves, the upstream "+
			"is healthy, the response is well-formed, and the only thing wrong with it is whose "+
			"money it describes.", len(missing), missing)
	}
	if len(extra) > 0 {
		t.Errorf("%d tenant(s) in API_GATEWAY_PER_TENANT_UPSTREAMS have no rendered compute under "+
			"infra/deploy/tenants/: %v\n\n"+
			"The gateway will dial <service>-<tenant> for them and every read 503s against a "+
			"Service that was never generated. Either render their compute "+
			"(go run ./cmd/kanz-tenantgen -tenant <t>) or remove them from the list.",
			len(extra), extra)
	}
}
