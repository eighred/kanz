package wealth

import "github.com/kanz-eng/kanz/internal/platform/subject"

// The household valuation subject (WEALTH-01b).
//
// COMPACTED, one retained message per household — the opposite of alternatives'
// journal, and deliberately so. A HouseholdValued is the CURRENT picture, not an
// event that happened: the fold is last-write-wins and a pod booting tomorrow must
// arm itself with the latest valuation per household. That is the same shape as
// compliance.mandate and risk.position; see the WEALTH stream in
// infra/nats/bootstrap-job.yaml, which carries --max-msgs-per-subject=1.
const (
	// Domain is the envelope domain segment.
	Domain = "wealth"

	// EventTypeHouseholdValued is the envelope event_type. The subject a message
	// rides appends routing tokens (see SubjectHouseholdFor); this stays the flat
	// three-segment logical name.
	EventTypeHouseholdValued = "wealth.household.valued"

	// SubjectHouseholdAll is the wildcard a consumer subscribes to.
	SubjectHouseholdAll = EventTypeHouseholdValued + ".>"
)

// SubjectHouseholdFor is the subject one household's valuation is published on.
// The tenant and household tokens are what make compaction per-household rather
// than per-domain: with a single flat subject the stream would retain exactly one
// valuation for the entire platform. subjectToken sanitizes each id the same way
// SubjectMandateFor and subject.PositionFor do — an id carrying `.`, `*`, or `>`
// would otherwise silently change which subject the message lands on.
func SubjectHouseholdFor(tenantID, householdID string) string {
	return EventTypeHouseholdValued + "." + subjectToken(tenantID) + "." + subjectToken(householdID)
}

// subjectToken is subject.Token, matching the same local-alias pattern
// internal/compliance uses.
func subjectToken(s string) string { return subject.Token(s) }
