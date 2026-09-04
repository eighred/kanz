package wealth

import "github.com/eighred/kanz/internal/platform/subject"

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

// The model-portfolio catalogue subject (WEALTH-01d).
//
// COMPACTED, one retained message per model — the same shape and the same reason
// as the household valuation above and as compliance.mandate. A ModelPortfolio is
// the target allocation IN FORCE for a risk profile, not an event that happened:
// a wealth pod booting tomorrow must arm its catalogue from the stream in one
// read, because until it does, every household's drift is UNEVALUABLE and the
// only detector of a drifted book is a human remembering to look (#1010).
//
// It rides the SAME `wealth.>` stream already provisioned with
// --max-msgs-per-subject=1 and no max-age (infra/nats/bootstrap-job.yaml), so no
// new stream is created — a model MUST NOT AGE OUT for the same reason a mandate
// must not: on the 168h streams a model published nine days ago is deleted, and
// the next restart comes back with an empty catalogue that is indistinguishable
// from a firm that has set no targets.
const (
	// EventTypeModelPublished is the envelope event_type. As with
	// EventTypeHouseholdValued this stays the flat logical name; the subject a
	// message rides appends routing tokens (see SubjectModelFor).
	EventTypeModelPublished = "wealth.model.published"

	// SubjectModelAll is the wildcard the catalogue arms from.
	SubjectModelAll = EventTypeModelPublished + ".>"
)

// SubjectModelFor is the subject one model portfolio is published on. The tenant
// and model tokens are what make compaction per-model rather than per-domain:
// with a single flat subject the stream would retain exactly one model for the
// entire platform, and a firm running five risk profiles would arm with one of
// them and silence about the other four.
func SubjectModelFor(tenantID, modelID string) string {
	return EventTypeModelPublished + "." + subjectToken(tenantID) + "." + subjectToken(modelID)
}
