package arch

// A PARTICIPATION RATE NEVER TRAVELS WITHOUT THE QUALITY THAT BOUNDS IT (#1007).
//
// # The failure this exists to refuse
//
// EXECUTION_ALGO_POV enforces its cap against a volume-profile FORECAST at
// admission. order.v1.ExecutionAttributionRecorded now carries what the
// participation actually TURNED OUT to be — and the whole value of that number is
// that a reader can tell a measured rate from one nobody could compute. The
// denominator is the volume that printed in a slice's interval, and the only
// realised-volume record the estate keeps emits NO CANDLE for a minute in which
// nothing traded, so "unobservable" is the ordinary case on exactly the thin
// instrument a cap exists for.
//
// The way that goes wrong is not malice, it is a default: a rate comes back nil,
// nil is awkward, and the field is filled with something. A record then reports
// 0% participation for an interval nobody watched — the calmest possible reading,
// manufactured in the one state the control was written for. That is #866's
// fabricated-leg failure moved one field over, and no test of the arithmetic
// would catch it, because the arithmetic stays correct.
//
// # The three arms
//
//  1. EVERY QUALITY THE SCHEMA DECLARES HAS A PRODUCER. A value the OMS never
//     emits is a state every consumer must handle and will never see; worse, a
//     branch DELETED from the producer collapses two different claims into one
//     silently — PARTIAL folded into UNOBSERVABLE would discard a measured,
//     real exceedance because a neighbouring minute was quiet.
//
//  2. THE METRIC'S SEED LIST IS THE SCHEMA'S OWN VOCABULARY. cmd/oms seeds
//     kanz_oms_participation_measurements_total from order.ParticipationQualities,
//     and seeding is the only reason a rule over that counter can fire: a
//     CounterVec exports NO series for a label it has never been incremented
//     with, so an OMS that measures nothing and one four minutes old are the same
//     observation unless the zeros are there. A label missing from that slice is
//     an alert that is silent in the state it detects.
//
//  3. A RATE IS ONLY EVER WRITTEN BESIDE A QUALITY. Any function assigning
//     RealisedParticipationRate or MaxSliceParticipationRate must assign
//     ParticipationQuality in the same body, so a rate cannot reach the wire from
//     a path that never said how much of it was observed.
//
// # Derivation, not lists
//
// The quality set comes from the generated protobuf DESCRIPTOR — the schema
// itself — and the metric labels are derived from it by the same rule the
// producer uses, rather than retyped here. A hand-written copy of a set is the
// artifact that rots, and the defect this guard is closest to (#806, #803) is
// precisely "somebody enumerated a set by hand and missed a member".
//
// Comments are not parsed into the AST (mode 0) and nothing below matches source
// text, so this file's own prose can neither satisfy nor defeat it.

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

const (
	// participationProducerDir is where the OMS turns a measurement into the FACT.
	participationProducerDir = "services/oms/internal/order"
	// participationSeedList is the exported slice cmd/oms seeds the counter from.
	participationSeedList = "ParticipationQualities"
	// participationQualityPrefix is the enum's own name prefix; stripping it and
	// lower-casing is how a wire value becomes a metric label, in the producer and
	// therefore here.
	participationQualityPrefix = "PARTICIPATION_QUALITY_"
)

// participationRateFields are the wire fields that must never be written without
// a quality beside them. Named rather than derived because the property is about
// these two specifically — the cap and the interval counts are meaningful on an
// UNOBSERVABLE record and are deliberately still written there.
var participationRateFields = []string{"RealisedParticipationRate", "MaxSliceParticipationRate"}

// wireQualities is every ParticipationQuality the schema declares except the
// zero value, as (Go identifier, metric label) pairs.
//
// UNSPECIFIED IS EXCLUDED ON PURPOSE. It is the value a record carries when it
// does not say what it was measured against, which order.v1's own doc rules
// nothing may aggregate — so it must have no seeded series and no ordinary
// producer path.
func wireQualities(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	values := orderpb.ParticipationQuality(0).Descriptor().Values()
	for i := range values.Len() {
		name := string(values.Get(i).Name())
		if !strings.HasPrefix(name, participationQualityPrefix) {
			t.Fatalf("ParticipationQuality value %q does not carry the %q prefix — the producer "+
				"and this guard both derive the metric label by stripping it, so a value outside "+
				"the convention would be counted under a label nothing seeds",
				name, participationQualityPrefix)
		}
		short := strings.TrimPrefix(name, participationQualityPrefix)
		if short == "UNSPECIFIED" {
			continue
		}
		out["ParticipationQuality_"+name] = strings.ToLower(short)
	}
	// NON-VACUITY. A descriptor lookup that returned nothing would pass every arm
	// below while checking none of them.
	if len(out) < 3 {
		t.Fatalf("only %d ParticipationQuality values found in the descriptor: %v — the schema "+
			"lookup is broken, and every assertion here is about that set", len(out), out)
	}
	return out
}

// participationProducerFiles parses the OMS package that builds the FACT.
//
// goFilesIn and parseNoComments are the directory's own helpers, and
// parseNoComments matters here: comments are NOT attached (mode 0), so this
// file's prose — which names every quality it checks for — can neither satisfy
// nor defeat the identifier scan below.
func participationProducerFiles(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	dir := filepath.Join(moduleRoot(t), filepath.FromSlash(participationProducerDir))
	var files []*ast.File
	for _, path := range goFilesIn(t, dir) {
		files = append(files, parseNoComments(t, fset, path))
	}
	if len(files) == 0 {
		t.Fatalf("no non-test files parsed from %s — this guard is blind", participationProducerDir)
	}
	return fset, files
}

