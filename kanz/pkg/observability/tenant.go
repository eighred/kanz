package observability

// Tenant labelling convention (MT-01e). The platform uses ONE label name for
// the tenant dimension across every `kanz_*` metric series, so dashboards and
// alerts can `by (tenant)` uniformly and a per-tenant noisy-neighbor view
// composes across exporters. Exporters that know their tenant add this label;
// the gateway (the per-tenant admission edge) is the first adopter.
const TenantLabel = "tenant"

// SystemTenant is the reserved tenant for cross-cutting platform signals and
// pre-tenancy events (mirrors bus.SystemTenant / MT-01a). Kept here too so the
// observability layer normalizes without importing the bus.
const SystemTenant = "__system__"

// NormalizeTenant maps an empty tenant to SystemTenant, bounding label
// cardinality and keeping the data-plane convention (empty ⇒ __system__,
// MT-01a) consistent on the metrics side. Callers at an authenticated edge that
// want to distinguish "no identity" from "platform" should label that case
// explicitly rather than passing it through here.
func NormalizeTenant(tenant string) string {
	if tenant == "" {
		return SystemTenant
	}
	return tenant
}
