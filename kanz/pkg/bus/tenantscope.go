package bus

import "fmt"

// A CONSUMER MUST NOT FOLD ANOTHER TENANT'S EVENT INTO ITS OWN BOOK (#223).
//
// Consumers read the envelope's tenant for routing decisions and then WRITE with
// their own configured one. The OMS is the clearest case: service.go resolves the
// venue account from env.GetTenantId(), and postgres.go inserts
// `VALUES (current_setting('app.tenant_id'), …)` — the GUC pinned to cfg.Tenant by
// internal/pg.NewTenantPool. Nothing compared the two, so tenant acme's order was
// admitted from an acme envelope and stored as __system__. accounting,
// alternatives and wealth fold the same way.
//
// The platform already had this guard twice and did not generalise it:
// tv-sync's projection (tenant != p.tenant ⇒ skip) and internal/topic.For
// ("cross-tenant event: envelope tenant_id %q on an archiver serving tenant %q").
//
// WHY THIS IS NOT A BLANKET REFUSAL, which is the obvious implementation and
// would take the estate down. The api-gateway stamps the CALLER's tenant on the
// command (orders.go: `TenantID: p.Tenant`, with a ProducerConfig.Tenant fallback
// deliberately rejected), while the shipped OMS runs OMS_TENANT="__system__". So
// on today's deployment every genuine order already arrives as a mismatch, and a
// service that refused them would reject all of them.
//
// The distinction that decides it is whether the service is TENANT-DEDICATED:
//
//   - serving == SystemTenant — the shared bucket. Every tenant's events arrive
//     here by design and are accepted. This IS the collapse #223 describes; it is
//     not fixed by a check, it is fixed by giving each tenant its own compute and
//     its own NATS account, which is #97. Accepting keeps the estate running
//     while that lands.
//   - serving is a real tenant — dedicated compute. A foreign envelope is an
//     anomaly: per-tenant NATS accounts do not cross, so it means a
//     misconfiguration or a leak, and it is refused loudly.
//
// The guard is therefore inert on the deployment that exists today and
// load-bearing on the one #97 creates — which is the point of landing it BEFORE
// tenant-beta rather than after. Provisioning a second tenant is the event that
// makes an unguarded fold a cross-tenant write, and it is also the moment nobody
// is re-reading these handlers.
//
// A LIVE SYMPTOM, so this does not read as purely theoretical: the archiver
// serving __system__ runs env.TenantId through topic.For, which refuses a
// mismatch. Every FACT derived from a real tenant's order is therefore already
// being rejected at the Kafka boundary — those events are outside DR today.
func RequireTenantScope(envTenant, serving string) error {
	if serving == "" {
		return fmt.Errorf("bus: consumer has no configured tenant — it cannot say whose book " +
			"an event belongs in, and a fold that cannot be scoped must not happen")
	}
	if serving == SystemTenant {
		// Documented above: the shared bucket accepts everything. Deliberately not
		// an error, and deliberately not silent either — the arch guard requires
		// every tenant-scoped consumer to route through here, so this branch is
		// the single place the collapse is described, and retiring it is one edit.
		return nil
	}
	if envTenant == "" {
		return fmt.Errorf("bus: untenanted event on a consumer serving tenant %q — an event that "+
			"names no tenant cannot be proven to belong to this one", serving)
	}
	if envTenant != serving {
		return fmt.Errorf("bus: cross-tenant event: envelope tenant_id %q on a consumer serving "+
			"tenant %q — folding it would write one tenant's data into another's book", envTenant, serving)
	}
	return nil
}
