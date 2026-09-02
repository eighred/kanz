package compliance

// UNGOVERNED MEANT TWO THINGS AND AN OPERATOR ACTS ON THEM DIFFERENTLY (#926).
//
// MandateRegistry.Mandate answered `(nil, false, nil)` from TWO places:
//
//   - the key has no versions at all — nobody has run `kanz-mandate` for this
//     portfolio yet. Expected during onboarding, and the reason
//     OMS_REQUIRE_MANDATE defaults false.
//   - the key HAS versions and none of them is in effect at the query time. The
//     portfolio WAS governed. Not expected, and money.
//
// The gate could not tell them apart, so `kanz_compliance_ungoverned_orders_total`
// and one WARN said the same sentence for both — and the second state reads
// exactly like a portfolio nobody has got round to mandating.
//
// # This is why #916 went unnoticed
//
// Scheduling a mandate change evicted the version in force from the compacted
// stream; the next replica booted holding only the FUTURE-DATED one, resolved the
// portfolio as ungoverned, and admitted its orders with no constraints. The
// registry was Armed and Complete — the message applied cleanly — so every health
// signal was green and the only trace was a counter that looks like onboarding.
// #916 removed that particular cause; it did not give the signal the ability to
// tell the two states apart, so the next cause would have been equally invisible.
//
// # Why an enum and not a second bool
//
// The estate has done this once already and wrote down why: MandateStatus is
// three states "and not the bool it replaced (#675)", with the old field number
// RESERVED so a message written by an old producer cannot decode as a new verdict.
// A second bool beside `ok` would reintroduce the same trap one level out — two
// booleans encode four states, two of which are nonsense, and nothing would stop
// a caller reading only the first.
//
// Changing the type rather than adding a field is also what makes the migration
// safe: every call site fails to compile until it has answered the new question,
// which is the "fail loudly, never silently" rule applied to a refactor.

// Governance is why a mandate lookup did or did not produce a mandate.
type Governance int

const (
	// GovernanceUnspecified is the zero value and is never a legitimate answer.
	// It exists so a caller that forgot to set one is detectable rather than
	// defaulting to the reassuring reading — the same reason MandateUnchecked is
	// the zero MandateStatus.
	GovernanceUnspecified Governance = iota

	// Governed: a mandate is in effect for this portfolio at the query time.
	Governed

	// NeverMandated: this key has no mandate versions at all.
	//
	// THE BENIGN ONE, and the only one OMS_REQUIRE_MANDATE=false was meant to
	// trade through. It is the onboarding state: somebody has not run
	// `kanz-mandate` for this portfolio yet.
	NeverMandated

	// MandateLapsed: versions EXIST for this key and none is in effect at the
	// query time.
	//
	// THE DANGEROUS ONE. Somebody decided what governs this portfolio and the
	// decision is not in force right now, which is a different fact from nobody
	// having decided. Every version being future-dated is the #916 shape; a
	// version list that ends before the query time is the other.
	//
	// It is NOT refused unconditionally here, and that restraint is deliberate:
	// whether an ungoverned order is admitted is OMS_REQUIRE_MANDATE's decision
	// and moving it would change trading behaviour under an observability issue.
	// What changes is that the state is now SAYABLE — the log names it and the
	// metric carries it — so an operator can see a portfolio that stopped being
	// governed instead of reading it as one that never was.
	MandateLapsed
)

// String renders the verdict for a log field and a metric label.
func (g Governance) String() string {
	switch g {
	case Governed:
		return "governed"
	case NeverMandated:
		return "never_mandated"
	case MandateLapsed:
		return "mandate_lapsed"
	default:
		return "unspecified"
	}
}

// Governances returns every legitimate verdict, in declaration order.
//
// DERIVED, NOT RETYPED, for the reason #806 and #803 both record: when the defect
// class is "somebody enumerated a set by hand and missed a member", a list written
// again in the consumer is a further copy of the thing that broke. The OMS seeds
// one counter series per verdict from this, so a verdict added above gets its
// series — and its zero — without anybody remembering a second edit.
func Governances() []Governance {
	return []Governance{Governed, NeverMandated, MandateLapsed}
}

// NoMandate reports whether this verdict means no mandate constrains the order.
//
// NAMED NoMandate AND NOT Ungoverned, deliberately. Decision.Ungoverned is a
// REFUSAL FLAG on the pre-trade decision, and
// test/arch/every_decision_consumer_handles_every_refusal_test.go finds a
// decision consumer by looking for a selector expression named after one of those
// flags. A method called Ungoverned() would make every caller of this predicate
// look like a consumer of the refusal family and be required to name all five
// flags — three files became false positives that way before the rename. The
// collision is in the name, not in the guard.
//
// BOTH NON-GOVERNED VERDICTS ARE UNGOVERNED, and collapsing them HERE is correct
// where collapsing them in the signal was not: the admission decision genuinely
// is the same for both — OMS_REQUIRE_MANDATE decides it — while the operator's
// response differs entirely. One predicate for the trading behaviour, three
// verdicts for the diagnosis.
func (g Governance) NoMandate() bool {
	return g == NeverMandated || g == MandateLapsed || g == GovernanceUnspecified
}
