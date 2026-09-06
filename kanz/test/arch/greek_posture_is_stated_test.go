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

// NO DEPLOYMENT COMPUTES A GREEK, Delta ANSWERS ANYWAY, AND THAT MUST BE A
// STATED POSTURE RATHER THAN A SILENCE (#1055).
//
// compute.Delta returns the portfolio's signed base-currency sum — NetExposure
// under a second name — and it is registered in compute.DefaultRegistry, which
// every engine builds. compute.RegisterGreeks is the only thing that overwrites
// it, and it has no production caller anywhere in this estate. Unlike the VaR
// placeholder, which a market-data DSN switches off, there is no configuration in
// which this one does not serve.
//
// Since #1037 the OMS gate refuses to fold a declared placeholder, so a mandate
// naming Delta is a PERMANENT REFUSAL rather than a check. That direction is
// right; what was missing is that nobody could learn it except by having orders
// refused.
//
// # What this binds together
//
//	compute.DefaultRegistry                                    Delta stays registered — the decision
//	compute.RegisterGreeks                                     the sole registrar the inference rests on
//	services/risk-engine/internal/app/greek_model_posture.go    the posture
//	services/risk-engine/cmd/risk-engine/main.go                the process that registers it
//
// THE JUSTIFICATION IS DERIVED, NOT TYPED IN. The posture does not contain a
// sentence saying "RegisterGreeks is unwired". It reads compute.Dark and infers:
// RegisterGreeks installs the WHOLE Greek family in one call, so a single dark
// member proves it did not run, and any member that answers is answering from
// something else. This guard checks the premise that inference rests on — that
// RegisterGreeks is the sole registrar of the members DefaultRegistry does not
// register — so the day a second registrar appears the posture fails loudly
// instead of quietly becoming wrong. Same failure mode
// settlement_basis_absence_is_stated_test.go was written for, one axis over.
//
// # Why a guard and not a comment
//
// Because the comment IS the thing being protected: the exemption in
// measure_carries_its_coverage_test.go said for months that RegisterGreeks
// overwrites this placeholder, and it overwrites it in no deployment. Every
// assertion below reads the AST rather than raw source, because three guards in
// this repository have passed with the checked thing deleted by matching their own
// prose.

// greekPostureFile is where the posture lives. Cited by path so the citation is
// checkable: a guard pointing at a file that no longer exists asserts nothing.
var greekPostureFile = filepath.Join(
	"services", "risk-engine", "internal", "app", "greek_model_posture.go")

// riskEngineCompositionRoot is the process that must register the posture.
var riskEngineCompositionRoot = filepath.Join(
	"services", "risk-engine", "cmd", "risk-engine", "main.go")

// registrarsOfGreekMeasures returns, for each measure constant in the catalogued
// Greek family, the names of the functions under internal/risk/compute that
// register it — read off `<recv>.Register(MeasureX, ...)` call sites.
//
// DISCOVERED BY WALKING THE PACKAGE, not listed here. A second registrar added
// tomorrow is found without anyone remembering to add it, which is the whole
// point: the posture's justification is "one call installs the whole family", and
// a hand-written list would make that claim about the registrars somebody
// remembered.
func registrarsOfGreekMeasures(t *testing.T, root string) (family []string, byMeasure map[string][]string) {
	t.Helper()
	computeDir := filepath.Join(root, "internal", "risk", "compute")
	family = cataloguedGreekConstants(t, computeDir)

	inFamily := map[string]bool{}
	for _, name := range family {
		inFamily[name] = true
	}

	byMeasure = map[string][]string{}
	for _, path := range goFilesInTree(t, computeDir) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		// No parser.ParseComments: greeks.go's own doc says "RegisterGreeks
		// overwrites its placeholder implementation", and a scan that could see
		// comments would be reading the explanation instead of the call.
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Register" {
					return true
				}
				arg, ok := call.Args[0].(*ast.Ident)
				if !ok || !inFamily[arg.Name] {
					return true
				}
				byMeasure[arg.Name] = append(byMeasure[arg.Name], fn.Name.Name)
				return true
			})
		}
	}
	for m := range byMeasure {
		sort.Strings(byMeasure[m])
	}
	return family, byMeasure
}

// cataloguedGreekConstants returns the measure constant names the catalogue maps
// to FamilyGreeks, read off catalogue.go's map literal.
func cataloguedGreekConstants(t *testing.T, computeDir string) []string {
	t.Helper()
	var out []string
	for _, path := range goFilesInTree(t, computeDir) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || !strings.HasPrefix(key.Name, "Measure") {
				return true
			}
			val, ok := kv.Value.(*ast.Ident)
			if !ok || val.Name != "FamilyGreeks" {
				return true
			}
			out = append(out, key.Name)
			return true
		})
	}
	sort.Strings(out)
	return out
}

