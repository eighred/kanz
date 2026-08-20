package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A uint64 MUST NOT RIDE A HAND-BUILT JSON BODY AS A NUMBER (#606).
//
// # The value is destroyed before any client code runs
//
// encoding/json writes a uint64 as a bare JSON number. JSON.parse — the only
// decoder a browser has — parses every number as a float64, so anything above
// 2^53 is silently rounded on arrival: 18446744073709551615 becomes
// 18446744073709552000, and nothing downstream can recover it. There is no error,
// no truncation warning and no way for the client to know it happened. The
// corruption is complete by the time the first line of application code sees the
// value.
//
// This is not hypothetical precision-lawyering. The field that opened #606 is
// compliance.v1.Mandate.version — the number pinning WHICH constraint set an
// order was audited against — served on three separate bodies of the mandate
// dual-control surface, where a signatory reads it before putting their name on
// a change to what governs a portfolio.
//
// # Why protobuf messages are not affected and hand-built maps are
//
// protojson emits every 64-bit integer as a JSON STRING, for exactly this reason.
// Anything marshalled through it is already correct and always was. The exposure
// is confined to the surfaces that build their reply as a Go map — where the
// field is whatever expression the author wrote, and a getter returning uint64
// lands in the body as a number with nothing objecting.
//
// Twenty-four files under services/ build a reply that way. That is the family
// this guard covers, and covering the family rather than the instance is the
// whole point: #606 found three sites in one file, and fixing three sites leaves
// the fourth author to copy the nearest line — which, before the repair, was a
// naked getter.
//
// # What it checks
//
// A value in a map[string]any (or map[string]interface{}) composite literal in a
// non-test file under services/ may not be a bare call to a getter for a proto
// field declared uint64 or fixed64. The getter set is DERIVED FROM THE PROTOS
// rather than listed here, so a new uint64 field is covered the day it is added
// and this guard cannot fall behind the schema.
//
// A wrapped value passes, because the wrapper is the fix: jsonUint64(...) in the
// compliance API, or strconv.FormatUint(...) directly. Both put a string on the
// wire.
//
// # What it CANNOT check, stated plainly
//
//   - It is SYNTACTIC. There is no type resolution here — type-checking the
//     module would need go/packages over every package on the import graph, which
//     this suite has declined three times over (see drain_single_writer_gate,
//     metric_writer, publisher_validation). So a uint64 reaching a body through a
//     local variable, a struct field, or a helper's return is invisible to it.
//   - It matches on the GETTER NAME, not the receiver's type. A message with a
//     STRING field spelled the same as some other message's uint64 field would be
//     flagged wrongly — which is what the exemption map is for, and why a false
//     positive here costs one reviewed line rather than a wrong body.
//
// What it does hold is the shape the defect actually took: somebody writing a
// reply map reaches for the getter, and the getter returns uint64.

// uint64JSONScope is the tree searched.
const uint64JSONScope = "services"

// uint64JSONExempt maps a module-relative file to the reason a bare uint64
// getter may sit in a JSON body there, and what retires the entry. DEFAULT-DENY:
// it is empty because the sweep for #606 found no legitimate case, and an empty
// exemption list is the strongest statement this guard can make. A new entry is
// somebody deciding on purpose that a client may receive a number it cannot
// parse.
var uint64JSONExempt = map[string]string{}

var (
	// uint64ProtoFieldRe matches a scalar 64-bit integer field declaration in a
	// .proto. `optional`/`repeated` are allowed to precede the type; a repeated
	// one is just as destroyable as a singular one.
	uint64ProtoFieldRe = regexp.MustCompile(`^\s*(?:optional\s+|repeated\s+)?(?:uint64|fixed64)\s+([a-z][a-z0-9_]*)\s*=\s*\d+`)
	// uint64JSONWrappers are the calls that make a 64-bit integer safe by turning
	// it into a string. A value that IS one of these is not inspected further —
	// the getter inside it is the argument to the fix, not an instance of the bug.
	uint64JSONWrappers = map[string]bool{"jsonUint64": true, "FormatUint": true, "Itoa": true, "Sprintf": true, "Sprint": true}
)

// uint64GetterNames derives the Go getter name for every uint64/fixed64 field in
// the schema tree: mandate_version -> GetMandateVersion.
//
// DERIVED, NOT LISTED, so the guard cannot fall behind the protos. A hand-copied
// list is correct on the day it is written and silently narrows every time a
// field is added — and the field it misses is exactly the new one nobody has
// thought about yet.
func uint64GetterNames(t *testing.T, protoRoot string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(protoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".proto") {
			return err
		}
		b, rErr := os.ReadFile(path)
		if rErr != nil {
			return rErr
		}
		rel, _ := filepath.Rel(protoRoot, path)
		for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
			// Comments cannot declare a field, and a guard that reads its own prose
			// is a guard that checks nothing (learned on body_identity).
			if s := strings.TrimSpace(line); strings.HasPrefix(s, "//") || strings.HasPrefix(s, "*") {
				continue
			}
			m := uint64ProtoFieldRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			var camel strings.Builder
			camel.WriteString("Get")
			for _, part := range strings.Split(m[1], "_") {
				if part == "" {
					continue
				}
				camel.WriteString(strings.ToUpper(part[:1]) + part[1:])
			}
			out[camel.String()] = m[1] + " (" + filepath.ToSlash(rel) + ")"
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", protoRoot, err)
	}
	return out
}

