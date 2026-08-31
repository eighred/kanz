package arch

// NO LEG OF AN EXECUTION-COST DECOMPOSITION MAY BE FABRICATED (#866).
//
// # The failure this exists to refuse
//
// #866 asks for implementation shortfall split into spread, impact and timing,
// and says plainly what would make that split worthless: "a three-way split that
// is arithmetic dressing on a single number is worse than an honest two-part
// answer." The way that happens is not malice, it is a default. Somebody adds a
// venue whose quotes are not on the spine, the spread leg comes back nil, a nil
// is awkward to render, and it becomes `new(big.Rat)`. Nothing fails. The report
// then says every order on that venue crossed for free — and because the legs
// must sum to the shortfall, the whole of the missing cost lands in IMPACT,
// which is the one leg nobody can independently check.
//
// That reads as an algorithm problem. It is a data problem, and no test of the
// arithmetic would catch it, because the arithmetic stays correct.
//
// # The two arms
//
//  1. In internal/execution/tca, no field of Attribution whose name ends in Bps
//     may be assigned a freshly-constructed ZERO *big.Rat. Unknown must stay nil
//     all the way out.
//
//  2. In the OMS, every *_bps field of order.v1.ExecutionAttributionRecorded must
//     be assigned, and every assignment must come from a value produced by
//     tca.ToDecimal in the same function. A basis-point figure on the wire is
//     then, structurally, the measurement — not a Decimal somebody built beside
//     it. This also catches the OTHER defect family this repository keeps
//     hitting: a field present upstream and dropped at a boundary (#240, #405,
//     #486, and TransactionCostRecorded's own arrival_at/executed_at).
//
// # Derivation, not lists
//
// Arm 1's field set comes from Attribution's own AST. Arm 2's comes from the
// generated protobuf DESCRIPTOR — the schema itself — so a leg added by a later
// benchmark (#864 wants interval VWAP next) is covered on the commit that adds
// it, by nobody remembering. A hand-written list is the artifact that has broken
// repeatedly here.
//
// Comments are not parsed into the AST (mode 0) and nothing below matches source
// text, so this file's own prose can neither satisfy nor defeat it.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// attributionFabricationExemptions is the DEFAULT-DENY allow-list, keyed
// "<file>:<Field>". It is EMPTY, and an empty allow-list is not the same thing as
// no allow-list: a leg that genuinely must be built some other way lands here,
// named, with the issue that retires it. A DEAD ENTRY FAILS, so an exemption
// cannot outlive its repair.
var attributionFabricationExemptions = map[string]string{}

// The non-vacuity floors. Tripping one means the analysis stopped recognising
// the shape, not that the estate got smaller — a guard that finds nothing to
// check passes forever.
const (
	// Attribution carries ShortfallBps, SpreadBps, ImpactBps and TimingBps.
	attributionRatLegFloor = 4
	// ExecutionAttributionRecorded carries the same four on the wire.
	attributionWireLegFloor = 4
)

const (
	tcaPkgDir = "internal/execution/tca"
	omsPkgDir = "services/oms/internal/order"
)

// ===== ARM 1: unknown stays nil inside the measurement =====

func TestNoDecompositionLegIsAFabricatedZero(t *testing.T) {
	root := moduleRoot(t)
	files := goFilesIn(t, filepath.Join(root, tcaPkgDir))
	if len(files) == 0 {
		t.Fatalf("no non-test Go files under %s — this guard would pass vacuously", tcaPkgDir)
	}

	legs := ratBpsFieldsOf(t, files, "Attribution")
	if len(legs) < attributionRatLegFloor {
		t.Fatalf("found %d *big.Rat fields ending in Bps on tca.Attribution (at least %d expected): %v — "+
			"either the decomposition was renamed or this analysis stopped recognising it, and a "+
			"guard that finds nothing to check passes forever", len(legs), attributionRatLegFloor, legs)
	}

	var violations []string
	used := map[string]bool{}
	for _, f := range files {
		fset := token.NewFileSet()
		file := parseNoComments(t, fset, f)
		rel := relTo(root, f)
		ast.Inspect(file, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CompositeLit:
				for _, elt := range v.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					name, ok := identName(kv.Key)
					if !ok || !legs[name] {
						continue
					}
					if isZeroRat(kv.Value) {
						key := rel + ":" + name
						if _, ex := attributionFabricationExemptions[key]; ex {
							used[key] = true
							continue
						}
						violations = append(violations, fmt.Sprintf("%s: %s is set to a zero *big.Rat",
							posOf(fset, kv.Pos()), name))
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range v.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || !legs[sel.Sel.Name] {
						continue
					}
					if i >= len(v.Rhs) {
						continue // a multi-value call; not a fabricable literal
					}
					if isZeroRat(v.Rhs[i]) {
						key := rel + ":" + sel.Sel.Name
						if _, ex := attributionFabricationExemptions[key]; ex {
							used[key] = true
							continue
						}
						violations = append(violations, fmt.Sprintf("%s: %s is assigned a zero *big.Rat",
							posOf(fset, v.Pos()), sel.Sel.Name))
					}
				}
			}
			return true
		})
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Fatalf("a decomposition leg is set to a fabricated zero:\n\n  %s\n\n"+
			"UNKNOWN IS NOT ZERO, and here it is the difference between a report and a lie. A zero "+
			"spread claims the fund crossed for free; because the legs must sum to the shortfall, "+
			"the whole of the unexplained cost then lands in impact — the one leg nobody can check "+
			"independently. Leave the leg nil and let the quality field say TOTAL_ONLY.",
			strings.Join(violations, "\n  "))
	}
	assertNoDeadExemptions(t, attributionFabricationExemptions, used)
}

