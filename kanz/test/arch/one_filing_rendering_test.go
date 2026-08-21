package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A FILED NUMBER HAS ONE RENDERING, AND IT LIVES IN internal/filing (#633).
//
// # What went wrong without it
//
// "A signed filing" was implemented twice, byte-identically: internal/regulatory
// (FRTB / Form PF / AIFMD) and internal/sustainability (TCFD / SFDR) each held
// their own LineItem, Report, Signer, HashSigner, BuildReport, canonical() and
// Lookup — serving ONE service. Nothing related them, and both had already lost
// something the other kept:
//
//  1. THE RENDERING. Each package had a LineItem.MarshalJSON emitting dec.Str, and
//     NEITHER was ever called: services/regulatory built the JSON body by hand, in
//     two functions, and the climate one passed the *big.Rat straight to
//     encoding/json. big.Rat's TextMarshaler is RatString(), so a WACI of 1234.5678
//     was SERVED as "5429686605511341/4398046511104" while the signature in the same
//     response committed to "1234.5678". A filing whose body cannot reproduce its own
//     signature is not a signed filing; it is a number with a hash stapled to it.
//
//  2. THE NON-FINITE REFUSAL. regulatory.BuildReport refused a nil value — how a
//     NaN/±Inf model output arrives at this boundary. sustainability.BuildReport did
//     not, so a non-finite climate metric was filed as "0" and signed, despite
//     disclosure.go's comment asserting the refusal it did not have.
//
// Neither divergence could produce a compile error or a test failure, because the
// two copies had no relationship to break.
//
// # What it checks
//
// Two fingerprints, each default-deny outside internal/filing:
//
//	RENDERER  — a composite literal or struct whose JSON-visible field names are
//	            code + label + value. That IS a rendered line item, whatever the
//	            function is called. Both hand-rolled renderers matched it, and so
//	            did both dead MarshalJSON methods.
//	CANONICAL — a `.Code + "=" + …` concatenation: the signed form of a line item.
//	            Both canonical() bodies matched it.
//
// Read from the AST rather than by name: the two renderers were called regReport
// and climateReport, and a third would be called something else again.
//
// # What it does not cover
//
// internal/validation signs a report too, and is deliberately out of scope: its
// canonical form is over benchmark Cases (name / got / want / tolerance), its
// Signer returns no error, and it has no line items. It shares the word "signed",
// not the concept — folding it in would mean changing its error contract to make
// a guard tidy. Its own drift risk (it already uses RFC3339Nano where the filings
// use RFC3339) is a real but separate concern.
//
// A third renderer written in a completely different shape — a fmt.Sprintf of a
// JSON body, say — is also out of reach. This is a tripwire on the shape the real
// copies shared, not a proof of absence; internal/filing carries the behavioural
// tests and services/regulatory carries the end-to-end re-derivation
// (TestClimateFilingServesTheDecimalItSigned), which is what actually proves the
// property.
func TestThereIsOneFilingRendering(t *testing.T) {
	root := moduleRoot(t)

	var offenders []string
	seen := map[string]bool{}
	scanned := 0
	homeRenderer, homeCanonical := 0, 0

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case "gen", "testdata", ".git", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		name := info.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)

		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v — the guard cannot check what it cannot read", rel, perr)
		}
		scanned++

		home := strings.HasPrefix(rel, "internal/filing/")
		for _, fn := range filingRenderers(file) {
			if home {
				homeRenderer++
				continue
			}
			key := rel + ":" + fn + ":renderer"
			seen[key] = true
			if _, ok := filingRenderingExempt[key]; ok {
				continue
			}
			offenders = append(offenders, key+" — builds a code/label/value line item outside "+
				"internal/filing. That is a second rendering of a FILED number: the last two "+
				"disagreed with the bytes their own signature covered, and the climate filing went "+
				"out in rational a/b form (#633). Put the line items into filing.Report.Body and "+
				"add your extra keys to the map it returns.")
		}
		for _, fn := range filingCanonicalizers(file) {
			if home {
				homeCanonical++
				continue
			}
			key := rel + ":" + fn + ":canonical"
			seen[key] = true
			if _, ok := filingRenderingExempt[key]; ok {
				continue
			}
			offenders = append(offenders, key+" — concatenates a line item's Code with \"=\" outside "+
				"internal/filing. That is a second CANONICAL form, and the signature over it is the "+
				"only thing making a filing authoritative. Two of these existed and drifted with no "+
				"compile error (#633). Sign filing.Report.Canonical().")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// NON-VACUITY, the walk half. This module has hundreds of non-test Go files;
	// a walk that scanned none would pass however many copies had grown back.
	if scanned < 200 {
		t.Fatalf("scanned only %d non-test Go file(s) — the walk is broken and this guard proves "+
			"nothing", scanned)
	}
	// NON-VACUITY, the match halves. If either fingerprint stops matching — the
	// struct tags are renamed, the canonical separator changes — this guard finds
	// nothing anywhere and passes while asserting nothing, INCLUDING that the one
	// real implementation still exists.
	if homeRenderer == 0 {
		t.Fatalf("found no code/label/value rendering in internal/filing — there must be exactly "+
			"one (LineItem.MarshalJSON). Either it moved, or the fingerprint stopped matching and "+
			"this guard is asserting nothing. %d file(s) scanned", scanned)
	}
	if homeCanonical == 0 {
		t.Fatalf("found no `Code + \"=\"` canonical form in internal/filing — there must be exactly "+
			"one (Report.Canonical). Either it moved, or the fingerprint stopped matching and this "+
			"guard is asserting nothing. %d file(s) scanned", scanned)
	}

	// DEAD-ENTRY ARM: an exemption whose site no longer carries the shape has
	// outlived its repair and must not sit there granting permission.
	for key, reason := range filingRenderingExempt {
		if !seen[key] {
			offenders = append(offenders, "the exemption for "+key+" ("+reason+") is DEAD: no such "+
				"site exists any more")
		}
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("%d second implementation(s) of a filed number's rendering or canonical form:\n\n  %s",
			len(offenders), strings.Join(offenders, "\n\n  "))
	}
}

