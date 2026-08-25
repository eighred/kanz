package arch

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	signalpb "github.com/eighred/kanz/kanz-schemas-go/signal/v1"
)

// THE TWO MarginMode VOCABULARIES MUST NAME THE SAME REGIMES (#417).
//
// signal.v1.MarginMode is what a STRATEGY ASKED FOR, recorded on StrategySignal
// — an immutable audit FACT. order.v1.MarginMode is what the VENUE IS
// INSTRUCTED to do. Two enums for what is today one set of values, deliberately:
// retyping the FACT's field to point at order.v1 is wire-identical but edits the
// schema of an audit root to save a mapping function, and forces two `buf
// breaking` exemptions onto it. The estate already works this way — SignalAction
// carries CLOSE, which is not an order side at all.
//
// # WHAT THE SPLIT COSTS, AND WHAT THIS PAYS
//
// Somewhere for them to drift apart. internal/signal/translate.orderMarginMode
// maps one onto the other with a `default:` arm that returns UNSPECIFIED —
// correct for an exhaustive mapping and a silent downgrade to SPOT the moment it
// is not. Add MARGIN_MODE_PORTFOLIO to signal.v1 alone and a strategy asking for
// portfolio margin produces an order that says spot: the OMS admits it (every
// connector declares CASH, so UNSPECIFIED is supported everywhere), the venue
// places it unlevered, and the audit root records the regime that was asked for.
// That is #240 exactly, rebuilt out of a mapping instead of a missing field.
//
// So the guard is not "the enums look alike". It is: the mapping cannot be
// silently incomplete, because a regime that exists on one side and not the
// other fails the build.
//
// # WHY IT READS DESCRIPTORS
//
// A guard grepping either .proto would match this explanation, the comment on
// orderMarginMode, and the retired-enum note — all three name the values. The
// compiled descriptor carries no comments, so there is nothing here for prose to
// satisfy.
func TestTheTwoMarginModeVocabulariesAgree(t *testing.T) {
	order := enumValuesByName(t, orderpb.MarginMode(0).Descriptor(), "order.v1.MarginMode")
	signal := enumValuesByName(t, signalpb.MarginMode(0).Descriptor(), "signal.v1.MarginMode")

	// NON-VACUITY. A renamed or unresolvable enum would leave both maps empty and
	// every comparison below trivially true.
	if len(order) < 3 || len(signal) < 3 {
		t.Fatalf("resolved %d order.v1 and %d signal.v1 margin modes — at least one enum was not "+
			"found, so this guard is comparing nothing", len(order), len(signal))
	}

	for name, num := range signal {
		got, ok := order[name]
		if !ok {
			t.Errorf("signal.v1.MarginMode declares %s and order.v1.MarginMode does not.\n\n"+
				"internal/signal/translate.orderMarginMode maps the first onto the second and "+
				"returns UNSPECIFIED for anything it does not recognise — so a strategy asking "+
				"for %[1]s would produce an order that says SPOT. Every connector declares CASH, "+
				"so the OMS would admit it, the venue would place it unlevered, and the audit "+
				"root would record %[1]s. That is #240 rebuilt out of a mapping. Add %[1]s to "+
				"order.v1.MarginMode and to orderMarginMode's switch.", name)
			continue
		}
		// THE NUMBERS TOO, not only the names. These are separate enums on
		// separate wires, so nothing forces them to agree — and a mapping written
		// by number rather than by constant would silently invert.
		if got != num {
			t.Errorf("%s is %d in signal.v1 and %d in order.v1. The mapping is written by "+
				"constant so this is not yet a live defect, but two enums that disagree on a "+
				"number are one refactor away from one", name, num, got)
		}
	}

	for name := range order {
		if _, ok := signal[name]; !ok {
			t.Errorf("order.v1.MarginMode declares %s and signal.v1.MarginMode does not.\n\n"+
				"Less dangerous than the other direction — an order can carry a regime no signal "+
				"can ask for — but it means a strategy cannot express something the venue layer "+
				"can work, and orderMarginMode has no input that produces it. Add it to "+
				"signal.v1.MarginMode, or delete it here.", name)
		}
	}
}

// enumValuesByName resolves an enum's values to name→number, keyed on the
// suffix after the shared prefix so the two packages' spellings compare.
func enumValuesByName(t *testing.T, d protoreflect.EnumDescriptor, want string) map[string]int32 {
	t.Helper()

	if got := string(d.FullName()); got != want {
		t.Fatalf("resolved %s, want %s — this guard has lost its subject", got, want)
	}
	out := make(map[string]int32, d.Values().Len())
	for i := 0; i < d.Values().Len(); i++ {
		v := d.Values().Get(i)
		out[string(v.Name())] = int32(v.Number())
	}
	return out
}