func TestNoUint64RidesAHandBuiltJSONBodyAsANumber(t *testing.T) {
	root := moduleRoot(t)
	protoRoot := filepath.Join(filepath.Dir(root), "kanz-schemas", "proto")

	getters := uint64GetterNames(t, protoRoot)

	// NON-VACUITY, ARM ONE: the schema scan found the fields. If the proto tree
	// moves or the field syntax changes, every scan below matches nothing and this
	// guard passes on a surface full of raw uint64s.
	if len(getters) < 5 {
		t.Fatalf("derived only %d uint64 getter name(s) from %s — expected at least 5.\n"+
			"The proto scan is finding nothing, so the sweep below would pass no matter what "+
			"any service puts in a JSON body", len(getters), protoRoot)
	}
	if _, ok := getters["GetVersion"]; !ok {
		t.Fatalf("GetVersion is not among the derived uint64 getters, but compliance.v1.Mandate."+
			"version IS a uint64 and is the field #606 was filed on. The derivation is wrong:\n  %v",
			getters)
	}

	type offence struct{ where, getter string }
	var offenders []offence
	literals := 0

	base := filepath.Join(root, uint64JSONScope)
	err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		fset := token.NewFileSet()
		f, pErr := parser.ParseFile(fset, path, nil, 0)
		if pErr != nil {
			t.Fatalf("parse %s: %v", path, pErr)
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)

		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isStringAnyMap(lit.Type) {
				return true
			}
			literals++
			for _, elt := range lit.Elts {
				kv, isKV := elt.(*ast.KeyValueExpr)
				if !isKV {
					continue
				}
				call, isCall := kv.Value.(*ast.CallExpr)
				if !isCall {
					continue
				}
				// A wrapped value is the FIX, not an instance of the defect.
				if uint64JSONIsWrapper(call.Fun) {
					continue
				}
				sel, isSel := call.Fun.(*ast.SelectorExpr)
				if !isSel {
					continue
				}
				if _, bad := getters[sel.Sel.Name]; !bad {
					continue
				}
				if _, exempt := uint64JSONExempt[rel]; exempt {
					continue
				}
				offenders = append(offenders, offence{
					where:  rel + ":" + ffmtLine(fset, kv.Pos()),
					getter: sel.Sel.Name,
				})
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// NON-VACUITY, ARM TWO: the AST sweep found the JSON bodies. Twenty-four files
	// under services/ build a reply as a map[string]any; a scan finding almost none
	// means the walk or the type match has broken, and the guard is asserting
	// nothing about a surface it believes it covered.
	if literals < 30 {
		t.Fatalf("found only %d map[string]any literal(s) under %s/ — expected at least 30.\n"+
			"The sweep is not finding the hand-built JSON bodies, so it cannot be finding raw "+
			"uint64s in them either", literals, uint64JSONScope)
	}

	var msgs []string
	for _, o := range offenders {
		msgs = append(msgs, o.where+": "+o.getter+"() — "+getters[o.getter])
	}
	sort.Strings(msgs)
	if len(msgs) > 0 {
		t.Errorf("these hand-built JSON bodies emit a 64-bit integer as a JSON NUMBER:\n  %s\n\n"+
			"JSON.parse reads every number as a float64, so above 2^53 the value is destroyed "+
			"before any client code runs — 18446744073709551615 arrives as 18446744073709552000 "+
			"and cannot be recovered. protojson emits every uint64 as a string for exactly this "+
			"reason; a map[string]any body gets none of that and has to do it deliberately.\n\n"+
			"Wrap it: jsonUint64(x) on the compliance API, or strconv.FormatUint(x, 10).\n\n"+
			"If a client genuinely requires a number here, add the file to uint64JSONExempt with "+
			"the reason and what retires it (#606).", strings.Join(msgs, "\n  "))
	}

	// DEAD ENTRIES: an exemption nothing needs any more is stale permission, and
	// the next raw uint64 that lands in that file inherits a justification written
	// for something else.
	var dead []string
	for rel := range uint64JSONExempt {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			dead = append(dead, rel+" (no such file)")
			continue
		}
		found := false
		for _, o := range offenders {
			if strings.HasPrefix(o.where, rel+":") {
				found = true
				break
			}
		}
		if !found {
			dead = append(dead, rel+" (no bare uint64 getter in a JSON body there any more)")
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("uint64JSONExempt has %d stale entr(y/ies):\n  %s\n\n"+
			"Delete them. An exemption that outlives its repair is how the next instance gets "+
			"waved through under an argument nobody re-read.", len(dead), strings.Join(dead, "\n  "))
	}
}

// isStringAnyMap reports whether the composite literal's type is a JSON body
// built by hand: map[string]any or its map[string]interface{} spelling.
func isStringAnyMap(e ast.Expr) bool {
	m, ok := e.(*ast.MapType)
	if !ok {
		return false
	}
	if k, isIdent := m.Key.(*ast.Ident); !isIdent || k.Name != "string" {
		return false
	}
	switch v := m.Value.(type) {
	case *ast.Ident:
		return v.Name == "any"
	case *ast.InterfaceType:
		return v.Methods == nil || len(v.Methods.List) == 0
	}
	return false
}

// uint64JSONIsWrapper reports whether a call turns its argument into a string.
func uint64JSONIsWrapper(fun ast.Expr) bool {
	switch f := fun.(type) {
	case *ast.Ident:
		return uint64JSONWrappers[f.Name]
	case *ast.SelectorExpr:
		return uint64JSONWrappers[f.Sel.Name]
	}
	return false
}

func ffmtLine(fset *token.FileSet, p token.Pos) string {
	return strconv.Itoa(fset.Position(p).Line)
}
