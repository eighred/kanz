package arch

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A FUNCTION MAY NOT WRITE A MANDATE VERDICT IT WAS GIVEN NO MANDATE TO REACH (#646).
//
// # What went wrong without it
//
// optimization.Rebalance takes current weights, target weights, NAV, prices, a
// threshold and a timestamp. It takes no mandate, no compliance engine and no
// classifier — it is in no position to say anything about compliance — and it
// ended every call with
//
//	return RebalanceProposal{..., MandateFeasible: true}
//
// That field is the gate on whether a rebalance becomes live capital commands:
// bridge.ToOrders read it and emitted a SubmitOrder per trade. The only code
// that ever computed a real answer was optimization.CheckMandate, reached only
// from optimization.Propose, which had NO CALLER ANYWHERE IN THE MODULE — the
// optimization service's handler ran Optimize and Rebalance inline and skipped
// the check. So the check had never once executed on this platform, and every
// proposal it produced was certified compliant by its own constructor.
//
// The repair was to make the verdict a three-state MandateStatus whose zero
// value is MandateUnchecked. That fixes the instance. This guard is what stops
// the shape returning: the defect was not the value `true`, it was a function
// asserting an outcome it had no input to compute.
//
// # What this checks
//
// Every non-test function that WRITES the MandateStatus field — as a composite
// literal key or as an assignment to a `.MandateStatus` selector — must have
// been handed one of:
//
//	a *…pb.Mandate parameter        it can run the check itself
//	a MandateStatus parameter       it was handed a verdict someone else computed
//
// Writing the constant MandateUnchecked is always allowed: it asserts nothing,
// which is the whole point of having a state that means "not checked".
//
// # Why it is a parameter rule and not a call-graph rule
//
// "Must call CheckMandate" was the obvious alternative and it is weaker in both
// directions. It passes for a function that calls CheckMandate and throws the
// answer away, and it fails for Propose the day the check moves behind a seam.
// The parameter rule asks the question the defect actually answers wrongly —
// COULD this function know? — and it is decidable from the signature alone.
//
// # The known weakness, stated rather than discovered later
//
// This reads SYNTAX, and a parameter is not proof the function used it: a
// Propose that took a mandate and ignored it would pass here. What catches that
// is TestProposeWithNoMandateIsUncheckedNotFeasible and the ToOrders refusal
// tests, which assert the values rather than the shapes. The non-vacuity arms
// below are what keep this file from becoming the only thing standing, since a
// guard keyed on a field name checks nothing at all once the field is renamed.

// mandateVerdictWriterExempt maps "<pkgdir>.<Func>" to why it may assert a
// verdict without being handed a mandate. Default-deny: an entry names the
// issue that retires it.
//
// IT IS EMPTY, AND THAT IS THE POINT. The one function that ever needed an entry
// was Rebalance, and the entry it would have carried — "it stamps feasible
// because the caller is expected to overwrite it" — is precisely the reasoning
// that produced #646. If a candidate appears, prefer giving the function the
// mandate over writing the exemption.
var mandateVerdictWriterExempt = map[string]string{}

func TestNoFunctionAssertsAMandateVerdictItCannotCompute(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	var writers []string // "<pkgdir>.<Func>" per function that writes a verdict
	compliant := map[string]bool{}
	fieldDeclared := false

	walkGoFiles(t, root, ".", fset, func(rel string, f *ast.File) {
		pkgDir := filepath.ToSlash(filepath.Dir(rel))
		// The generated protobuf SDK is a vendored artifact, not this estate's code.
		if strings.HasPrefix(pkgDir, "kanz-schemas-go/") {
			return
		}

		// EVIDENCE FOR THE FIRST NON-VACUITY ARM: the field must exist, and it must
		// be a MandateStatus rather than a bool. A bool cannot carry "not checked" at
		// all, which is the state the whole repair rests on — and a guard keyed on a
		// field name checks nothing once the field is gone.
		ast.Inspect(f, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok {
				return true
			}
			for _, fld := range st.Fields.List {
				for _, nm := range fld.Names {
					if nm.Name == "MandateStatus" && typeName(fld.Type) == "MandateStatus" {
						fieldDeclared = true
					}
				}
			}
			return true
		})

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			var writes []int
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.CompositeLit:
					for _, elt := range node.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "MandateStatus" &&
							!isUncheckedConst(kv.Value) {
							writes = append(writes, fset.Position(kv.Pos()).Line)
						}
					}
				case *ast.AssignStmt:
					for i, lhs := range node.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if !ok || sel.Sel.Name != "MandateStatus" {
							continue
						}
						if i < len(node.Rhs) && isUncheckedConst(node.Rhs[i]) {
							continue
						}
						writes = append(writes, fset.Position(sel.Pos()).Line)
					}
				}
				return true
			})
			if len(writes) == 0 {
				continue
			}
			key := pkgDir + "." + fn.Name.Name
			writers = append(writers, key)
			if hasMandateInput(fn.Type.Params) {
				compliant[key] = true
			}
		}
	})

	// NON-VACUITY, first arm: the field this guard is keyed on must exist, as a
	// MandateStatus. Rename it, or turn it back into a bool, and every rule below
	// matches nothing and reports a clean estate.
	if !fieldDeclared {
		t.Fatal("no struct in the module declares a MandateStatus field of type MandateStatus — " +
			"the verdict was renamed or reverted to a two-state value, and this guard now checks " +
			"nothing. A bool cannot distinguish 'no mandate was consulted' from 'consulted, and it " +
			"passed', which is #646")
	}

	// NON-VACUITY, second arm: somebody must write the verdict. A zero-writer
	// estate means the AST rules are broken, not that the code is clean —
	// optimization.Propose assigns it from CheckMandate on every call.
	if len(writers) == 0 {
		t.Fatal("no function in the module writes a MandateStatus — the composite-literal and " +
			"assignment rules are broken. optimization.Propose assigns proposal.MandateStatus from " +
			"CheckMandate and must be found")
	}

	// NON-VACUITY, third arm: the parameter rule must resolve at least one writer
	// as compliant. A hasMandateInput that answered false for everything would put
	// the whole estate in the failure list and drive the exemption map to cover a
	// bug in this file.
	if !compliant["internal/optimization.Propose"] {
		t.Fatalf("optimization.Propose was not recognised as holding a mandate — hasMandateInput or "+
			"the parameter walk is broken, and every other verdict in this list is being judged by "+
			"the same rule. Writers found: %v", sortedUnique(writers))
	}

	var unearned []string
	seen := map[string]bool{}
	for _, key := range sortedUnique(writers) {
		if compliant[key] {
			continue
		}
		if reason, ok := mandateVerdictWriterExempt[key]; ok {
			seen[key] = true
			t.Logf("%s: writes a verdict without a mandate, tracked — %s", key, reason)
			continue
		}
		unearned = append(unearned, key)
	}
	if len(unearned) > 0 {
		sort.Strings(unearned)
		t.Errorf("%d function(s) assert a mandate verdict without being given a mandate to compute "+
			"it from: %v.\n"+
			"A verdict is what bridge.ToOrders gates live SubmitOrder commands on. A function with "+
			"no mandate parameter cannot have checked anything, so the value it writes is a claim "+
			"about a control that did not run — and MandateUnchecked exists so it does not have to "+
			"make one (#646).\n"+
			"Give the function the mandate, leave the field at MandateUnchecked, or add an entry to "+
			"mandateVerdictWriterExempt naming the issue that retires it.",
			len(unearned), unearned)
	}

	// DEAD-ENTRY ARM: an exemption for a function that now holds a mandate, or no
	// longer writes a verdict, is a claim about the estate that is no longer true.
	for name, reason := range mandateVerdictWriterExempt {
		if seen[name] {
			continue
		}
		t.Errorf("exemption for %q is stale — it no longer writes a verdict, or it now takes a "+
			"mandate. Delete the entry (%s)", name, reason)
	}
}

// isUncheckedConst reports whether an expression is the MandateUnchecked
// constant, qualified or not. Writing it asserts nothing about compliance, so it
// is not a verdict and needs no mandate behind it.
func isUncheckedConst(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name == "MandateUnchecked"
	case *ast.SelectorExpr:
		return t.Sel.Name == "MandateUnchecked"
	}
	return false
}

// hasMandateInput reports whether a parameter list carries something the
// function could reach a verdict FROM: a *Mandate (it can run the check) or a
// MandateStatus (it was handed a verdict someone else computed).
func hasMandateInput(params *ast.FieldList) bool {
	if params == nil {
		return false
	}
	for _, field := range params.List {
		switch typeName(field.Type) {
		case "Mandate", "MandateStatus":
			return true
		}
	}
	return false
}

// typeName renders the bare name of a type expression, seeing through a pointer
// and a package qualifier so *compliancepb.Mandate, *Mandate and Mandate are the
// same thing to the rule above.
func typeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return typeName(t.X)
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.Ident:
		return t.Name
	}
	return ""
}

// sortedUnique renders a set of write-site keys for a message.
func sortedUnique(keys []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