// TestEveryParticipationQualityIsProducedAndSeeded is arms 1 and 2.
func TestEveryParticipationQualityIsProducedAndSeeded(t *testing.T) {
	want := wireQualities(t)
	_, files := participationProducerFiles(t)

	// Arm 1: every quality is named as an IDENTIFIER somewhere in the producer.
	// Identifiers, never source text, because this file and the producer both
	// discuss these values in prose and three guards in this directory have
	// already passed while the thing they checked was deleted.
	produced := map[string]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			produced[sel.Sel.Name] = true
			return true
		})
	}
	var missing []string
	for ident := range want {
		if !produced[ident] {
			missing = append(missing, ident)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("these ParticipationQuality values are declared in the schema and NEVER produced "+
			"by %s:\n\n  %s\n\n"+
			"A quality nothing emits is a state every consumer must handle and will never see. The "+
			"way this arm fires in practice is a DELETED branch: fold PARTIAL into UNOBSERVABLE and "+
			"the platform silently discards a measured, real exceedance because a neighbouring "+
			"minute was quiet — silencing the alarm with exactly the market thinness that caused it.",
			participationProducerDir, strings.Join(missing, "\n  "))
	}

	// Arm 2: the seed list is exactly the schema's vocabulary, lower-cased.
	seeded := participationSeedValues(t, files)
	var wantLabels []string
	for _, label := range want {
		wantLabels = append(wantLabels, label)
	}
	sort.Strings(wantLabels)
	sort.Strings(seeded)
	if strings.Join(seeded, ",") != strings.Join(wantLabels, ",") {
		t.Errorf("%s = %v, want exactly %v.\n\n"+
			"cmd/oms seeds kanz_oms_participation_measurements_total from that slice. A label "+
			"missing from it exports NO series until the first decision produces it, so a rule "+
			"comparing against it gets an EMPTY VECTOR and stays silent — which is how ten "+
			"data-quality rules came to parse, deploy and never fire (alerts/README.md). A label "+
			"present with no schema value behind it is a series nothing can increment, sitting at "+
			"zero forever and reading as \"measured, and never happened\".",
			participationSeedList, seeded, wantLabels)
	}
}

// participationSeedValues reads the string literals out of the seed slice's
// composite literal, following the constants it names.
func participationSeedValues(t *testing.T, files []*ast.File) []string {
	t.Helper()
	consts := map[string]string{}
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if v, err := strconv.Unquote(lit.Value); err == nil {
							consts[name.Name] = v
						}
					}
				}
			}
		}
	}

	var out []string
	found := false
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != 1 || vs.Names[0].Name != participationSeedList {
					continue
				}
				found = true
				if len(vs.Values) != 1 {
					continue
				}
				lit, ok := vs.Values[0].(*ast.CompositeLit)
				if !ok {
					continue
				}
				for _, el := range lit.Elts {
					switch x := el.(type) {
					case *ast.Ident:
						if v, ok := consts[x.Name]; ok {
							out = append(out, v)
							continue
						}
						t.Errorf("%s names %s, which is not a string constant in this package — "+
							"the seeded label cannot be established, so this guard cannot say "+
							"whether the metric's vocabulary matches the schema's",
							participationSeedList, x.Name)
					case *ast.BasicLit:
						if v, err := strconv.Unquote(x.Value); err == nil {
							out = append(out, v)
						}
					}
				}
			}
		}
	}
	if !found {
		t.Fatalf("%s is not declared in %s — cmd/oms seeds the participation counter from it, and "+
			"this guard cannot check a list it cannot find", participationSeedList,
			participationProducerDir)
	}
	return out
}

// TestAParticipationRateIsWrittenBesideItsQuality is arm 3.
//
// It is about the FUNCTION rather than the statement, deliberately: proving the
// two assignments are ordered or conditional on each other needs dataflow this
// test cannot afford, while "no function writes a rate without also writing the
// quality" is exactly the shape the fabrication defect takes — a second builder,
// somewhere else, filling the number and not the verdict.
func TestAParticipationRateIsWrittenBesideItsQuality(t *testing.T) {
	fset, files := participationProducerFiles(t)

	rate := map[string]bool{}
	for _, f := range participationRateFields {
		rate[f] = true
	}

	checked := 0
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			writes := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, lhs := range as.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok {
						writes[sel.Sel.Name] = true
					}
				}
				return true
			})
			var wrote []string
			for name := range writes {
				if rate[name] {
					wrote = append(wrote, name)
				}
			}
			if len(wrote) == 0 {
				continue
			}
			checked++
			if writes["ParticipationQuality"] {
				continue
			}
			sort.Strings(wrote)
			t.Errorf("%s:%d %s writes %v and never writes ParticipationQuality.\n\n"+
				"A participation rate with no quality beside it is a number a reader cannot tell "+
				"from one nobody could compute. The denominator is the volume that printed in a "+
				"slice's interval, and the estate's only realised-volume record emits no candle "+
				"for a minute in which nothing traded — so a rate written without a verdict "+
				"reports the calmest possible participation in exactly the thin market the cap "+
				"exists for.",
				participationProducerDir, fset.Position(fn.Pos()).Line, fn.Name.Name, wrote)
		}
	}

	// NON-VACUITY. If nothing writes a rate at all, every arm above is satisfied
	// by an estate that no longer measures participation — green, and measuring
	// nothing.
	if checked == 0 {
		t.Fatalf("no function in %s assigns any of %v — the participation measurement is not "+
			"reaching the FACT at all, or this analysis stopped recognising the assignment",
			participationProducerDir, participationRateFields)
	}
}