// ===== ARM 2: every wire leg is assigned, and only from the measurement =====

func TestEveryWireBpsFieldIsSetFromTheMeasurement(t *testing.T) {
	root := moduleRoot(t)

	// THE SCHEMA IS THE SOURCE OF TRUTH. Reading the descriptor rather than a
	// list here is what makes a leg added by a later benchmark covered without
	// anyone coming back to this file.
	wire := map[string]bool{} // Go field name -> required
	fields := (&orderpb.ExecutionAttributionRecorded{}).ProtoReflect().Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if strings.HasSuffix(string(fd.Name()), "_bps") {
			wire[goFieldName(fd)] = true
		}
	}
	if len(wire) < attributionWireLegFloor {
		t.Fatalf("found %d *_bps fields on order.v1.ExecutionAttributionRecorded (at least %d "+
			"expected) — the message was renamed or the descriptor is stale, and this guard would "+
			"pass vacuously", len(wire), attributionWireLegFloor)
	}

	files := goFilesIn(t, filepath.Join(root, omsPkgDir))
	if len(files) == 0 {
		t.Fatalf("no non-test Go files under %s", omsPkgDir)
	}

	assigned := map[string]bool{}
	var violations []string
	used := map[string]bool{}

	for _, f := range files {
		fset := token.NewFileSet()
		file := parseNoComments(t, fset, f)
		rel := relTo(root, f)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			// Every identifier in this function that holds a value tca.ToDecimal
			// produced. One hop, one function — enough to make "the wire value IS
			// the measurement" a structural property rather than a convention.
			measured := measuredIdents(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.CompositeLit:
					for _, elt := range v.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						name, ok := identName(kv.Key)
						if !ok || !wire[name] {
							continue
						}
						assigned[name] = true
						if !fromMeasurement(kv.Value, measured) {
							violations = append(violations, recordLegViolation(rel, name, posOf(fset, kv.Pos()), &used))
						}
					}
				case *ast.AssignStmt:
					for i, lhs := range v.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if !ok || !wire[sel.Sel.Name] {
							continue
						}
						assigned[sel.Sel.Name] = true
						if i >= len(v.Rhs) || !fromMeasurement(v.Rhs[i], measured) {
							violations = append(violations, recordLegViolation(rel, sel.Sel.Name, posOf(fset, v.Pos()), &used))
						}
					}
				}
				return true
			})
		}
	}

	var missing []string
	for name := range wire {
		if !assigned[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("these basis-point fields exist on order.v1.ExecutionAttributionRecorded and "+
			"NOTHING IN %s EVER SETS THEM:\n\n  %s\n\n"+
			"That is the defect family this repository has now hit five times — a value present "+
			"upstream and dropped at a boundary (#240, #405, #486, and this message's own sibling "+
			"TransactionCostRecorded, which shipped without arrival_at and executed_at). A consumer "+
			"cannot tell an unset leg from an absent producer.",
			omsPkgDir, strings.Join(missing, "\n  "))
	}

	// Drop violations that an exemption covered.
	var real []string
	for _, v := range violations {
		if v != "" {
			real = append(real, v)
		}
	}
	sort.Strings(real)
	if len(real) > 0 {
		t.Fatalf("a basis-point figure reaches the wire from something other than the "+
			"measurement:\n\n  %s\n\n"+
			"Every figure on this FACT must be the exact value tca produced, rendered through "+
			"tca.ToDecimal — which REFUSES rather than rounds. A cost report is read as "+
			"authoritative, and a rounded basis-point figure is how a venue or algorithm comparison "+
			"gets decided by the fourth decimal.", strings.Join(real, "\n  "))
	}
	assertNoDeadExemptions(t, attributionFabricationExemptions, used)
}