// TestGreekPostureDerivationPremiseHolds is the arm that keeps the posture's
// inference from outliving its evidence.
func TestGreekPostureDerivationPremiseHolds(t *testing.T) {
	root := moduleRoot(t)
	family, byMeasure := registrarsOfGreekMeasures(t, root)

	// NON-VACUITY, BEFORE ANYTHING IS JUDGED. A renamed catalogue map or a moved
	// compute tree leaves both of these empty, and an empty family satisfies
	// "RegisterGreeks registers all of them" perfectly while checking nothing.
	if len(family) < 5 {
		t.Fatalf("the catalogue maps %d measure constants to FamilyGreeks (%v) — expected at "+
			"least the five RegisterGreeks installs. The catalogue moved and this guard is "+
			"asserting nothing about what justifies kanz_risk_greek_model_live", len(family), family)
	}
	if len(byMeasure) == 0 {
		t.Fatalf("no `Register(Measure...)` call site for any Greek was found under "+
			"internal/risk/compute — the scan is broken, not the estate (family=%v)", family)
	}

	const registrar = "RegisterGreeks"
	var missing []string
	for _, measure := range family {
		fns := byMeasure[measure]
		found := false
		for _, fn := range fns {
			if fn == registrar {
				found = true
			}
		}
		if !found {
			missing = append(missing, measure+" registered by "+strings.Join(fns, ",")+
				" and not by "+registrar)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%s no longer installs the whole catalogued Greek family:\n  %s\n\n"+
			"kanz_risk_greek_model_live infers 'no Greek is model-served' from a single DARK "+
			"member of the family, and that inference is only sound because one call installs "+
			"all of them. Split across two registrars, a half-wired engine would report the "+
			"whole family unwired — or worse, report it wired while Delta still answers with "+
			"net exposure. Re-derive the posture in %s.",
			registrar, strings.Join(missing, "\n  "), greekPostureFile)
	}

	// AND NOTHING ELSE MAY REGISTER THE HIGHER-ORDER GREEKS. Delta is registered
	// by DefaultRegistry too — that is the placeholder, and it is the whole
	// subject. The members DefaultRegistry does NOT register are the ones whose
	// darkness proves RegisterGreeks did not run, so a second registrar for one of
	// those is what would break the inference.
	const defaultRegistrar = "DefaultRegistry"
	var extra []string
	for _, measure := range family {
		fns := byMeasure[measure]
		fromDefault := false
		for _, fn := range fns {
			if fn == defaultRegistrar {
				fromDefault = true
			}
		}
		if fromDefault {
			continue
		}
		for _, fn := range fns {
			if fn != registrar {
				extra = append(extra, measure+" is also registered by "+fn)
			}
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		t.Errorf("a second registrar exists for a Greek that %s does not register:\n  %s\n\n"+
			"The posture in %s reads 'this member is dark, therefore RegisterGreeks did not "+
			"run, therefore the Delta that DOES answer is the net-exposure placeholder'. "+
			"Another registrar makes that member's darkness stop proving anything, and the "+
			"posture would then report a placeholder Delta as model-served — a claim the "+
			"estate makes about itself, which is worse than the silence it replaced.",
			defaultRegistrar, strings.Join(extra, "\n  "), greekPostureFile)
	}
}

// TestDeltaStaysInTheDefaultRegistry pins the #1055 decision itself.
//
// Removing Delta was the alternative considered and rejected: the mandate would
// still refuse, but through the never-announced arm, which is the arm a
// risk-engine OUTAGE takes — and services/oms/cmd/oms/riskfold.go's whole
// argument is that those refusals must stay distinguishable because different
// people fix them. Both existing signals would go quiet with it
// (kanz_risk_measure_method stops reporting a placeholder,
// kanz_oms_risk_measures_placeholder_total stops counting), so the dashboards
// would improve while nothing about the platform's ability to measure delta
// changed.
//
// The decision is here as an assertion rather than only as a paragraph because it
// is also the PREMISE OF AN ALERT: RiskGreekServedByANetExposurePlaceholder reads
// kanz_risk_greek_model_live{measure="Delta"} == 0 as "Delta answers, and not from
// a model". Unregister Delta and that expression keeps firing while meaning
// something else entirely.
func TestDeltaStaysInTheDefaultRegistry(t *testing.T) {
	root := moduleRoot(t)
	family, byMeasure := registrarsOfGreekMeasures(t, root)
	if len(family) < 5 {
		t.Fatalf("catalogued Greek family is %v — the scan is broken and this guard is asserting "+
			"nothing", family)
	}

	for _, fn := range byMeasure["MeasureDelta"] {
		if fn == "DefaultRegistry" {
			return
		}
	}
	t.Fatalf("compute.DefaultRegistry no longer registers MeasureDelta (registrars: %v).\n\n"+
		"That was a considered decision and not an accident (#1055). Unregistered, a Delta "+
		"mandate still refuses — through the never-announced arm, which is the one a "+
		"risk-engine outage takes, so two incidents with different owners collapse into one "+
		"signal. kanz_risk_measure_method stops reporting a placeholder for it and "+
		"kanz_oms_risk_measures_placeholder_total stops counting it, so the estate's "+
		"dashboards improve while its ability to measure delta is unchanged. And "+
		"RiskGreekServedByANetExposurePlaceholder reads "+
		"kanz_risk_greek_model_live{measure=\"Delta\"} == 0 as \"it answers, and not from a "+
		"model\" — a claim that stops being true the moment it stops answering.\n\n"+
		"If this is being removed deliberately, retire the alert and this guard in the same "+
		"change, and say where the operator learns it instead.", byMeasure["MeasureDelta"])
}

// AND THE PROCESS MUST REGISTER THE POSTURE, BEFORE THE BROKER BRANCH.
//
// A collector registered inside `if cfg.NATSURL != ""` exports NO series on the
// deployment that takes the other path, so an `== 0` alert over it is silent in
// exactly the state it was written for. That has shipped twice in this estate
// (#973, #963) and #1050 fixed it once already in this very file for
// kanz_risk_recompute_*. The measure postures beside this one are still inside
// runEngine and still have that defect; this one must not join them.
//
// Asserted off the AST with comments excluded: main.go's own comment names the
// function, so a guard that grepped raw source would match the explanation and
// keep passing with the call deleted.
func TestRiskEngineCompositionRootRegistersTheGreekPostureUnconditionally(t *testing.T) {
	const ctor = "NewGreekModelPosture"
	path := filepath.Join(moduleRoot(t), riskEngineCompositionRoot)

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", riskEngineCompositionRoot, err)
	}

	var run *ast.FuncDecl
	for _, decl := range f.Decls {
		if d, ok := decl.(*ast.FuncDecl); ok && d.Name.Name == "run" && d.Recv == nil {
			run = d
		}
	}
	if run == nil || run.Body == nil {
		t.Fatalf("%s no longer declares run() — this guard found nothing to check. If the "+
			"composition root moved, move this assertion with it", riskEngineCompositionRoot)
	}

	// Anywhere in run, to prove the call exists at all; and at the top level of
	// run's body, to prove nothing guards it. Both arms, because "not nested" is
	// satisfied vacuously by "not present".
	if !callsFunc(run.Body, ctor) {
		t.Fatalf("run() in %s never calls %s().\n\n"+
			"Without it kanz_risk_greek_model_live is never registered, so every series is "+
			"ABSENT rather than zero — and an absent series answers an alert with \"no data\", "+
			"which is the same ambiguity moved out of the engine and into the monitoring "+
			"system. Delta then goes on answering with net exposure, and a mandate naming it "+
			"goes on refusing every order, with nothing anywhere saying so (#1055).",
			riskEngineCompositionRoot, ctor)
	}

	var target string
	for _, stmt := range run.Body.List {
		assign, ok := stmt.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 || len(assign.Lhs) != 1 {
			continue
		}
		if !callsFunc(assign.Rhs[0], ctor) {
			continue
		}
		if id, ok := assign.Lhs[0].(*ast.Ident); ok {
			target = id.Name
		}
	}
	if target == "" {
		t.Fatalf("run() in %s calls %s() from inside a nested statement rather than at the top "+
			"level of its body.\n\n"+
			"A collector registered behind a branch is exported by SOME deployments and not "+
			"others, and the ones that skip it report no series at all — so "+
			"`kanz_risk_greek_model_live{measure=\"Delta\"} == 0` matches nothing in precisely "+
			"the deployment whose Delta is a net-exposure placeholder. Registered absent is "+
			"worse than registered zero (#973, #963).",
			riskEngineCompositionRoot, ctor)
	}

	// AND THE THING IT BUILT MUST BE HANDED ON. A posture registered and never
	// stated seeds every Greek at 0 and never re-reads the registry, so it would
	// report an unwired family forever — including on the day somebody wires it.
	var passedOn bool
	ast.Inspect(run.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || callsFunc(call, ctor) {
			return true
		}
		for _, arg := range call.Args {
			if id, ok := arg.(*ast.Ident); ok && id.Name == target {
				passedOn = true
			}
		}
		return true
	})
	if !passedOn {
		t.Errorf("run() in %s builds %s with %s() and never passes it to anything.\n\n"+
			"The gauge would then hold its seeded zeroes for the life of the pod and never "+
			"read the final registry — so it would report the Greek family unwired on the day "+
			"RegisterGreeks is finally wired, which is the one day it has something new to say.",
			riskEngineCompositionRoot, target, ctor)
	}
}

