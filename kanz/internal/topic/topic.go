// Package topic maps an envelope onto the Kafka topic that archives it.
//
// The name is derived from the envelope's EVENT_TYPE, not from the NATS subject
// it arrived on: event_type carries the full three-segment logical name
// ({domain}.{entity}.{event_type}) and the bus client already enforces that the
// envelope's `domain` equals its first segment. The subject is a transport
// detail; the logical name is the contract (kanz-schemas/docs/subject-taxonomy.md).
//
// Every failure here is a REFUSAL, never a guess. The caller turns an error into
// a NACK, which leaves the event safe in NATS until someone fixes the cause. A
// fallback topic would misfile the event permanently — and cross-filing one
// tenant's events into another's topic (or into the shared __system__ topic)
// breaches the Kafka PREFIXED-ACL isolation boundary (MT-01c).
package topic

import (
	"fmt"
	"strings"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
)

// SystemTenant carries cross-cutting platform events and pre-tenancy events. Its
// topics keep the UN-PREFIXED legacy names (subject-taxonomy.md §6).
const SystemTenant = "__system__"

// reserved leading segments that are not part of the {domain}.{entity} space.
var reserved = map[string]bool{"replay": true, "dlq": true}

// For returns the Kafka topic that archives env, for an archiver configured to
// serve the given tenant.
//
//	order.order.submitted   tenant acme       -> acme.order.order
//	platform.mode.changed   tenant __system__ -> platform.mode
//	risk.position.changed   STATE_SNAPSHOT    -> acme.risk.position.snapshot
func For(env *envelopepb.Envelope, tenant string) (string, error) {
	if tenant == "" {
		return "", fmt.Errorf("archiver tenant is empty: refusing to route %q", env.GetEventType())
	}
	if got := env.GetTenantId(); got != tenant {
		return "", fmt.Errorf("cross-tenant event: envelope tenant_id %q on an archiver serving tenant %q (event %q)",
			got, tenant, env.GetEventId())
	}

	parts := strings.Split(env.GetEventType(), ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", fmt.Errorf("malformed event_type %q: want exactly three segments {domain}.{entity}.{event_type}",
			env.GetEventType())
	}
	if reserved[parts[0]] {
		return "", fmt.Errorf("reserved leading segment %q in event_type %q: replay/dlq are not archivable",
			parts[0], env.GetEventType())
	}

	name := parts[0] + "." + parts[1]
	if env.GetEventClass() == envelopepb.EventClass_EVENT_CLASS_STATE_SNAPSHOT {
		// A snapshot is STATE: it needs compaction, while its sibling FACTs need
		// time-retention. One topic cannot be both.
		name += ".snapshot"
	}
	return Qualify(tenant, name), nil
}

// Qualify applies the tenant prefix to an already-derived topic name.
//
// Split out of For because the DR rebuild needs the PREFIX rule without an
// envelope to derive a name from: it works backwards, from the un-prefixed topic
// list in the rebuild Job to the per-tenant topics that actually hold the data
// (tools/natsrebuild). It had to reimplement this while the package lived under
// services/archiver/internal/, where Go's internal scoping put it out of reach —
// and a second copy of the rule that decides WHICH TENANT'S DATA a DR run reads
// is the kind of copy that gets discovered during a failover.
//
//	Qualify("__system__", "order.order") -> "order.order"
//	Qualify("acme", "order.order")       -> "acme.order.order"
//
// __system__ stays un-prefixed so the legacy topics and their running consumers
// are untouched (MT-01a); every other tenant is confined to its own `{tenant}.`
// prefix, which is the handle the MT-01c ACL binds to.
func Qualify(tenant, name string) string {
	if tenant == SystemTenant {
		return name
	}
	return tenant + "." + name
}
