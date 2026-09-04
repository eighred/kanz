package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// A FIELD THE PLATFORM STAMPS IS CLEARED OFF THE COMMAND BEFORE ANYTHING READS IT
// (#943, #897).
//
// # The hole this closes
//
// order.v1.ExecutionSchedule now carries two fields a client must not set:
// volume_profile_version, which names the measured curve a VWAP or POV parent was
// sized against, and volume_profile, which IS that curve. Both ride on
// SubmitOrder, because SubmitOrder is where the schedule lives, so both arrive
// from the wire and both must be discarded.
//
// THE SECOND ONE IS NOT THE FIRST ONE AGAIN. A caller who could name a version
// could only choose among curves somebody measured — an older, thinner one, which
// is bad. A caller who can send the SHAPE supplies a market nobody measured at
// all: a curve concentrating the whole session into one bucket sizes a child at
// the full parent quantity, and the desk's participation cap is enforced against
// a tape that does not exist. And the check that would otherwise catch it cannot:
// the OMS verifies the stored curve against the stored VERSION — the version is a
// hash of the curve's own content — so a pair a client fabricated together is
// self-consistent and verifies exactly like a published one. The strip is the
// whole control.
//
// One line in services/oms/internal/order.validateSchedule protects both. That
// line is silent when it is wrong: an order admitted with a client's curve is
// admitted, works, fills, and reports a version that hashes correctly. Nothing
// downstream can tell it from a real one, so nothing downstream will ever raise
// it.
//
// # WHY THE FIELD LIST IS DERIVED AND NOT WRITTEN HERE
//
// Because a list written here would be a further copy of the fallible list, which
// is how the estate acquired #806 and #803. orderDigestScheduleExempt already
// enumerates the ExecutionSchedule fields excluded from the dual-control digest,
// and its own header states the rule that makes it the right source: "EVERY FIELD
// A CALLER CAN SET MUST BE HASHED" — so an entry there is, by that guard's
// contract, a field no caller may set. It is also dead-entry-checked by
// TestTheOrderDigestCoversEverySubmitOrderField, so an entry cannot outlive its
// field.
//
// That coupling is the point rather than a shortcut: exempting a new field from
// the digest and forgetting to strip it is the exact pair of edits that would
// hand a caller a stamped field, and after this guard the first half fails
// without the second.
//
// # What this checks, and what it cannot
//
// Inside validateSchedule's body, each such field is ASSIGNED A ZERO VALUE
// through a selector — sch.VolumeProfile = nil, sch.VolumeProfileVersion = "".
//
// It cannot check that the assignment precedes every read, or that the value
// stamped afterwards is the platform's. Those are behavioural and they are proved
// behaviourally, by
// services/oms/internal/order.TestValidateSchedule_DiscardsAClientSuppliedProfileVersion,
// which sends a genuinely self-consistent forged pair and asserts neither half
// reaches the store. This bounds the SHAPE that keeps that test possible to write.
//
// Comments are detached (parser.ParseFile with mode 0) because the paragraphs
// above name both fields, and three guards in this tree have already passed while
// matching nothing but their own prose.

// stampedScheduleFn is the one admission path that may stamp them, and therefore
// the one that must strip them.
const (
	stampedSchedulePkg = "services/oms/internal/order"
	stampedScheduleFn  = "validateSchedule"
)

func TestAPlatformStampedScheduleFieldIsStrippedFromTheCommand(t *testing.T) {
	// NON-VACUITY, THE SOURCE HALF. An empty exemption map would make every arm
	// below trivially satisfied while the fields it describes were still arriving
	// from the wire unstripped.
	if len(orderDigestScheduleExempt) == 0 {
		t.Fatalf("orderDigestScheduleExempt is empty, so this guard is asserting that nothing " +
			"needs stripping. Either every ExecutionSchedule field is now a caller field — in " +
			"which case say so and delete this guard — or the map moved and the check is dead")
	}

	// THE PROTO DESCRIPTOR SUPPLIES THE GO NAME, not a snake-to-camel routine
	// written here. protoc-gen-go's mapping is the only authority on what the field
	// is actually called in the code being scanned, and a second implementation of
	// it would silently stop matching the day a field name needs the rule this one
	// got wrong — so the guard would pass while the strip was gone.
	fields := (&orderpb.ExecutionSchedule{}).ProtoReflect().Descriptor().Fields()
	want := make([]string, 0, len(orderDigestScheduleExempt))
	for field := range orderDigestScheduleExempt {
		fd := fields.ByName(protoreflect.Name(field))
		if fd == nil {
			t.Fatalf("orderDigestScheduleExempt names %q, which order.v1.ExecutionSchedule does "+
				"not declare — the exemption outlived its field and this guard cannot tell "+
				"whether anything needs stripping", field)
		}
		want = append(want, goFieldName(fd))
	}
	sort.Strings(want)

	root := moduleRoot(t)
	dir := filepath.Join(root, filepath.FromSlash(stampedSchedulePkg))
	files := goFilesUnder(t, dir)
	fset := token.NewFileSet()

	cleared := map[string]bool{}
	sawFn := false

	for _, f := range files {
		if strings.HasSuffix(f.rel, "_test.go") {
			// A TEST MAY SET THEM. Setting a stamped field is how
			// TestValidateSchedule_DiscardsAClientSuppliedProfileVersion builds the
			// forged command it then proves is discarded.
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, filepath.FromSlash(f.rel)), f.body, 0)
		if err != nil {
			t.Fatalf("parse %s/%s: %v", stampedSchedulePkg, f.rel, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != stampedScheduleFn || fn.Body == nil {
				continue
			}
			sawFn = true
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Lhs) != len(as.Rhs) {
					return true
				}
				for i, lhs := range as.Lhs {
					// A ZERO VALUE, NOT ANY ASSIGNMENT. `sch.VolumeProfile = somethingElse`
					// is a substitution rather than a strip, and counting it would let a
					// future edit satisfy this guard while still carrying a caller's value
					// forward under another name. isZeroValue is the estate's one spelling
					// of "absent" and is shared with ephemeral_store_fallback_test.go.
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || !isZeroValue(as.Rhs[i]) {
						continue
					}
					cleared[sel.Sel.Name] = true
				}
				return true
			})
		}
	}

	// NON-VACUITY, THE SUBJECT HALF. A renamed or relocated admission check makes
	// every field below "missing" for the wrong reason, or — worse, if the loop
	// above found nothing to scan — makes the guard pass over an empty package.
	if !sawFn {
		t.Fatalf("no %s found in %s — the admission check that strips the platform's own fields "+
			"has been renamed or moved, so this guard is protecting nothing",
			stampedScheduleFn, stampedSchedulePkg)
	}

	var missing []string
	for _, name := range want {
		if !cleared[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%s.%s does not clear %v off the inbound command.\n\n"+
			"Each of those is a field orderDigestScheduleExempt says the PLATFORM stamps and a "+
			"caller cannot — which is also why the dual-control digest does not hash it. If it "+
			"is not cleared here, a client sets it and nothing downstream can tell: "+
			"volume_profile and volume_profile_version are checked against EACH OTHER (the "+
			"version is a hash of the curve's own content), so a pair a caller fabricated "+
			"together verifies exactly like a published one, and the order's audit record then "+
			"asserts a market the platform never measured.\n\n"+
			"Clear it beside the others, or — if it really is a caller field — remove its "+
			"exemption and let the digest hash it.",
			stampedSchedulePkg, stampedScheduleFn, missing)
	}
}
