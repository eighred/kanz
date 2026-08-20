package bus

import "strings"

// TenantRoutePrefix is the leading segment that lets the broker route a command
// to the tenant that issued it (MT-02, #358).
const TenantRoutePrefix = "tenant."

// TenantRoutedSubject prefixes the WIRE subject with the issuing tenant, leaving
// the logical event_type alone.
//
// THE RULING THIS IMPLEMENTS (#97): tenants get DEDICATED COMPUTE. A shared OMS
// would need per-transaction tenant scoping, because internal/pg/pool.go binds a
// pool to one tenant for the process lifetime — a rewrite of the one component
// every service's RLS correctness rests on. Dedicated compute keeps a property
// that is currently structural: a pool that CANNOT serve the wrong tenant.
//
// WHY THE SUBJECT AND NOT THE ENVELOPE. Dedicated compute means the tenant's OMS
// lives in the tenant's NATS account, and accounts are isolated by construction —
// measured, not assumed: a publish in __system__ reaches NOTHING in `acme`, while
// the same subscriber receives a same-account publish. A CROSS-TENANT publisher
// (the gateway, webhook-ingest) holds ONE SVID, hence one account, and NATS
// routes on SUBJECT. It cannot dispatch on the envelope's tenant_id, however
// correct that field is. So the tenant has to be in the subject to be seen.
//
// WHAT IS NOT CHANGED, and this is the part worth protecting: event_type stays
// the 3-segment logical name. Every consumer, guard, audit projection and Kafka
// topic keys on it, and kanz-schemas/docs/subject-taxonomy.md defines it as the
// stable contract. The prefix lives ONLY on the wire inside __system__ —
// tenancy.yaml's per-account import remaps it back before any workload sees it,
// so a tenant's OMS subscribes the unchanged name and never learns it exists.
//
// Callers prefix UNCONDITIONALLY, including for __system__, which resolves its
// own prefix via an account mapping. A runtime branch would need a list of
// "tenants that have accounts" — a second place to update when provisioning, and
// the failure when it fell behind would be orders silently going nowhere.
//
// IT LIVES IN pkg/bus BECAUSE A SECOND CALLER APPEARED. It began beside the
// gateway's order handler; webhook-ingest publishes the same command from the
// signal path (#360). A copied helper is how a fix stops spreading, and the two
// wire formats disagreeing would route one publisher's orders into a tenant
// account and the other's into __system__ — with nothing failing.
func TenantRoutedSubject(tenant, subject string) string {
	if tenant == "" || strings.ContainsAny(tenant, ". *>") {
		// Unreachable on both live paths: the gateway refuses an authenticated
		// caller with no tenant, and translate resolves the tenant through a
		// FundAuthority that REFUSES the signal outright rather than answering with
		// an empty string — translate.NewFundAuthority rejects a fund declaring no
		// tenant at startup, and Emit returns ErrUnboundFund for a pair the table
		// does not grant (#632). The premise this comment used to carry — "a
		// non-empty FALLBACK" — was the defect: the fallback was the caller's own
		// fund_id, which made THIS FUNCTION's output attacker-chosen.
		// Returning the bare subject rather than minting
		// "tenant..order.order.submit" — or a tenant carrying a wildcard, which
		// would import into EVERY account — keeps a future caller that skips
		// those gates from producing a subject the broker silently drops or,
		// worse, fans out.
		return subject
	}
	return TenantRoutePrefix + tenant + "." + subject
}
