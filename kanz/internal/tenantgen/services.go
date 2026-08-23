package tenantgen

import (
	"fmt"
	"path"
	"sort"
)

// WHAT MT-02 RENDERS PER TENANT, IN ONE PLACE (#637).
//
// This table used to be implicit: tenantgen knew the string "oms", the CLI
// defaulted to oms-deploy.yaml, the arch guard looked for oms-<tenant>.yaml, and
// provision-tenant.sh printed instructions naming the OMS. Four places agreeing
// by coincidence, and a fifth — infra/tenancy/tenantctl.sh's Kafka grant — that
// did not agree at all.
//
// WHY A TENANT NEEDS MORE THAN AN OMS. A NATS account's subjects are physically
// invisible outside it. A tenant that is given an OMS and nothing else therefore
// publishes every FACT it produces into an account no platform service is a
// member of: the order was accepted, the fill happened, the position moved, and
// no consumer anywhere can hear any of it. The tenant's OMS looks healthy and so
// does every service that should have recorded the event, because "this tenant
// produced no events" and "this tenant's events cannot reach me" are the same
// observable state.
//
// The remedy is not to export the FACTs back to __system__: the platform
// consumers are pinned to __system__ by a *_TENANT env var, so they would either
// refuse the envelope (internal/topic.For NACKs a tenant mismatch, forever) or
// fold another tenant's event into the platform's own book (#223). The remedy is
// that a consumer which must record a tenant's FACTs runs INSIDE that tenant's
// account, with its own tenant-pinned pool — which is #97's ruling, applied to
// the read side of the bus instead of only the write side.
//
// ADDING A SERVICE HERE IS A PLATFORM DECISION, NOT A CONFIG EDIT. Every entry
// costs a rendered manifest per tenant, a NATS user in every tenant account, and
// — where KafkaProducer is set — a prefixed Kafka ACL per tenant. Read
// test/arch/tenant_bridge_parity_test.go before adding one: it names the
// consumer roles a tenant still has no path to, and adding an entry here is how
// one of those exemptions retires.
type Service struct {
	// Name is the base manifest's object name, and the prefix of the rendered
	// per-tenant name: <Name>-<tenant>.
	Name string
	// Base is the shared manifest this service renders from, relative to the
	// module root.
	Base string
	// Container is the container whose TenantEnv is rewritten. Named, never
	// positional, so the base may reorder or grow its container list.
	Container string
	// TenantEnv is the env var that pins this service to one tenant. REQUIRED:
	// a per-tenant deployment that still reads __system__ is the defect this
	// whole file exists to close, wearing the tenant's name.
	TenantEnv string
	// KafkaProducer marks a service that produces to the tenant's PREFIXED Kafka
	// topics, and therefore needs the tenant's prefixed ACL granted to its
	// compute SVID. infra/tenancy/tenantctl.sh's COMPUTE_KAFKA_SAS carries the
	// same list on the provisioning side; test/arch/tenant_compute_test.go fails
	// the build if the two disagree.
	KafkaProducer bool
	// DropEnv names env vars the base sets that MUST NOT be carried into a
	// per-tenant render, each with the reason.
	//
	// IT IS FOR DEPENDENCIES THAT EXIST ONLY FOR __system__. Copying such a
	// variable through is worse than omitting it: the rendered pod points at a
	// SINGLE-TENANT peer that will refuse it, and the refusal arrives wearing
	// the vocabulary of a different fault. A dropped variable leaves the service
	// in its own documented "not configured" posture, which every one of them
	// states at startup.
	//
	// A NAME THAT IS NOT IN THE BASE IS AN ERROR, not a no-op — see Render. An
	// entry that stopped matching would silently start carrying the variable it
	// exists to remove.
	DropEnv []DropEnvVar
	// Why states what the tenant loses without this service. It is printed by
	// cmd/kanz-tenantgen and quoted by the guards, so an operator reading a
	// failure learns the consequence rather than the rule.
	Why string
}

// DropEnvVar is one env var removed from a per-tenant render, and why.
type DropEnvVar struct {
	Name string
	// Why is printed by cmd/kanz-tenantgen beside the rendered file, so the
	// operator learns which capability the tenant does NOT have.
	Why string
}

