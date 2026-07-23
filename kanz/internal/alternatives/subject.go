package alternatives

// The commitment-lifecycle subjects (ALT-01b). One subject per event type, flat
// three-segment names carrying identity in the envelope's PartitionKey — the same
// shape as order.order.filled, and for the same reason: these are events that
// happened, not the current state of anything.
//
// THEY MUST NOT BE COMPACTED. The fund position is the FOLD of this journal, and
// IRR/TVPI/DPI are computed from the dated cashflow series it carries. A stream
// that kept only the last message per subject would destroy the history those
// numbers are derived from, and the damage would surface as wrong returns rather
// than as an error — see the ALTERNATIVES stream in infra/nats/bootstrap-job.yaml,
// which is deliberately append-only.
const (
	// Domain is the envelope domain segment for every subject below.
	Domain = "alternatives"

	// SubjectCommitted records a new capital commitment to a fund.
	SubjectCommitted = "alternatives.commitment.committed"
	// SubjectCalled records a capital call drawn against a commitment.
	SubjectCalled = "alternatives.commitment.called"
	// SubjectDistributed records a distribution returned to the LP.
	SubjectDistributed = "alternatives.commitment.distributed"
	// SubjectMarked records a NAV mark of the residual holding.
	SubjectMarked = "alternatives.commitment.marked"
)

// AllSubjects is the set an alternatives consumer subscribes to. It exists so the
// service's default subject list and the operator publisher cannot drift apart.
func AllSubjects() []string {
	return []string{SubjectCommitted, SubjectCalled, SubjectDistributed, SubjectMarked}
}
