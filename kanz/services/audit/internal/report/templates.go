package report

import "github.com/kanz-eng/kanz/services/audit/internal/audit"

// BuiltIns are the shipped regulatory report templates. Deployments add their
// own as data (JSON → Template), but these cover the common asks: the authz
// trail, the command-execution record, the data-quality history, and the full
// log. A template is just a named filter + format, so adding one is config, not
// code (the AUDIT-01d "configurable templates" requirement).
func BuiltIns() map[string]Template {
	return map[string]Template{
		"authz-decisions": {
			Name: "authz-decisions", Title: "Authorization Decisions",
			Filter: audit.Filter{Kind: audit.KindAuthzDecision}, Format: FormatJSON,
		},
		"command-outcomes": {
			Name: "command-outcomes", Title: "Command Outcomes",
			Filter: audit.Filter{Kind: audit.KindCommandOutcome}, Format: FormatJSON,
		},
		"data-quality": {
			Name: "data-quality", Title: "Data-Quality Events",
			Filter: audit.Filter{Kind: audit.KindDataQuality}, Format: FormatJSON,
		},
		"full-log": {
			Name: "full-log", Title: "Full Audit Log",
			Filter: audit.Filter{}, Format: FormatJSON,
		},
	}
}
