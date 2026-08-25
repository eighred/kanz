package arch

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	optimizationpb "github.com/eighred/kanz/kanz-schemas-go/optimization/v1"
)

// THE MANDATE VERDICT MAY NOT BE TWO-STATE ON THE WIRE (#675, #646).
//
// #646 removed a `bool mandate_feasible` from the Go proposal type because a
// bool cannot tell "no mandate was ever consulted" from "consulted, and it
// passed" — and it defaulted to the second, so Rebalance stamped every proposal
// it built as feasible, CheckMandate had never once run on this platform, and
// the bridge that turns a proposal into live order commands used that stamp as
// its only gate.
//
// The WIRE kept the boolean. optimization.v1.RebalanceProposal still declared
// `bool mandate_feasible = 9` for another sixteen weeks, which is the same trap
// waiting for the first consumer to adopt the message — plausibly a non-Go one,
// where the Go tri-state offers no protection at all. #675 replaced it with a
// MandateStatus enum whose zero value is not a pass.
//
// # WHY THIS READS THE DESCRIPTOR AND NOT THE .proto TEXT
//
// A guard that grepped optimization.proto for "mandate_feasible" would match
// the `reserved "mandate_feasible"` line that is the CURE, and the paragraph
// above, and the field's own historical comment — passing whether or not the
// field exists. Sibling guards in this package were found doing exactly that.
//
// The generated descriptor is the compiled truth: it knows the field's type,
// which numbers are reserved, and what an enum's zero value is named. Comments
// do not survive into it, so there is nothing here for prose to satisfy.
func TestMandateVerdictIsNotTwoStateOnTheWire(t *testing.T) {
	msg := (&optimizationpb.RebalanceProposal{}).ProtoReflect().Descriptor()

	// ── The verdict field exists, and is an ENUM.
	field := msg.Fields().ByName("mandate_status")
	if field == nil {
		t.Fatal("RebalanceProposal has no mandate_status field. If the verdict was renamed, " +
			"retarget this guard; if it was removed, the proposal now carries no compliance " +
			"verdict at all and the bridge gate has nothing to read (#675).")
	}
	if field.Kind() != protoreflect.EnumKind {
		t.Fatalf("mandate_status is %s, not an enum — a two-state verdict cannot express "+
			"\"nothing checked this\", which is the whole defect (#646)", field.Kind())
	}

	// ── Its ZERO VALUE must not be a pass. proto3 cannot distinguish an unset
	// enum from one explicitly set to 0, so whatever 0 means is what an
	// unevaluated proposal claims about itself.
	values := field.Enum().Values()
	if values.Len() < 3 {
		t.Fatalf("%s has %d values; a verdict needs at least three — unchecked, feasible, "+
			"infeasible. Two collapses back into the bool this replaced",
			field.Enum().FullName(), values.Len())
	}
	zero := values.ByNumber(0)
	if zero == nil {
		t.Fatalf("%s declares no zero value, which proto3 requires", field.Enum().FullName())
	}
	switch string(zero.Name()) {
	case "MANDATE_STATUS_UNSPECIFIED":
		// The state every proposal starts in. Not a soft pass.
	default:
		t.Errorf("%s's zero value is %q. The zero value is what a proposal NOBODY EVALUATED "+
			"reports, so it must name the unevaluated state — never a verdict. A proposal must "+
			"not acquire a pass merely by being constructed (#646).",
			field.Enum().FullName(), zero.Name())
	}

	// ── The old bool cannot come back, by either name or number.
	if old := msg.Fields().ByName("mandate_feasible"); old != nil {
		t.Errorf("RebalanceProposal declares mandate_feasible again (number %d) — this is the "+
			"exact two-state field #675 removed", old.Number())
	}
	assertReserved(t, msg, 9, "mandate_feasible")
}

// assertReserved confirms a retired field's number AND name are both reserved.
//
// BOTH, because they fail differently. A reused NUMBER lets a message written by
// an old producer decode as a new verdict rather than fail — and `true` and
// FEASIBLE are both varint 1, so that mistake would produce a plausible answer
// rather than a visible error. A reused NAME silently changes what JSON and
// protojson callers mean by it.
func assertReserved(t *testing.T, msg protoreflect.MessageDescriptor, number protoreflect.FieldNumber, name protoreflect.Name) {
	t.Helper()

	numberReserved := false
	ranges := msg.ReservedRanges()
	for i := 0; i < ranges.Len(); i++ {
		if r := ranges.Get(i); number >= r[0] && number < r[1] {
			numberReserved = true
			break
		}
	}
	if !numberReserved {
		t.Errorf("field number %d is not reserved on %s, so it can be handed to a new field. "+
			"A message from an old producer would then decode as that field rather than fail",
			number, msg.FullName())
	}

	nameReserved := false
	names := msg.ReservedNames()
	for i := 0; i < names.Len(); i++ {
		if names.Get(i) == name {
			nameReserved = true
			break
		}
	}
	if !nameReserved {
		t.Errorf("field name %q is not reserved on %s, so it can be redeclared on a different "+
			"number — the same trap wearing a new tag", name, msg.FullName())
	}
}