// AND THE POSTURE MUST BE STATED FROM THE FINAL REGISTRY. The composition root
// registers it; something has to fill it in. Found by TYPE rather than by
// variable name: any function taking a *app.GreekModelPosture must call State on
// it, so renaming the parameter or moving the wiring to another file leaves this
// assertion working.
func TestTheGreekPostureIsStatedFromARegistry(t *testing.T) {
	dir := filepath.Join(moduleRoot(t), "services", "risk-engine", "cmd", "risk-engine")

	holders := 0
	stated := 0
	for _, path := range goFilesInTree(t, dir) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Type.Params == nil {
				continue
			}
			var name string
			for _, param := range fn.Type.Params.List {
				if bareTypeName(param.Type) != "GreekModelPosture" || len(param.Names) == 0 {
					continue
				}
				name = param.Names[0].Name
			}
			if name == "" {
				continue
			}
			holders++
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "State" {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == name {
					stated++
				}
				return true
			})
		}
	}

	if holders == 0 {
		t.Fatalf("no function under services/risk-engine/cmd/risk-engine takes a "+
			"*app.GreekModelPosture — the wiring moved and this guard is asserting nothing "+
			"about whether kanz_risk_greek_model_live ever sees the registry (%s)",
			greekPostureFile)
	}
	if stated == 0 {
		t.Errorf("the risk-engine composition root holds a *app.GreekModelPosture and never " +
			"calls State on it.\n\n" +
			"The gauge would keep its seeded zeroes forever: correct today by accident, and " +
			"wrong on the day RegisterGreeks is wired — reporting an unwired Greek family from " +
			"an engine that computes real Greeks, which is the inverse of the claim it exists " +
			"to make.")
	}
}