// recordLegViolation returns a violation string, or "" when an exemption covers it.
func recordLegViolation(rel, field, pos string, used *map[string]bool) string {
	key := rel + ":" + field
	if _, ex := attributionFabricationExemptions[key]; ex {
		(*used)[key] = true
		return ""
	}
	return fmt.Sprintf("%s: %s", pos, field)
}

func assertNoDeadExemptions(t *testing.T, exemptions map[string]string, used map[string]bool) {
	t.Helper()
	var dead []string
	for key := range exemptions {
		if !used[key] {
			dead = append(dead, key)
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Fatalf("these exemptions name sites that no longer offend:\n\n  %s\n\n"+
			"An exemption that outlives its repair is a hole nobody is watching. Delete them.",
			strings.Join(dead, "\n  "))
	}
}

// measuredIdents collects the identifiers in one function that were bound to a
// tca.ToDecimal result. Multi-value assignment only, because that is the shape
// ToDecimal has — (*commonpb.Decimal, bool) — and a caller ignoring the bool has
// already lost the refusal that makes the value exact.
func measuredIdents(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "ToDecimal" {
			return true
		}
		if pkg, ok := identName(sel.X); !ok || pkg != "tca" {
			return true
		}
		for _, lhs := range as.Lhs {
			if name, ok := identName(lhs); ok && name != "_" {
				out[name] = true
			}
		}
		return true
	})
	return out
}

// fromMeasurement reports whether an expression carries a tca-produced value: a
// bound identifier, or a direct tca.ToDecimal call.
func fromMeasurement(e ast.Expr, measured map[string]bool) bool {
	if name, ok := identName(e); ok {
		return measured[name]
	}
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "ToDecimal" {
		return false
	}
	pkg, ok := identName(sel.X)
	return ok && pkg == "tca"
}

// ratBpsFieldsOf returns the *big.Rat fields of one struct whose names end in
// "Bps", read from the struct's own declaration.
func ratBpsFieldsOf(t *testing.T, files []string, structName string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, f := range files {
		fset := token.NewFileSet()
		file := parseNoComments(t, fset, f)
		ast.Inspect(file, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || ts.Name.Name != structName {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, fld := range st.Fields.List {
				star, ok := fld.Type.(*ast.StarExpr)
				if !ok {
					continue
				}
				sel, ok := star.X.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Rat" {
					continue
				}
				for _, nm := range fld.Names {
					if strings.HasSuffix(nm.Name, "Bps") {
						out[nm.Name] = true
					}
				}
			}
			return true
		})
	}
	return out
}

// isZeroRat reports whether an expression constructs a *big.Rat that is zero:
// new(big.Rat), &big.Rat{}, or big.NewRat(0, n).
func isZeroRat(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.UnaryExpr:
		if v.Op != token.AND {
			return false
		}
		cl, ok := v.X.(*ast.CompositeLit)
		return ok && isBigRatType(cl.Type) && len(cl.Elts) == 0
	case *ast.CallExpr:
		fn, ok := identName(v.Fun)
		if ok && fn == "new" && len(v.Args) == 1 {
			return isBigRatType(v.Args[0])
		}
		sel, ok := v.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewRat" || len(v.Args) != 2 {
			return false
		}
		if pkg, ok := identName(sel.X); !ok || pkg != "big" {
			return false
		}
		lit, ok := v.Args[0].(*ast.BasicLit)
		return ok && lit.Value == "0"
	}
	return false
}

func isBigRatType(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Rat" {
		return false
	}
	pkg, ok := identName(sel.X)
	return ok && pkg == "big"
}

func identName(e ast.Expr) (string, bool) {
	id, ok := e.(*ast.Ident)
	if !ok {
		return "", false
	}
	return id.Name, true
}

// goFieldName converts a protobuf field name to the Go field protoc-gen-go
// generates for it: snake_case to UpperCamelCase.
func goFieldName(fd protoreflect.FieldDescriptor) string {
	var b strings.Builder
	up := true
	for _, r := range string(fd.Name()) {
		if r == '_' {
			up = true
			continue
		}
		if up {
			b.WriteString(strings.ToUpper(string(r)))
			up = false
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// goFilesIn lists the non-test Go files directly under dir. It does not walk, so
// it needs no skip set — but the sibling helper does, and every guard in this
// directory that DOES walk must call skipWalkDir (walk_skip_test.go).
func goFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || skipWalkDir(fs.DirEntry(e)) {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	sort.Strings(out)
	return out
}

func parseNoComments(t *testing.T, fset *token.FileSet, path string) *ast.File {
	t.Helper()
	// Mode 0: comments are NOT attached. Three guards in this directory have
	// passed with the checked thing deleted, by matching their own prose.
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return f
}

func posOf(fset *token.FileSet, p token.Pos) string {
	pos := fset.Position(p)
	return fmt.Sprintf("%s:%d", filepath.Base(pos.Filename), pos.Line)
}

func relTo(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}
