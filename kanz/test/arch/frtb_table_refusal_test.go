package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// AN FRTB COMPONENT FED A SUPERVISORY TABLE MUST BE ABLE TO REFUSE IT (#617).
//
// # What went wrong without it
//
// Every FRTB capital component looks up a risk weight by key. A Go map returns
// the zero value on a miss, so an absent weight is not an error — it is a
// weight of zero, and a position weighted to zero contributes nothing to the
// charge. The filing is assembled, FRTB_TOTAL is smaller than the book, and it
// is signed. Nothing denies, nothing dead-letters, nothing logs.
//
// It errs toward comfort in the one direction a capital number must not, and it
// has happened twice:
//
//   - #565 — Delta, Vega and Curvature. A table missing Commodity produced a
//     filing "indistinguishable from a book with no commodity risk".
//   - #617 — DRC, the fourth component, left behind by that repair. DRCParams{}
//     weighted EVERY jump-to-default position to nothing. handleFRTB decodes
//     DRCParams straight from the request body, so `"drc_params": {}` reached it
//     from outside the process.
//
// Both were found by reading, years apart in intent and one release apart in
// fact. The property is mechanical, so it is checked mechanically: a function
// handed a caller-supplied weight table must have a way to say the table does
// not cover this book.
//
// # What this does NOT claim
//
// It checks that refusal is POSSIBLE, not that it happens. A component could
// return an error and never use it. That is the weaker property, and it is worth
// having anyway: the signature is what a new component copies, and an errorless
// one cannot be fixed at the call site by anybody who notices later.
func TestEveryFRTBComponentTakingATableCanRefuseIt(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, "internal", "regulatory", "frtb")

	fset := token.NewFileSet()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	var files []*ast.File
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		files = append(files, f)
	}
	if len(files) < 3 {
		t.Fatalf("parsed only %d non-test file(s) under %s — the walk is broken and this guard proves "+
			"nothing", len(files), dir)
	}

	// A TABLE TYPE is one a missing key can silently zero: a map, or a struct
	// holding one. Named rather than listed, because the sixth component will
	// bring its own.
	tableTypes := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, s := range gd.Specs {
				ts, ok := s.(*ast.TypeSpec)
				if !ok || ts.Name == nil {
					continue
				}
				if holdsAMap(ts.Type) {
					tableTypes[ts.Name.Name] = true
				}
			}
		}
	}

	var offenders []string
	seen := map[string]bool{}
	takers := 0
	for _, f := range files {
		rel, _ := filepath.Rel(root, fset.Position(f.Pos()).Filename)
		rel = filepath.ToSlash(rel)
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name == nil || !fn.Name.IsExported() {
				continue
			}
			if fn.Type.Params == nil || !takesATable(fn.Type.Params, tableTypes) {
				continue
			}
			takers++
			if returnsAnError(fn.Type.Results) {
				continue
			}
			key := rel + ":" + fn.Name.Name
			seen[key] = true
			if _, exempt := frtbTablelessRefusalExempt[key]; exempt {
				continue
			}
			offenders = append(offenders, key+" takes a caller-supplied supervisory table and cannot "+
				"refuse it. A missing risk weight is not an error in Go — it is a weight of ZERO, so the "+
				"position contributes nothing, FRTB_TOTAL comes out smaller than the book, and the filing "+
				"is signed with no warning (#565, #617). Return an error wrapping frtb.ErrUncovered.")
		}
	}

	// NON-VACUITY, BOTH HALVES. A guard that found no table types, or no function
	// taking one, would pass while every component was errorless.
	if len(tableTypes) < 2 {
		t.Fatalf("found %d table type(s) in package frtb — expected at least Params/ClassParams/DRCParams; "+
			"the type walk is broken and this guard proves nothing", len(tableTypes))
	}
	if takers < 3 {
		t.Fatalf("found only %d exported function(s) taking a table — the walk is broken and this guard "+
			"proves nothing", takers)
	}

	for key := range frtbTablelessRefusalExempt {
		if !seen[key] {
			offenders = append(offenders, "the exemption for "+key+" is DEAD: it now returns an error, "+
				"or no such function exists")
		}
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("%d FRTB component(s) cannot refuse an uncovering table:\n\n  %s",
			len(offenders), strings.Join(offenders, "\n\n  "))
	}
}

// frtbTablelessRefusalExempt names exported functions that take a supervisory
// table and legitimately cannot refuse it, with the reason.
var frtbTablelessRefusalExempt = map[string]string{
	// NOT A PRODUCTION ENTRY POINT. Its only non-test caller is maxScenario, which
	// Charge reaches ONLY after coversBuckets has proved every bucket present in
	// the sensitivities has a risk weight — the check whose own comment names this
	// exact hazard ("rw[bucket] on a missing key is 0, so the sensitivity is
	// weighted to nothing and vanishes inside an aggregate that is otherwise
	// computed correctly"). It is exported for the MAR21 worked-example
	// reconciliation in internal/risk/benchmarks/frtb.go, which passes tables it
	// constructs itself.
	//
	// If it ever acquires a caller outside this package that is not a benchmark,
	// this exemption is wrong and the validation must move into it.
	"internal/regulatory/frtb/sbm.go:ChargeForScenario": "#617 — guarded by coversBuckets in its only production caller",
}

// holdsAMap reports whether t is a map type or a struct with a map field.
func holdsAMap(t ast.Expr) bool {
	switch v := t.(type) {
	case *ast.MapType:
		return true
	case *ast.StructType:
		if v.Fields == nil {
			return false
		}
		for _, f := range v.Fields.List {
			if _, ok := f.Type.(*ast.MapType); ok {
				return true
			}
		}
	}
	return false
}

// takesATable reports whether any parameter's type names a table type.
func takesATable(params *ast.FieldList, tables map[string]bool) bool {
	for _, p := range params.List {
		if n, ok := baseTypeName(p.Type); ok && tables[n] {
			return true
		}
	}
	return false
}

// returnsAnError reports whether any result is error.
func returnsAnError(results *ast.FieldList) bool {
	if results == nil {
		return false
	}
	for _, r := range results.List {
		if n, ok := baseTypeName(r.Type); ok && n == "error" {
			return true
		}
	}
	return false
}

// baseTypeName strips pointers and slices to the underlying identifier.
func baseTypeName(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name, true
	case *ast.StarExpr:
		return baseTypeName(v.X)
	case *ast.ArrayType:
		return baseTypeName(v.Elt)
	}
	return "", false
}
