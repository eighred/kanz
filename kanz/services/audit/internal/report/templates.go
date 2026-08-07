package report

import "github.com/eighred/kanz/services/audit/internal/audit"

// BuiltIns are the shipped regulatory report templates. Deployments add their
// own as data (JSON → Template), but these cover the common asks: the authz
// trail, the command-execution record, the data-quality history, and the full
// log. A template is just a named filter + format, so adding one is config, not
// code (the AUDIT-01d "configurable templates" requirement).
//
// EVERY TEMPLATE CARRIES A LIMIT, INCLUDING THE KIND-SCOPED ONES (#304).
//
// `full-log` was the filed defect — a zero-value filter over an append-only
// table, so one GET read a tenant's entire history into the pod and marshalled a
// second indented copy. But a Kind is not a bound either: `authz-decisions` is
// every authorization decision the tenant has ever made, which on a busy estate
// is the LARGEST of these four, not a small one. Capping only the template with
// "full" in its name would fix the example and leave the defect.
//
// The cap is a page, not a truncation: Generate reports Complete/NextCursor and
// the route resumes with ?after=. test/arch/report_templates_bounded_test.go
// makes this default-deny, so a template added later cannot omit it.
func BuiltIns() map[string]Template {
	return map[string]Template{
		"authz-decisions": {
			Name: "authz-decisions", Title: "Authorization Decisions",
			Filter: audit.Filter{Kind: audit.KindAuthzDecision, Limit: DefaultPageSize}, Format: FormatJSON,
		},
		"command-outcomes": {
			Name: "command-outcomes", Title: "Command Outcomes",
			Filter: audit.Filter{Kind: audit.KindCommandOutcome, Limit: DefaultPageSize}, Format: FormatJSON,
		},
		"data-quality": {
			Name: "data-quality", Title: "Data-Quality Events",
			Filter: audit.Filter{Kind: audit.KindDataQuality, Limit: DefaultPageSize}, Format: FormatJSON,
		},
		"full-log": {
			Name: "full-log", Title: "Full Audit Log",
			Filter: audit.Filter{Limit: DefaultPageSize}, Format: FormatJSON,
		},
	}
}
