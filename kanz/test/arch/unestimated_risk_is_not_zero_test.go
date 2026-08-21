package arch

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A RISK NUMBER NOBODY COULD COMPUTE MUST NOT BE STORABLE AS A NUMBER (#621).
//
// # What went wrong without it
//
// optimization.Optimize ended every successful call with
//
//	res.ExpectedRisk = math.Sqrt(math.Max(quadForm(in.Covariance, w), 0))
//
// over a float64 field. Three separate inputs made that expression 0:
//
//   - a covariance of all zeros, which is symmetric and PSD and therefore passed
//     every check the optimizer had;
//   - SampleCovariance handed fewer than two observations, which RETURNED a
//     matrix of zeros by documented design;
//   - any rank-deficient Σ, for the portfolios in its null space.
//
// A genuinely riskless book also reports 0, so the field could not distinguish
// "we could not estimate this" from "we estimated it, and it is zero" — and
// services/optimization writes it straight into the /v1/propose response a PM
// approves capital against. MarketInputs had no observation-count field either,
// so an estimate over three periods and two genuinely collinear assets arrived
// as the same accepted matrix.
//
// The repair made ExpectedRisk a *float64, nil unless CovarianceQuality supports
// a number, and put that quality on the response beside it.
//
// # What this guard stops
//
// The pointer IS the mechanism, and it is exactly the kind of thing a later
// reader "simplifies": *float64 looks like ceremony next to a float64, the
// compiler then demands every producer be updated, and the quickest way to
// satisfy it is to write 0 again. That change compiles, passes the arithmetic
// tests (which assert values, not absence), and silently restores the
// conflation. The second arm keeps the explanation attached: a null risk with
// no quality beside it tells the reader nothing about why.
//
// # The known weakness, stated rather than discovered later
//
// This reads DECLARATIONS, not behaviour. A pointer that Optimize sets
// unconditionally would pass here; what catches that is
// TestZeroCovarianceDoesNotProduceAComputedZeroRisk and
// TestProposeDoesNotReportZeroRiskFromAZeroCovariance, which assert the values.
// The non-vacuity arms below are what keep this file from passing on an estate
// where the field has simply been renamed.

// expectedRiskFieldExempt maps "<pkgdir>.<Struct>.ExpectedRisk" to why that
// field may be a bare float64. Default-deny: an entry names the issue that
// retires it.
//
// IT IS EMPTY, AND THAT IS THE POINT. The reason an entry would carry — "this
// one is always computed, so it can never be absent" — is the reasoning the
// float64 rested on for the whole life of the field, and it was false for three
// different inputs at once.
var expectedRiskFieldExempt = map[string]string{}

// covarianceQualityHome is where the quality type lives. Named so a rename
// surfaces here rather than as a silently vacuous guard.
const covarianceQualityHome = "internal/optimization"

func TestAnUnestimatedRiskCannotBeReportedAsZero(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	type field struct{ key, typ string }
	var risks []field
	qualityFieldOwners := map[string]bool{} // "<pkgdir>.<Struct>" carrying a CovarianceQuality
	qualityTypeDeclared := false
	zeroConstName := ""

	walkGoFiles(t, root, ".", fset, func(rel string, f *ast.File) {
		pkgDir := filepath.ToSlash(filepath.Dir(rel))
		// The generated protobuf SDK is a vendored artifact, not this estate's code.
		if strings.HasPrefix(pkgDir, "kanz-schemas-go/") {
			return
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gen.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if s.Name.Name == "CovarianceQuality" && pkgDir == covarianceQualityHome {
						qualityTypeDeclared = true
					}
					st, ok := s.Type.(*ast.StructType)
					if !ok {
						continue
					}
					owner := pkgDir + "." + s.Name.Name
					for _, fld := range st.Fields.List {
						for _, nm := range fld.Names {
							switch nm.Name {
							case "ExpectedRisk":
								risks = append(risks, field{owner + ".ExpectedRisk", exprString(fld.Type)})
							case "CovarianceQuality":
								if typeName(fld.Type) == "CovarianceQuality" {
									qualityFieldOwners[owner] = true
								}
							}
						}
					}
				case *ast.ValueSpec:
					// The FIRST name in the CovarianceQuality iota block is the zero
					// value, and it is what a struct acquires by being constructed.
					if zeroConstName != "" || len(s.Names) == 0 || s.Type == nil {
						continue
					}
					if typeName(s.Type) == "CovarianceQuality" {
						zeroConstName = s.Names[0].Name
					}
				}
			}
		}
	})

	// NON-VACUITY, first arm: the type this guard is keyed on must exist where it
	// is expected. Rename or delete it and every rule below matches nothing.
	if !qualityTypeDeclared {
		t.Fatalf("no CovarianceQuality type is declared in %s — the explanation that accompanies a "+
			"withheld risk number is gone, and this guard now checks nothing (#621)", covarianceQualityHome)
	}

	// NON-VACUITY, second arm: the field walk must find the two structs the repair
	// reshaped. Finding none means the AST rules are broken, not that the estate
	// is clean.
	if len(risks) < 2 {
		t.Fatalf("found %d ExpectedRisk field(s); optimization.Result and "+
			"optimization.RebalanceProposal both declare one and must both be found. The struct "+
			"walk is broken, or the field was renamed and this guard is asserting nothing", len(risks))
	}

	// THE ZERO VALUE MUST MEAN "NOTHING EVALUATED THIS". A block reordered so that
	// FULL_RANK or OBSERVED lands on iota 0 would hand every zero-valued struct a
	// clean bill of health for free — the defect #646 removed from the mandate
	// verdict, re-created one field over.
	if zeroConstName != "CovarianceUnchecked" {
		t.Errorf("the zero value of CovarianceQuality is %q, want CovarianceUnchecked.\n"+
			"A proposal must not acquire a covariance verdict merely by being constructed: "+
			"RebalanceProposal is built by optimization.Rebalance, which is handed no covariance "+
			"at all (#621).", zeroConstName)
	}

	// THE POINTER ARM.
	var bare []string
	seen := map[string]bool{}
	sort.Slice(risks, func(i, j int) bool { return risks[i].key < risks[j].key })
	for _, r := range risks {
		if r.typ == "*float64" {
			continue
		}
		if reason, ok := expectedRiskFieldExempt[r.key]; ok {
			seen[r.key] = true
			t.Logf("%s is %s, tracked — %s", r.key, r.typ, reason)
			continue
		}
		bare = append(bare, r.key+" is "+r.typ)
	}
	if len(bare) > 0 {
		t.Errorf("%d ExpectedRisk field(s) cannot hold \"not estimable\": %v (want *float64).\n"+
			"A covariance of all zeros, a rank-deficient one, and an estimate over fewer periods "+
			"than assets all make √(wᵀΣw) exactly 0 — which is also what a riskless book reports. "+
			"This field lands in the /v1/propose response a PM approves capital against, so the "+
			"two must not be the same bytes (#621).\n"+
			"Make it a *float64, or add an entry to expectedRiskFieldExempt naming the issue that "+
			"retires it.", len(bare), bare)
	}

	// THE EXPLANATION ARM: every struct that can withhold a risk number must carry
	// the quality that says why. A bare null is not an answer.
	var unexplained []string
	for _, r := range risks {
		owner := strings.TrimSuffix(r.key, ".ExpectedRisk")
		if !qualityFieldOwners[owner] {
			unexplained = append(unexplained, owner)
		}
	}
	if len(unexplained) > 0 {
		sort.Strings(unexplained)
		t.Errorf("%v declare an ExpectedRisk but no CovarianceQuality beside it.\n"+
			"A null risk number with nothing explaining it leaves the caller exactly where the "+
			"zero did: unable to tell a riskless book from an unestimable one (#621).", unexplained)
	}

	// DEAD-ENTRY ARM: an exemption for a field that is now a pointer, or no longer
	// exists, is a claim about the estate that is no longer true.
	for name, reason := range expectedRiskFieldExempt {
		if seen[name] {
			continue
		}
		t.Errorf("exemption for %q is stale — the field is a pointer now, or it is gone. Delete "+
			"the entry (%s)", name, reason)
	}
}

// exprString renders a type expression as source-like text, so *float64 and
// float64 are distinguishable (typeName deliberately sees through the pointer).
func exprString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return "*" + exprString(t.X)
	case *ast.SelectorExpr:
		return exprString(t.X) + "." + t.Sel.Name
	case *ast.Ident:
		return t.Name
	case *ast.ArrayType:
		return "[]" + exprString(t.Elt)
	}
	return "?"
}