// Services is the per-tenant compute set, ordered by Name so every generated
// artifact and every guard message is deterministic.
var Services = []Service{
	{
		Name:          "archiver",
		Base:          "infra/deploy/archiver-deploy.yaml",
		Container:     "archiver",
		TenantEnv:     "ARCHIVER_TENANT",
		KafkaProducer: true,
		Why: "the tenant's DURABLE LOG OF RECORD. archiver drains this account's NATS spine " +
			"into the tenant's prefixed Kafka topics, which is what replay, the lakehouse and " +
			"every regulatory reconstruction read. Without it the tenant's FACTs live only in " +
			"NATS, age off at the stream's max-age, and are outside DR entirely — while every " +
			"health check reports green. CLAUDE.md's one sequencing rule that outlives any issue " +
			"is that real orders must never be placed against a store that may not be backed up.",
	},
	{
		Name:      "accounting",
		Base:      "infra/deploy/accounting-deploy.yaml",
		Container: "accounting",
		TenantEnv: "ACCOUNTING_TENANT",
		Why: "the tenant's BOOK OF RECORD. accounting folds the tenant's fills into ledger " +
			"entries, and it is pinned to one tenant for the process lifetime by ACCOUNTING_TENANT " +
			"and internal/pg.NewTenantPool (#97), so it cannot be reached by an export/import " +
			"bridge the way audit can — it subscribes the LOGICAL subject names, and a " +
			"tenant-prefixed subject arrives as nothing. Without a rendered instance inside the " +
			"tenant's NATS account, the tenant's orders fill and NO LEDGER ENTRY IS POSTED: NAV, " +
			"cash and the buying-power gate are all computed over an empty book, while every " +
			"service reports Ready (#668).",
	},
	{
		Name:      "oms",
		Base:      "infra/deploy/oms-deploy.yaml",
		Container: "oms",
		TenantEnv: "OMS_TENANT",
		DropEnv: []DropEnvVar{{
			Name: "OMS_DATAMASTER_URL",
			Why: "THE SECURITY MASTER IS __system__-ONLY (#640). datamaster serves ONE tenant " +
				"per instance and answers every other caller with a no-oracle 404, and this " +
				"estate renders no per-tenant datamaster — so carrying the base's URL through " +
				"would point this pod at an instance that refuses it. A 404 is refdata's word " +
				"for \"the master does not hold this instrument\", so every lookup would come " +
				"back as a REFERENCE-DATA GAP naming the tenant's whole book, sending an " +
				"operator to load data that no instance was ever going to serve them. Dropped, " +
				"the OMS runs its stated \"no reference-data source\" posture instead: one WARN " +
				"at startup, kanz_instrument_classifier_wired 0, and a refusal that says " +
				"classifier: unavailable. Retire this entry by rendering a per-tenant " +
				"datamaster, at which point the URL becomes datamaster-<tenant>.",
		}},
		Why: "the tenant's ORDER PATH. Without it the tenant's account receives the order " +
			"commands the bridge carries and nothing consumes them.",
	},
}

// ServiceByName returns the declared service with the given name.
func ServiceByName(name string) (Service, bool) {
	for _, s := range Services {
		if s.Name == name {
			return s, true
		}
	}
	return Service{}, false
}

// KafkaProducerNames returns the declared services that produce to a tenant's
// prefixed Kafka topics, sorted. tenantctl.sh's COMPUTE_KAFKA_SAS must match it.
func KafkaProducerNames() []string {
	var out []string
	for _, s := range Services {
		if s.KafkaProducer {
			out = append(out, s.Name)
		}
	}
	sort.Strings(out)
	return out
}

// WorkloadName is the name a per-tenant render carries: "<service>-<tenant>".
//
// ONE RULE, IN ONE PLACE. It was spelled out four times — the object name and
// the ServiceAccount suffix in render.go, the manifest path and the SPIFFE ID
// here — and a fifth caller was about to arrive outside this package: the
// api-gateway has to derive `accounting-acme.kanz-services.svc` from the shared
// address to reach a tenant's own instance (#668). A naming rule the ROUTER and
// the RENDERER each hold their own copy of is one that breaks the day a tenant
// id contains something one of them normalises and the other does not — and it
// breaks as a 503 against a Service that was rendered under a different name.
//
// The tenant is not validated here: callers that accept one from outside go
// through validateTenant first, and the render path has already done so by the
// time it names anything.
func WorkloadName(service, tenant string) string {
	return service + "-" + tenant
}

// ManifestPath is where a tenant's rendered manifest for svc lives, relative to
// the module root. One rule, so the generator, the CLI, the provisioning script
// and the drift guard cannot disagree about where a manifest is.
func (s Service) ManifestPath(tenant string) string {
	return path.Join("infra", "deploy", "tenants", tenant, WorkloadName(s.Name, tenant)+".yaml")
}

// ComputeSPIFFEID is the identity a rendered pod actually presents: the SPIRE
// controller templates it from the pod's namespace and ServiceAccount
// (infra/security/spire/registration.yaml), and MT-02 compute is a name-suffixed
// ServiceAccount in the shared kanz-services namespace — NOT the tenant-<t>
// namespace tenantctl.sh mints for a tenant's own workloads.
//
// A tenant's NATS account must admit this exact string or the pod authenticates
// and maps to NO account, able to neither publish nor subscribe (SEC-M3).
func (s Service) ComputeSPIFFEID(tenant string) string {
	return "spiffe://kanz.internal/ns/kanz-services/sa/" + WorkloadName(s.Name, tenant)
}

// ValidateTenant is the one place a tenant id is checked.
//
// EXPORTED because the api-gateway checks the same ids (#668): it resolves a
// tenant's own upstream by rewriting a hostname to tenantgen.WorkloadName, so an
// id this package would refuse to render is one the gateway must refuse to
// route to — it would name a Service that was never generated, and the only
// symptom would be a 503 nobody can trace back to a typo in an env var. A second
// regex at the edge is the copy this repository keeps paying for.
func ValidateTenant(tenant string) error {
	if !tenantIDRE.MatchString(tenant) {
		return fmt.Errorf("tenantgen: invalid tenant id %q: must be lowercase alphanumeric with internal hyphens only", tenant)
	}
	return nil
}

// validateTenant is the in-package spelling, kept so the render path reads as it
// did.
func validateTenant(tenant string) error { return ValidateTenant(tenant) }
