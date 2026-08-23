package proxy

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/eighred/kanz/internal/tenantgen"
)

// A TENANT WITH ITS OWN BOOK MUST BE READ FROM ITS OWN BOOK (#668).
//
// # What the shared address answers, and why it is the wrong answer
//
// MT-02 renders some services PER TENANT: internal/tenantgen.Services is the
// list, and a rendered workload is named <service>-<tenant> with its own
// Service object, its own database scope and its own NATS account. accounting
// is one of them — it is pinned to a single tenant for the process lifetime by
// ACCOUNTING_TENANT and internal/pg.NewTenantPool (#97), which is exactly why
// it has to be rendered rather than bridged.
//
// The gateway dials ONE address per service. For a tenant with its own
// instance, that address is the __system__ instance — so a read of acme's NAV,
// cash or ledger would answer from the PLATFORM book. Not empty: WRONG. A
// number computed over somebody else's fills, returned to an authenticated acme
// caller as their own, with a 200.
//
// That is worse than the failure it replaces, and it is the reason this cannot
// be a silent fallback.
//
// # The rule
//
// A service rendered per tenant is dialled at its tenant's instance whenever
// the caller's tenant has one. Everything else — a service not rendered per
// tenant, or a tenant with no rendered compute — keeps the shared address
// unchanged, which is what __system__ and every un-onboarded tenant use.
//
// # The list of tenants is configuration, and its omission is the dangerous half
//
// This process cannot read infra/deploy/tenants/, so which tenants have their
// own instance arrives as config. Get it WRONG BY INCLUSION and reads 503
// against a Service that does not exist — loud, and fixed in a minute. Get it
// wrong by OMISSION and that tenant silently reads the platform book, which is
// the failure above. test/arch's TestGatewayTenantUpstreamsMatchTheRenderedTenants
// compares the configured list against what is actually rendered, so the
// omission cannot ship.

// tenantUpstreams resolves a service's base address for one tenant.
type tenantUpstreams struct {
	// perTenant is the set of services MT-02 renders per tenant, taken from
	// internal/tenantgen.Services rather than restated — that package is what
	// actually renders them, and a second list here would drift the day a
	// service is added to it.
	perTenant map[Service]bool
	// tenants have their own rendered instances.
	tenants map[string]bool
}

// newTenantUpstreams builds the resolver over the tenants named in config.
//
// A tenant id that tenantgen would refuse is refused here too, and loudly: it
// would render no manifest, so routing to it could only ever 503, and the
// operator's mistake is a typo in an env var rather than anything about the
// estate.
func newTenantUpstreams(tenants []string) (*tenantUpstreams, error) {
	u := &tenantUpstreams{
		perTenant: make(map[Service]bool),
		tenants:   make(map[string]bool, len(tenants)),
	}
	for _, svc := range tenantgen.Services {
		u.perTenant[Service(svc.Name)] = true
	}
	for _, t := range tenants {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if err := tenantgen.ValidateTenant(t); err != nil {
			return nil, fmt.Errorf("proxy: per-tenant upstream %q: %w", t, err)
		}
		u.tenants[t] = true
	}
	return u, nil
}

// resolve returns the base address to dial for svc on behalf of tenant, and
// whether it rewrote the shared one.
//
// THE REWRITE IS THE FIRST HOSTNAME LABEL, matching tenantgen.WorkloadName —
// `http://accounting.kanz-services.svc:8101` becomes
// `http://accounting-acme.kanz-services.svc:8101`. It is the label and not a
// substring replace: a service whose name also appears in the namespace or the
// port would otherwise be rewritten twice, and the resulting address resolves
// to nothing at all.
func (u *tenantUpstreams) resolve(base string, svc Service, tenant string) (string, bool) {
	if u == nil || tenant == "" || !u.perTenant[svc] || !u.tenants[tenant] {
		return base, false
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Hostname() == "" {
		// Unparseable bases are already cleaned out by NewMeshBackend; if one
		// reaches here it is the shared address and dialling it unchanged is the
		// established behaviour, not a new failure introduced by this path.
		return base, false
	}
	host := parsed.Hostname()
	label, rest, found := strings.Cut(host, ".")
	if !found {
		label, rest = host, ""
	}
	renamed := tenantgen.WorkloadName(label, tenant)
	if rest != "" {
		renamed += "." + rest
	}
	if port := parsed.Port(); port != "" {
		renamed += ":" + port
	}
	parsed.Host = renamed
	return parsed.String(), true
}