// filingRenderingExempt names sites outside internal/filing allowed to carry a
// filing rendering or canonical form, keyed "path:FuncName:kind", with the reason
// and the issue that retires each.
//
// Empty, and that is the point: both copies were retired into internal/filing and
// neither needed to stay. An entry here is permission; the dead-entry arm above
// removes it when the site goes.
var filingRenderingExempt = map[string]string{}

// filingRenderers returns the names of functions containing a composite literal
// or struct whose JSON-visible names are exactly the line-item triple.
//
// Both spellings the real copies used are covered: the hand-rolled
// map[string]any{"code": …, "label": …, "value": …}, and the anonymous struct
// with `json:"code"` / `json:"label"` / `json:"value"` tags inside MarshalJSON.
func filingRenderers(f *ast.File) []string {
	return filingSitesIn(f, func(n ast.Node) bool {
		switch t := n.(type) {
		case *ast.CompositeLit:
			names := map[string]bool{}
			for _, elt := range t.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				lit, ok := kv.Key.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				names[strings.ToLower(strings.Trim(lit.Value, "`\""))] = true
			}
			return names["code"] && names["label"] && names["value"]
		case *ast.StructType:
			names := map[string]bool{}
			if t.Fields == nil {
				return false
			}
			for _, fld := range t.Fields.List {
				if fld.Tag == nil {
					continue
				}
				tag := strings.ToLower(fld.Tag.Value)
				for _, want := range []string{"code", "label", "value"} {
					if strings.Contains(tag, `json:"`+want) {
						names[want] = true
					}
				}
			}
			return names["code"] && names["label"] && names["value"]
		}
		return false
	})
}

// filingCanonicalizers returns the names of functions concatenating a `.Code`
// selector with the literal "=" — the signed form of a line item.
func filingCanonicalizers(f *ast.File) []string {
	return filingSitesIn(f, func(n ast.Node) bool {
		bin, ok := n.(*ast.BinaryExpr)
		if !ok || bin.Op != token.ADD {
			return false
		}
		return exprHasStringLit(bin, "=") && exprSelects(bin, "Code")
	})
}

// filingSitesIn returns the names of the top-level funcs whose bodies contain a
// node matching. A file-level match (a package-level var) is reported under the
// synthetic name "<file>".
func filingSitesIn(f *ast.File, match func(ast.Node) bool) []string {
	var out []string
	inFunc := map[string]bool{}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Name == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if n != nil && match(n) {
				inFunc[fn.Name.Name] = true
			}
			return true
		})
	}
	for name := range inFunc {
		out = append(out, name)
	}
	// Package-level declarations, so a table of rendered line items outside a
	// function is not invisible to this.
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		hit := false
		ast.Inspect(gd, func(n ast.Node) bool {
			if n != nil && match(n) {
				hit = true
			}
			return true
		})
		if hit {
			out = append(out, "<file>")
			break
		}
	}
	sort.Strings(out)
	return out
}

// exprHasStringLit reports whether e contains the given string literal.
func exprHasStringLit(e ast.Expr, want string) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if ok && lit.Kind == token.STRING && strings.Trim(lit.Value, "`\"") == want {
			found = true
		}
		return true
	})
	return found
}

// exprSelects reports whether e contains a selector ending in name (li.Code).
func exprSelects(e ast.Expr, name string) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if ok && sel.Sel != nil && sel.Sel.Name == name {
			found = true
		}
		return true
	})
	return found
}