// AND THE GAUGE MAY NOT BE REGISTERED BEHIND A BRANCH inside the constructor
// either. Same argument as the call site above, one layer in: there is nothing to
// branch on — the posture is a fact about the build and its registry, not about
// the config — so the registration must sit at the top level of the function body.
func TestGreekModelGaugeIsRegisteredUnconditionally(t *testing.T) {
	const fn = "NewGreekModelPosture"
	path := filepath.Join(moduleRoot(t), greekPostureFile)

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", greekPostureFile, err)
	}

	var body *ast.BlockStmt
	for _, decl := range f.Decls {
		if d, ok := decl.(*ast.FuncDecl); ok && d.Name.Name == fn {
			body = d.Body
		}
	}
	if body == nil {
		t.Fatalf("%s no longer declares %s — this guard found nothing to check. If the seeding "+
			"moved, move this assertion with it", greekPostureFile, fn)
	}

	var anywhere bool
	ast.Inspect(body, func(n ast.Node) bool {
		if isMustRegisterCall(n) {
			anywhere = true
		}
		return true
	})
	if !anywhere {
		t.Fatalf("%s no longer calls MustRegister — the gauge is never registered and every "+
			"kanz_risk_greek_model_live series is absent", fn)
	}

	var topLevel bool
	for _, stmt := range body.List {
		expr, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}
		if isMustRegisterCall(expr.X) {
			topLevel = true
		}
	}
	if !topLevel {
		t.Fatalf("%s registers the Greek-model gauge from inside a nested statement rather than "+
			"at the top level of its body.\n\n"+
			"A collector registered behind a branch is exported by SOME deployments and not "+
			"others, and the ones that skip it report no series at all — so "+
			"`kanz_risk_greek_model_live{measure=\"Delta\"} == 0` matches nothing in precisely "+
			"the deployment it was written for.", fn)
	}
}

// bareTypeName returns the bare type name of a parameter, dropping any pointer
// and package qualifier — `*app.GreekModelPosture` yields `GreekModelPosture`.
func bareTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return bareTypeName(t.X)
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.Ident:
		return t.Name
	}
	return ""
}
