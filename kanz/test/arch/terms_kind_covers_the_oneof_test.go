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

// EVERY ContractTerms VARIANT NEEDS A Kind, OR IT CAN BE STORED UNDER NO LABEL.
//
// # The gap this closes, which shipped
//
// #518 added a BOND case to the reference.v1.ContractTerms oneof and a doc
// comment for terms.KindBond — and no declaration. For a while the constant did
// not exist, so nothing could WRITE a bond row with the right kind.
//
// EVERY TEST STILL PASSED. The read path this repo actually uses, LatestAsOf,
// does not filter on kind: it resolves by (instrument_id, as_of) and decodes the
// blob, so the FI measures worked. What did not work was the half nothing
// exercised — ChainAsOf, which DOES filter by kind, and any writer choosing a
// label. The broken half and the tested half were disjoint.
//
// # Why a guard rather than remembering
//
// The oneof is where a variant is added, and the Kind constant is 500 lines away
// in another module. The two are a pair and nothing said so. A reviewer who adds
// `StructuredTerms structured = 14;` has no reason to look at a Go const block —
// and the failure is silent in exactly the way above: reads keep working, and a
// filtered query quietly returns nothing.
//
// # What it compares
//
// The oneof cases declared in reference/v1/contract.proto against terms.Kinds().
// Names are matched loosely — the proto field is `bond`, the constant is
// KindBond with value "BOND" — because the point is COVERAGE, not a naming
// convention. Requiring an exact spelling would make this a style rule and it
// would be argued with; requiring that every variant has SOME kind is the
// property that keeps a row findable.

func TestEveryContractTermsVariantHasAKind(t *testing.T) {
	root := moduleRoot(t)
	// kanz/ and kanz-schemas/ are siblings under the repo root.
	protoPath := filepath.Join(filepath.Dir(root), "kanz-schemas", "proto", "reference", "v1", "contract.proto")

	variants := oneofCases(t, protoPath, "ContractTerms")
	if len(variants) < 3 {
		t.Fatalf("found %d cases in the ContractTerms oneof (%v) — the proto moved or the parse "+
			"is broken, and this guard would be comparing against nothing", len(variants), variants)
	}

	declared := termsKinds(t, root)
	if len(declared) < 3 {
		t.Fatalf("found %d terms.Kind constants (%v) — the const block moved and this guard is "+
			"checking nothing", len(declared), declared)
	}

	// Loose match: a variant is covered when some kind's VALUE equals the field
	// name, case-insensitively. `bond` <-> "BOND".
	covered := map[string]bool{}
	for _, k := range declared {
		covered[strings.ToLower(k)] = true
	}
	var missing []string
	for _, v := range variants {
		if !covered[strings.ToLower(v)] {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("ContractTerms variant(s) %v have no terms.Kind constant.\n"+
			"A variant with no kind can be stored under no label and found by no filtered query "+
			"— ChainAsOf filters on kind, and a writer has nothing correct to pass. It fails "+
			"SILENTLY, because LatestAsOf does not filter on kind, so the read path the FI "+
			"measures use keeps working while the rest does not (#509).\n"+
			"Add the constant to internal/marketdata/terms and include it in Kinds().",
			missing)
	}

	// THE OTHER DIRECTION. A kind naming no variant is a label nothing can ever
	// carry, so a query filtering on it returns empty forever — which reads as
	// "we hold none of those" rather than as a dead constant.
	declaredSet := map[string]bool{}
	for _, v := range variants {
		declaredSet[strings.ToLower(v)] = true
	}
	var orphaned []string
	for _, k := range declared {
		if !declaredSet[strings.ToLower(k)] {
			orphaned = append(orphaned, k)
		}
	}
	if len(orphaned) > 0 {
		sort.Strings(orphaned)
		t.Errorf("terms.Kind constant(s) %v name no ContractTerms variant — a filter on them "+
			"returns empty forever, which reads as holding none rather than as a dead label",
			orphaned)
	}
}

// oneofCases returns the field names of the named message's oneof.
func oneofCases(t *testing.T, protoPath, message string) []string {
	t.Helper()
	raw, err := os.ReadFile(protoPath) //#nosec G304 -- a path this guard computes itself
	if err != nil {
		t.Fatalf("read %s: %v", protoPath, err)
	}
	body := string(raw)
	start := strings.Index(body, "message "+message+" {")
	if start < 0 {
		t.Fatalf("no message %s in %s", message, protoPath)
	}
	oneof := strings.Index(body[start:], "oneof ")
	if oneof < 0 {
		t.Fatalf("message %s has no oneof", message)
	}
	seg := body[start+oneof:]
	end := strings.Index(seg, "\n  }")
	if end < 0 {
		t.Fatalf("could not find the end of %s's oneof", message)
	}
	var out []string
	for _, line := range strings.Split(seg[:end], "\n") {
		line = strings.TrimSpace(line)
		// A field line looks like `OptionTerms option = 10;`. Comments and the
		// `oneof terms {` header are skipped.
		if line == "" || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "oneof") {
			continue
		}
		parts := strings.Fields(strings.TrimSuffix(line, ";"))
		if len(parts) < 4 || parts[2] != "=" {
			continue
		}
		out = append(out, parts[1])
	}
	sort.Strings(out)
	return out
}

// termsKinds reads the VALUES of the Kind constants out of the source, rather
// than importing the package — test/arch/risk_boundary_test.go forbids importing
// internal/risk from here, and reading the source keeps this guard uniform with
// the others in this directory (and working when the package does not compile).
func termsKinds(t *testing.T, root string) []string {
	t.Helper()
	path := filepath.Join(root, "internal", "marketdata", "terms", "postgres.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		id, ok := vs.Type.(*ast.Ident)
		if !ok || id.Name != "Kind" {
			return true
		}
		for _, v := range vs.Values {
			lit, ok := v.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			out = append(out, strings.Trim(lit.Value, `"`))
		}
		return true
	})
	sort.Strings(out)
	return out
}
