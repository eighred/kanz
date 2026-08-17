package arch

import (
	"go/ast"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// THE QUALITY-FLAG LIST MUST NAME EVERY FLAG, AND EVERY FLAG MUST HAVE A
// PRODUCER (#527).
//
// v1.QualityFlags is the slice every exhaustiveness sweep iterates —
// TestProtoFlags_EveryAPIFlagMaps in services/risk-engine/internal/grpcsrv most
// of all, which is what stops the gRPC translation layer dropping a flag it was
// never taught. That makes the slice a piece of enforcement machinery, and it is
// hand-maintained, so it has the failure mode all hand-maintained inventories
// have: a flag declared above it and not appended to it.
//
// # Why forgetting the append is worse than it looks
//
// It does not merely leave the new flag unlisted. Every guard downstream then
// sweeps a set that does not contain it and PASSES — the wire mapping is never
// checked, the flag is dropped in translation, and the caller receives a clean
// response for a number the engine had already marked untrustworthy. One
// omission disarms the guard AND commits the failure the guard exists to catch,
// with nothing red anywhere. That is the silence QualityFlagCurrencyExcluded was
// added to break, restored one layer out.
//
// # Both directions
//
// A missing entry disarms the sweeps. A stale entry — a name in the slice whose
// constant was renamed or deleted — makes the file stop compiling, so that
// direction is already the compiler's job and is not re-checked here.
//
// # Static, like measure_catalogue_test.go, and for the same reason
//
// risk_boundary_test.go forbids outsiders from importing internal/risk's
// implementation packages, and test/arch is an outsider. Reading both sides out
// of the source is exactly equivalent for this question and keeps working when
// the package does not compile, which is when an inventory check is most useful.
//
// # AST, not grep
//
// Both halves match *identifiers*, never text. A grep would match the flag names
// inside the very comments that discuss them — a-guard-that-matches-prose is a
// failure this estate has shipped three times — and the producer half below
// would then be satisfied by a doc comment mentioning a flag nothing emits.

const qualityFlagHome = "internal/risk/api/v1/engine.go"

// qualityFlagProducerTree is where a flag must actually be ATTACHED to a
// response. Deliberately narrower than "anywhere in the module": the gRPC
// translation layer under services/ references every flag by construction (it
// maps them), so counting it as a producer would let a flag that is declared,
// listed and mapped — but never emitted by anything — pass this guard. The risk
// module is the only thing that produces one.
const qualityFlagProducerTree = "internal/risk"

func TestEveryQualityFlagIsInTheExhaustiveList(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	declared, listed := qualityFlagDecls(t, root, fset)

	// NON-VACUITY, BOTH SIDES. A rename of the type or of the slice would leave
	// this comparing two empty sets, which is the failure mode one level up.
	if len(declared) < 3 {
		t.Fatalf("found only %d QualityFlag constants in %s — the const-shape match is broken, "+
			"not the estate", len(declared), qualityFlagHome)
	}
	if len(listed) == 0 {
		t.Fatalf("read no elements from `var QualityFlags` in %s — the literal was not found, so "+
			"this guard would compare against nothing", qualityFlagHome)
	}

	var missing []string
	for name := range declared {
		if !listed[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%d QualityFlag constant(s) are declared and ABSENT from `var QualityFlags`: %v\n\n"+
			"Append them. The slice is not documentation — TestProtoFlags_EveryAPIFlagMaps "+
			"iterates it to prove the query.v1 gRPC mapping is exhaustive, so a flag missing "+
			"here is a flag whose wire mapping is never checked. It then arrives at a caller as "+
			"nothing at all, and the response looks clean for a number the engine had marked "+
			"untrustworthy (#527).", len(missing), missing)
	}
}

// AND EVERY LISTED FLAG MUST BE PRODUCED SOMEWHERE.
//
// A flag can be declared, appended to the slice, mapped onto the wire and proven
// exhaustive by a test while NOTHING EVER ATTACHES IT TO A RESPONSE. Every guard
// is green and the signal does not exist. This is the same rule
// stored_series_has_a_producer_test.go applies to enumerated values on durable
// records, at the query surface instead.
func TestEveryQualityFlagHasAProducer(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	_, listed := qualityFlagDecls(t, root, fset)
	if len(listed) == 0 {
		t.Fatalf("read no elements from `var QualityFlags` in %s — nothing to check", qualityFlagHome)
	}

	// Every identifier used anywhere in the risk module OUTSIDE the file that
	// declares them. Identifiers only: a comment naming a flag is not a producer,
	// and matching text would make this guard satisfied by its own subject
	// matter.
	used := map[string]bool{}
	files := 0
	walkGoFiles(t, root, qualityFlagProducerTree, fset, func(rel string, f *ast.File) {
		if rel == qualityFlagHome {
			return
		}
		files++
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && strings.HasPrefix(id.Name, "QualityFlag") {
				used[id.Name] = true
			}
			return true
		})
	})
	if files == 0 {
		t.Fatalf("walked zero production files under %s — the path moved and this guard is "+
			"comparing against nothing", qualityFlagProducerTree)
	}

	var dark []string
	for name := range listed {
		if !used[name] {
			dark = append(dark, name)
		}
	}
	if len(dark) > 0 {
		sort.Strings(dark)
		t.Errorf("%d quality flag(s) are declared and listed but NOTHING under %s/ ever attaches "+
			"them to a response: %v\n\n"+
			"A flag no producer emits is a signal that does not exist, and every guard around it "+
			"stays green — the mapping test passes, the enum is on the wire, and a caller waiting "+
			"for the flag waits forever. Either produce it (engine.withCoverageFlags is where the "+
			"coverage flags are attached) or delete it.",
			len(dark), qualityFlagProducerTree, dark)
	}
}

// qualityFlagDecls reads both sides out of the api/v1 source: the set of
// `QualityFlagXxx QualityFlag = "..."` constant identifiers, and the set of
// identifiers listed in the `var QualityFlags` slice literal.
func qualityFlagDecls(t *testing.T, root string, fset *token.FileSet) (declared, listed map[string]bool) {
	t.Helper()
	declared, listed = map[string]bool{}, map[string]bool{}
	found := false

	walkGoFiles(t, root, "internal/risk/api", fset, func(rel string, f *ast.File) {
		if rel != qualityFlagHome {
			return
		}
		found = true
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				switch gen.Tok {
				case token.CONST:
					if !isIdentNamed(vs.Type, "QualityFlag") {
						continue
					}
					for _, ident := range vs.Names {
						declared[ident.Name] = true
					}
				case token.VAR:
					if len(vs.Names) != 1 || vs.Names[0].Name != "QualityFlags" || len(vs.Values) != 1 {
						continue
					}
					lit, ok := vs.Values[0].(*ast.CompositeLit)
					if !ok {
						continue
					}
					for _, elt := range lit.Elts {
						if id, ok := elt.(*ast.Ident); ok {
							listed[id.Name] = true
						}
					}
				}
			}
		}
	})
	if !found {
		t.Fatalf("%s was not walked — the path moved and this guard is comparing against nothing",
			qualityFlagHome)
	}
	return declared, listed
}
