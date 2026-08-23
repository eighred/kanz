package gateway

import (
	"fmt"

	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
)

// A TENANT WITH ITS OWN RISK ENGINE MUST BE QUERIED ON IT (#668).
//
// # Why the single client was wrong once a tenant had its own engine
//
// The gateway dialled ONE risk-engine and handed the client to this Handler.
// risk-engine holds its state per deployment — pg.NewTenantPool bound to
// cfg.Tenant (#97) — so once internal/tenantgen renders risk-engine-<tenant>,
// there are two engines with two books, and the single client points at the
// __system__ one.
//
// A tenant's exposure, VaR or scenario query would then be answered from the
// PLATFORM's positions. Not empty: another book's numbers, returned to an
// authenticated caller as their own risk, with a 200. That is the same failure
// the accounting read path has, and it is worse here because a risk number is
// what a limit is checked against.
//
// # Bounded at construction, never lazily
//
// The registry is built ONCE from a known tenant list and never grows. The
// alternative — creating a connection on first sight of a tenant — would make
// an unbounded map keyed by a string that arrives on a request, held for the
// process lifetime: a leak with an external trigger, in the one process every
// request passes through. Every connection here is created at startup and
// closed by the composition root's defer, so the set of connections is a
// function of configuration and nothing else.
//
// grpc.NewClient is lazy, so building them all costs no sockets: the TCP/TLS
// connection forms on the first RPC to that tenant, and a tenant that never
// queries never dials.

// riskClients resolves the risk-engine query client for a tenant.
type riskClients struct {
	// platform answers for __system__ and for every tenant with no rendered
	// engine of its own. Required — a gateway with no risk client at all is
	// refused at construction.
	platform querypb.RiskQueryServiceClient
	// perTenant holds the clients for tenants that have their own engine. Read
	// only after construction, so it needs no lock: the map is never written
	// again, and Go's memory model makes a map safe for concurrent READS.
	perTenant map[string]querypb.RiskQueryServiceClient
}

// newRiskClients builds a registry over the platform client alone.
//
// A NIL PLATFORM CLIENT IS ACCEPTED, and that is New's long-standing contract
// rather than an oversight: a gateway may be constructed without a risk upstream
// (several tests build one to exercise the approval and order routes), and the
// risk routes then panic if exercised — exactly as they did when the Handler
// held the client directly. Rejecting it here would change the behaviour of
// every such caller, which is a different change from this one.
func newRiskClients(platform querypb.RiskQueryServiceClient) *riskClients {
	return &riskClients{platform: platform}
}

// withPerTenant returns a registry that also routes the named tenants to their
// own engines. The map is COPIED, so a caller mutating its own afterwards cannot
// change the routing table at runtime — which is what makes the lock-free
// concurrent reads in clientFor safe.
func (r *riskClients) withPerTenant(perTenant map[string]querypb.RiskQueryServiceClient) (*riskClients, error) {
	m := make(map[string]querypb.RiskQueryServiceClient, len(perTenant))
	for tenant, c := range perTenant {
		if c == nil {
			// UNLIKE the platform client above, this one is refused. A nil here
			// panics on the first query FOR THAT TENANT ONLY — a crash reachable
			// by one tenant's traffic and by no test that does not use that
			// tenant — and nothing constructs this map except a composition root
			// that has just dialled every entry.
			return nil, fmt.Errorf("gateway: nil risk client for tenant %q", tenant)
		}
		m[tenant] = c
	}
	return &riskClients{platform: r.platform, perTenant: m}, nil
}

// clientFor returns the client that owns this tenant's positions.
//
// THE FALLBACK IS THE PLATFORM ENGINE, and that is correct rather than a
// compromise: a tenant with no rendered engine has no book of its own, and
// __system__'s engine is the one that holds its positions. The dangerous case
// is the reverse — a tenant that DOES have an engine being answered from the
// platform — and that cannot happen here, because a tenant is in this map
// exactly when the composition root was told it has one.
// test/arch's TestGatewayTenantUpstreamsMatchTheRenderedTenants is what keeps
// that list in step with what is actually rendered.
func (r *riskClients) clientFor(tenant string) querypb.RiskQueryServiceClient {
	if r == nil {
		return nil
	}
	if c, ok := r.perTenant[tenant]; ok {
		return c
	}
	return r.platform
}
