package natsrebuild

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/topic"
	"github.com/eighred/kanz/tools/replay"
)

// SystemTenant carries the cross-cutting platform events; its archived topics
// keep the UN-PREFIXED legacy names (kanz-schemas/README.md § Subject Taxonomy
// §6).
//
// Re-exported from internal/topic, NOT redefined. This was briefly a second copy
// of the archiver's rule, because the package lived under
// services/archiver/internal/ where Go's scoping put it out of reach — and it was
// guarded by a test that read the archiver's source and failed on divergence.
// That guard worked, but it protected a duplicate rather than removing one. This
// tool becoming the second consumer is exactly the trigger CLAUDE.md names for
// promoting shared code to kanz/internal/, so the package moved and both sides
// now call the same function.
const SystemTenant = topic.SystemTenant

// tenantName bounds a tenant id to what is safe as a Kafka topic prefix. A dot
// is the segment separator of the topic grammar, so a tenant containing one
// could resolve to another tenant's — or the __system__ — topic and breach the
// PREFIXED-ACL isolation boundary (MT-01c). Refuse rather than sanitize.
var tenantName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Qualify returns the Kafka topic that archives the un-prefixed topic `base` for
// `tenant`. It delegates to internal/topic — the same function the archiver WRITES
// through — so a rebuild cannot read from a name the archiver never wrote to.
func Qualify(tenant, base string) string { return topic.Qualify(tenant, base) }

// ParseTenants turns NATS_REBUILD_TENANTS into the tenant set to rebuild.
//
// `set` must be the second result of os.LookupEnv, not a len() test: UNSET and
// SET-BUT-EMPTY are different operator intents and must not resolve to the same
// run. Unset means "the historical single-tenant run" and defaults to
// SystemTenant, which keeps the DR runbook's behaviour unchanged. Set-but-empty
// means someone tried to configure tenants and the value did not survive
// templating — that run would rebuild nothing for anyone and exit 0, which is
// the exact silent success this tool exists to stop.
func ParseTenants(raw string, set bool) ([]string, error) {
	if !set {
		return []string{SystemTenant}, nil
	}
	var tenants []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			tenants = append(tenants, p)
		}
	}
	if len(tenants) == 0 {
		return nil, fmt.Errorf(
			"NATS_REBUILD_TENANTS is set to %q but names no tenant: a run configured for tenants that "+
				"resolves to none would rebuild nothing and still exit 0. Unset it to rebuild the "+
				"%s tenant only, or name the tenants explicitly", raw, SystemTenant)
	}
	for _, t := range tenants {
		if t == SystemTenant {
			continue
		}
		if !tenantName.MatchString(t) {
			return nil, fmt.Errorf(
				"NATS_REBUILD_TENANTS names invalid tenant %q: want %s or a %s id — a dot would make the "+
					"prefix resolve to another tenant's topic space (MT-01c)",
				t, SystemTenant, tenantName.String())
		}
	}
	for i, t := range tenants {
		if slices.Index(tenants, t) != i {
			return nil, fmt.Errorf(
				"NATS_REBUILD_TENANTS names %q more than once: every topic of that tenant would be "+
					"drained twice in one run", t)
		}
	}
	return tenants, nil
}

// Target is one (tenant, topic) unit of work: the Kafka topic actually read,
// the un-prefixed archived name it was derived from, and whether it is
// compacted STATE (read in full) rather than an EVENT window.
type Target struct {
	Tenant string
	Base   string
	Topic  string
	State  bool
}

// Window is the read range for this target, deferring to WindowFor so EVENT vs
// STATE stays one rule.
func (t Target) Window(since time.Duration, now time.Time) replay.Range {
	return WindowFor(t.Topic, map[string]bool{t.Topic: t.State}, since, now)
}

// ResolveTargets expands the archived topic set across every requested tenant.
//
// The base lists stay UN-PREFIXED — they are the archiver's produced set, held
// to infra/kafka/topics-job.yaml by TestArchivedTopicConsumersMatchTheArchiver-
// ProducedSet — and the tenant prefix is DERIVED here. A hand-written prefixed
// list in the manifest would be a second copy of that table, which is precisely
// how the pre-36a9ba1 dead table survived: it was never derived from anything.
//
// A requested tenant that resolves to zero topics is a hard error. That is the
// defect this whole capability exists to close: before it, asking for a tenant
// the run could not serve drained nothing and exited 0.
func ResolveTargets(tenants, topics, stateTopics []string) ([]Target, error) {
	if len(tenants) == 0 {
		return nil, fmt.Errorf("natsrebuild: no tenants requested — nothing would be rebuilt")
	}
	if len(topics) == 0 {
		return nil, fmt.Errorf("natsrebuild: no topics to rebuild — nothing would be restored for any tenant")
	}
	state := make(map[string]bool, len(stateTopics))
	for _, s := range stateTopics {
		state[s] = true
	}
	var out []Target
	for _, tenant := range tenants {
		for _, base := range topics {
			out = append(out, Target{
				Tenant: tenant,
				Base:   base,
				Topic:  Qualify(tenant, base),
				State:  state[base],
			})
		}
	}
	return out, RequireTenantCoverage(tenants, out)
}

// RequireTenantCoverage refuses a work set that does not cover every requested
// tenant. It is the check ResolveTargets' own inputs cannot make redundant: the
// Runner takes its targets from a caller, and a tenant that was asked for but
// carries no target drains nothing while every per-topic line still reads
// "rebuilt". Zero resolved topics for a requested tenant is a hard error, never
// a quiet zero.
func RequireTenantCoverage(tenants []string, targets []Target) error {
	covered := make(map[string]int, len(tenants))
	for _, t := range targets {
		covered[t.Tenant]++
	}
	var empty []string
	for _, tenant := range tenants {
		if covered[tenant] == 0 {
			empty = append(empty, tenant)
		}
	}
	if len(empty) > 0 {
		return fmt.Errorf(
			"natsrebuild: tenant(s) %s were requested but resolved to 0 topics — a rebuild that "+
				"restores nothing for a requested tenant must not report success",
			strings.Join(empty, ", "))
	}
	return nil
}
