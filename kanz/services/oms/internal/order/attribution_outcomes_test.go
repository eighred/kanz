package order

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// THE SEED LIST MUST BE DERIVED FROM THE CONSTANTS, NOT WRITTEN BESIDE THEM.
//
// AttributionOutcomes is what cmd/oms seeds the counter's series from, and
// seeding is the only reason the #875 alert can fire. An outcome that is
// declared, counted, and missing from that slice is therefore not a cosmetic
// omission: its series does not exist until the first order produces it, which
// is exactly the "nothing configured looks like checked, and fine" state the
// counter was added to end.
//
// A hand-written list checked by a hand-written list is two copies of the same
// mistake, so this reads the CONST BLOCK ITSELF out of attribution.go's AST and
// requires the two sets to be equal in both directions:
//
//   - a constant missing from the slice is an unseeded label;
//   - a value in the slice matching no constant is a series seeded for an
//     outcome nothing can emit, which is a permanent zero an operator would read
//     as "measured, and never happened".
//
// The AST rather than a grep, because the values are Go string literals and the
// only thing that reliably knows a literal from the same characters inside a
// doc comment is the parser. This file's own prose mentions every one of these
// values, and a regex over the source would match here (#875, and the guard that
// matched its own comments before it).
func TestEveryAttributionOutcomeConstantIsSeeded(t *testing.T) {
	declared := attributionOutcomeConstants(t)

	// NON-VACUITY. An extractor that finds nothing passes every assertion below
	// while proving none of them. Eight constants are declared today; the floor is
	// deliberately below that so adding one does not fail this arm, and
	// deliberately above zero so a broken parse cannot.
	if len(declared) < 4 {
		t.Fatalf("found only %d outcome constants in attribution.go: %v\n\n"+
			"The extractor is broken, not the source. Every assertion in this test is about "+
			"that set, so an empty or near-empty one makes the guard green while checking nothing.",
			len(declared), declared)
	}

	seeded := map[string]bool{}
	for _, o := range AttributionOutcomes {
		if seeded[o] {
			t.Errorf("AttributionOutcomes lists %q twice — the counter would be seeded twice for "+
				"one label, which is harmless, but the duplicate is usually a constant renamed in "+
				"one place and copied in the other", o)
		}
		seeded[o] = true
	}

	var unseeded []string
	for name, value := range declared {
		if !seeded[value] {
			unseeded = append(unseeded, name+" = "+strconv.Quote(value))
		}
	}
	if len(unseeded) > 0 {
		sort.Strings(unseeded)
		t.Errorf("these attribution outcomes are declared and countable but NOT in "+
			"AttributionOutcomes:\n\n  %s\n\n"+
			"cmd/oms seeds kanz_oms_execution_attributions_total from that slice, so each of these "+
			"exports no series until the first order produces it. An alert comparing against the "+
			"missing series gets an EMPTY VECTOR and stays silent — which is how ten data-quality "+
			"rules came to parse, deploy and never fire (alerts/README.md).",
			strings.Join(unseeded, "\n  "))
	}

	values := map[string]bool{}
	for _, v := range declared {
		values[v] = true
	}
	var phantom []string
	for _, o := range AttributionOutcomes {
		if !values[o] {
			phantom = append(phantom, strconv.Quote(o))
		}
	}
	if len(phantom) > 0 {
		sort.Strings(phantom)
		t.Errorf("AttributionOutcomes seeds label values no constant in attribution.go declares: %s\n\n"+
			"A seeded series nothing can increment sits at zero forever, and a permanent zero reads "+
			"as \"this outcome was measured and never happened\" rather than \"this outcome does not "+
			"exist\". Delete it, or point it at the constant it was meant to name.",
			strings.Join(phantom, ", "))
	}
}

// attributionOutcomeConstants returns every `outcome*` string constant declared
// in attribution.go, keyed by its Go identifier.
//
// It parses the file rather than the whole package on purpose: the const block
// it checks lives there, and a package-wide scan would silently start covering
// constants from files this guard was never reasoned about.
func attributionOutcomeConstants(t *testing.T) map[string]string {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "attribution.go", nil, 0)
	if err != nil {
		t.Fatalf("parse attribution.go: %v — this guard reads the const block out of that file; "+
			"if it moved, move the guard rather than deleting the check", err)
	}

	out := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "outcome") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					// A non-literal outcome would be an unbounded label in the making,
					// which the const block's own comment forbids. Fail rather than skip:
					// skipping is how the value would leave this guard's sight.
					t.Fatalf("%s is not a string literal — the outcome label set is bounded by "+
						"construction, and a computed value is an unbounded label a step away",
						name.Name)
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s = %s: %v", name.Name, lit.Value, err)
				}
				out[name.Name] = value
			}
		}
	}
	return out
}
