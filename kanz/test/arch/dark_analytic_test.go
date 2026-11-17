package arch

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// #1042's vector/method siblings do not take Registry/Providers, so they are
// outside no_dark_measure_seam. Derive exported analytics from these two owned
// source families, including future additions. Tests are not production callers.
// Method calls count only on an explicitly typed Model parameter AND the
// reviewed canonical entry chain below. An arbitrary non-test/parked helper is
// not a live root. This is a bounded recipe, not whole-program reachability:
// admitting another entry requires its owning chain and behavioral proof here.
var analyticFamilies = map[string]string{
	"internal/risk/factormodel/decompose.go": "internal/risk/factormodel",
	"internal/risk/pricing/deltagamma.go":    "internal/risk/pricing",
}

var darkAnalyticExempt = map[string]string{
	"internal/risk/factormodel.Model.TrackingError": "#1028: no admitted dated benchmark projection/surface. Retire when a production caller uses independently owned benchmark inputs; a caller in the parked performance service is not deployment evidence.",
	"internal/risk/pricing.DeltaGammaVaR":           "#1230/#1232: pricing-derived live Greeks and the full scenario caller/input contract are not admitted. Retire with the actual caller, not a decorative constructor.",
	"internal/risk/pricing.DeltaGammaPnL":           "#1230/#1232: currently called only by the equally dark DeltaGammaVaR; no independently reachable production consumer. Retire when that caller chain is wired.",
}

func TestNoDarkAnalyticIsUntracked(t *testing.T) {
	files := analyticSourceFiles(t)
	if problems := darkAnalyticProblems(files, darkAnalyticExempt); len(problems) != 0 {
		t.Fatal(strings.Join(problems, "\n"))
	}
}

func analyticSourceFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	files := map[string]*ast.File{}
	walkGoFiles(t, moduleRoot(t), ".", token.NewFileSet(), func(path string, file *ast.File) { files[path] = file })
	return files
}

func analyticDecl(pkg string, fn *ast.FuncDecl) string {
	if fn.Recv == nil {
		return pkg + "." + fn.Name.Name
	}
	if len(fn.Recv.List) != 1 {
		return ""
	}
	typ := fn.Recv.List[0].Type
	if ptr, ok := typ.(*ast.StarExpr); ok {
		typ = ptr.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return pkg + "." + id.Name + "." + fn.Name.Name
	}
	return ""
}

func analyticImports(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		rel, ok := strings.CutPrefix(path, modulePath+"/")
		if !ok {
			continue
		}
		alias := filepath.Base(rel)
		if imp.Name != nil {
			alias = imp.Name.Name
		}
		out[alias] = rel
	}
	return out
}

func darkAnalyticProblems(files map[string]*ast.File, exemptions map[string]string) []string {
	declared := map[string]bool{}
	for path, pkg := range analyticFamilies {
		if f := files[path]; f != nil {
			for _, d := range f.Decls {
				if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.IsExported() {
					declared[analyticDecl(pkg, fn)] = true
				}
			}
		}
	}
	var problems []string
	for _, anchor := range []string{"internal/risk/factormodel.Model.Decompose", "internal/risk/factormodel.Model.TrackingError", "internal/risk/pricing.DeltaGammaPnL", "internal/risk/pricing.DeltaGammaVaR"} {
		if !declared[anchor] {
			problems = append(problems, "missing analytic declaration: "+anchor)
		}
	}
	live := map[string]bool{}
	edges := map[string][]string{}
	for path, file := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		pkg := filepath.ToSlash(filepath.Dir(path))
		imports := analyticImports(file)
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			caller := analyticDecl(pkg, fn)
			modelParams := map[string]bool{}
			if fn.Type.Params != nil {
				for _, param := range fn.Type.Params.List {
					ptr, ok := param.Type.(*ast.StarExpr)
					if !ok {
						continue
					}
					sel, ok := ptr.X.(*ast.SelectorExpr)
					if !ok {
						continue
					}
					alias, ok := sel.X.(*ast.Ident)
					if ok && imports[alias.Name] == "internal/risk/factormodel" && sel.Sel.Name == "Model" {
						for _, name := range param.Names {
							modelParams[name.Name] = true
						}
					}
				}
			}
			// Refuse ambiguous shadowing/reassignment rather than crediting a
			// same-named, untyped receiver. Compiled type checking is independent.
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if assign, ok := n.(*ast.AssignStmt); ok {
					for _, lhs := range assign.Lhs {
						if id, ok := lhs.(*ast.Ident); ok {
							delete(modelParams, id.Name)
						}
					}
				}
				if spec, ok := n.(*ast.ValueSpec); ok {
					for _, id := range spec.Names {
						delete(modelParams, id.Name)
					}
				}
				return true
			})
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				target := ""
				switch callee := call.Fun.(type) {
				case *ast.Ident:
					target = pkg + "." + callee.Name
				case *ast.SelectorExpr:
					if receiver, ok := callee.X.(*ast.Ident); ok {
						if modelParams[receiver.Name] {
							target = "internal/risk/factormodel.Model." + callee.Sel.Name
						} else if p := imports[receiver.Name]; p != "" {
							target = p + "." + callee.Sel.Name
						}
					}
				}
				if declared[target] {
					if declared[caller] {
						edges[caller] = append(edges[caller], target)
					} else if qualifiedAnalyticCaller(path, fn.Name.Name, target) {
						live[target] = true
					} else {
						problems = append(problems, "unreviewed analytic caller "+path+":"+fn.Name.Name+" -> "+target+" needs qualified root")
					}
				}
				return true
			})
		}
	}
	// The explicit Decompose root earns liveness only with its complete reviewed
	// query/provider chain. Merely leaving the typed helper behind is insufficient.
	if live["internal/risk/factormodel.Model.Decompose"] && len(decompositionWiringProblems(files)) != 0 {
		delete(live, "internal/risk/factormodel.Model.Decompose")
	}
	for changed := true; changed; {
		changed = false
		for caller, callees := range edges {
			if !live[caller] {
				continue
			}
			for _, target := range callees {
				if !live[target] {
					live[target], changed = true, true
				}
			}
		}
	}
	for name := range declared {
		reason, exempt := exemptions[name]
		if !live[name] && (!exempt || strings.TrimSpace(reason) == "") {
			problems = append(problems, "dark analytic without owner: "+name)
		}
	}
	for name, reason := range exemptions {
		if !declared[name] || live[name] || strings.TrimSpace(reason) == "" {
			problems = append(problems, "stale/unowned analytic exemption: "+name)
		}
	}
	sort.Strings(problems)
	return problems
}

func qualifiedAnalyticCaller(path, function, target string) bool {
	return target == "internal/risk/factormodel.Model.Decompose" && path == "internal/risk/compute/decomposition.go" && function == "decomposeModel"
}

func TestDarkAnalyticGuardMutatesActualSources(t *testing.T) {
	base := analyticSourceFiles(t)
	const owner = "internal/risk/compute/decomposition.go"
	b, err := os.ReadFile(filepath.Join(moduleRoot(t), filepath.FromSlash(owner)))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, before, after string }{
		{"remove real call", "d := model.Decompose(values)", "d := factormodel.Decomposition{}"},
		{"wrong receiver type", "model *factormodel.Model", "model *other.Model"},
		{"wrong import", `"github.com/eighred/kanz/internal/risk/factormodel"`, `"example.invalid/unrelated/factormodel"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if bytes.Count(b, []byte(tc.before)) != 1 {
				t.Fatal("mutation no longer matches exact owning source")
			}
			mutant := strings.Replace(string(b), tc.before, tc.after, 1)
			files := cloneAnalyticFiles(base)
			files[owner], err = parser.ParseFile(token.NewFileSet(), owner, mutant, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(darkAnalyticProblems(files, darkAnalyticExempt), "\n"), "dark analytic without owner: internal/risk/factormodel.Model.Decompose") {
				t.Fatal("mutated real analytic call was not caught")
			}
		})
	}
	files := cloneAnalyticFiles(base)
	delete(files, owner)
	files["internal/risk/compute/decomposition_test.go"] = base[owner]
	if !strings.Contains(strings.Join(darkAnalyticProblems(files, darkAnalyticExempt), "\n"), "dark analytic without owner: internal/risk/factormodel.Model.Decompose") {
		t.Fatal("test-only call certified a production analytic")
	}
	files = cloneAnalyticFiles(base)
	const family = "internal/risk/factormodel/decompose.go"
	decl, err := os.ReadFile(filepath.Join(moduleRoot(t), filepath.FromSlash(family)))
	if err != nil {
		t.Fatal(err)
	}
	files[family], err = parser.ParseFile(token.NewFileSet(), family, string(decl)+"\nfunc NewUntrackedAnalytic() {}\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(darkAnalyticProblems(files, darkAnalyticExempt), "\n"), "NewUntrackedAnalytic") {
		t.Fatal("new family entry escaped discovery")
	}
	exempt := map[string]string{}
	for key, reason := range darkAnalyticExempt {
		exempt[key] = reason
	}
	exempt["internal/risk/factormodel.Model.Decompose"] = "stale"
	exempt["internal/risk/factormodel.Model.Removed"] = "gone"
	problems := strings.Join(darkAnalyticProblems(base, exempt), "\n")
	for _, want := range []string{"stale/unowned analytic exemption: internal/risk/factormodel.Model.Decompose", "stale/unowned analytic exemption: internal/risk/factormodel.Model.Removed"} {
		if !strings.Contains(problems, want) {
			t.Fatal("dead exemption accepted: " + want)
		}
	}
	// Import aliasing is a positive control; lexical spelling is not identity.
	alias := strings.ReplaceAll(string(b), "factormodel.", "fm.")
	alias = strings.Replace(alias, `"github.com/eighred/kanz/internal/risk/factormodel"`, `fm "github.com/eighred/kanz/internal/risk/factormodel"`, 1)
	files = cloneAnalyticFiles(base)
	files[owner], err = parser.ParseFile(token.NewFileSet(), owner, alias, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p := darkAnalyticProblems(files, darkAnalyticExempt); len(p) != 0 {
		t.Fatalf("resolved alias refused: %v", p)
	}
	// A valid typed call in a parked service or an unreachable module helper
	// cannot retire TrackingError's missing admitted benchmark/surface owner.
	for _, path := range []string{"services/performance/internal/parked.go", "internal/risk/compute/dead_helper.go"} {
		files := cloneAnalyticFiles(base)
		files[path], err = parser.ParseFile(token.NewFileSet(), path, `package fixture
import fm "github.com/eighred/kanz/internal/risk/factormodel"
func unused(m *fm.Model) float64 { return m.TrackingError(nil, nil) }
`, 0)
		if err != nil {
			t.Fatal(err)
		}
		problems := strings.Join(darkAnalyticProblems(files, darkAnalyticExempt), "\n")
		if !strings.Contains(problems, "unreviewed analytic caller "+path+":unused -> internal/risk/factormodel.Model.TrackingError needs qualified root") || strings.Contains(problems, "stale/unowned analytic exemption: internal/risk/factormodel.Model.TrackingError") {
			t.Fatalf("new caller was silently ignored or retired a required exemption: %v", problems)
		}
		exempt := map[string]string{}
		for key, reason := range darkAnalyticExempt {
			if key != "internal/risk/factormodel.Model.TrackingError" {
				exempt[key] = reason
			}
		}
		if !strings.Contains(strings.Join(darkAnalyticProblems(files, exempt), "\n"), "dark analytic without owner: internal/risk/factormodel.Model.TrackingError") {
			t.Fatal("decorative caller replaced benchmark ownership")
		}
	}
}

func cloneAnalyticFiles(in map[string]*ast.File) map[string]*ast.File {
	out := make(map[string]*ast.File, len(in))
	for path, file := range in {
		out[path] = file
	}
	return out
}

// This recipe checks actual CallExpr/assignment nodes, not comments or a helper
// tested in isolation. It preserves both scalar and vector replay wrappers over
// the SAME fitted provider, plus the real Engine -> compute -> model path.
func TestDecompositionUsesOneLiveProvider(t *testing.T) {
	files := analyticSourceFiles(t)
	if p := decompositionWiringProblems(files); len(p) != 0 {
		t.Fatal(strings.Join(p, "\n"))
	}
	for _, tc := range []struct{ path, before, after string }{
		{"services/risk_engine/cmd/risk_engine/main.go", "factorModel = modelProvider", "factorModel = nil"},
		{"services/risk_engine/cmd/risk_engine/main.go", "engine.WithFactorModel(replay.Models{Source: factorModel})", "engine.WithFactorModel(nil)"},
		{"services/risk_engine/cmd/risk_engine/main.go", "Model: replay.Models{Source: modelProvider}", "Model: replay.Models{}"},
		{"internal/risk/engine/decomposition.go", "compute.DecomposePortfolio(ctx, p, e.factorModel)", "compute.DecomposePortfolio(ctx, p, nil)"},
		{"internal/risk/compute/decomposition.go", "decomposeModel(model, values)", "decomposeModel(nil, values)"},
	} {
		b, err := os.ReadFile(filepath.Join(moduleRoot(t), filepath.FromSlash(tc.path)))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), tc.before) {
			t.Fatal("wiring mutation no longer reaches owner: " + tc.before)
		}
		mutant := strings.Replace(string(b), tc.before, tc.after, 1)
		f, err := parser.ParseFile(token.NewFileSet(), tc.path, mutant, 0)
		if err != nil {
			t.Fatal(err)
		}
		copy := cloneAnalyticFiles(files)
		copy[tc.path] = f
		if len(decompositionWiringProblems(copy)) == 0 {
			t.Fatal("broken production wiring escaped: " + tc.before)
		}
		if !strings.Contains(strings.Join(darkAnalyticProblems(copy, darkAnalyticExempt), "\n"), "dark analytic without owner: internal/risk/factormodel.Model.Decompose") {
			t.Fatal("unreachable typed helper still certified a live analytic: " + tc.before)
		}
	}
}

func decompositionWiringProblems(files map[string]*ast.File) []string {
	var problems []string
	required := map[string]map[string][]string{
		"services/risk_engine/cmd/risk_engine/main.go":           {"runEngine": {"newFactorModelProvider(universe, provider, artifacts)", "engine.WithFactorModel(replay.Models{Source: factorModel})", "replay.Models{Source: modelProvider}", "factorModel = modelProvider"}},
		"services/risk_engine/cmd/risk_engine/factor_model.go":   {"newFactorModelProvider": {"compute.NewLiveModelProvider(factormodel.Config{Type: factormodel.Statistical}, universe, factormodel.Providers{Returns: provider}, artifacts.FitOptions()...)"}},
		"internal/risk/engine/decomposition.go":                  {"DecomposeRisk": {"e.refuseIfNotOwned(req.PortfolioID)", "e.store.Snapshot(req.PortfolioID)", "compute.DecomposePortfolio(ctx, p, e.factorModel)"}},
		"internal/risk/compute/decomposition.go":                 {"DecomposePortfolio": {"provider.Model(ctx, p.AsOf())", "decomposeModel(model, values)"}},
		"services/risk_engine/internal/grpcsrv/decomposition.go": {"DecomposeRisk": {"s.engine.DecomposeRisk(ctx, v1.DecomposeRiskRequest{PortfolioID: v1.PortfolioID(req.GetPortfolioId()), AsOf: asOf})"}},
	}
	for path, functions := range required {
		file := files[path]
		for name, expressions := range functions {
			found := map[string]bool{}
			if file != nil {
				for _, d := range file.Decls {
					fn, ok := d.(*ast.FuncDecl)
					if !ok || fn.Name.Name != name || fn.Body == nil {
						continue
					}
					ast.Inspect(fn.Body, func(n ast.Node) bool {
						switch n.(type) {
						case *ast.CallExpr, *ast.CompositeLit, *ast.AssignStmt:
							var b bytes.Buffer
							if format.Node(&b, token.NewFileSet(), n) == nil {
								found[b.String()] = true
							}
						}
						return true
					})
				}
			}
			for _, expr := range expressions {
				if !found[expr] {
					problems = append(problems, path+":"+name+" missing actual wiring "+expr)
				}
			}
		}
	}
	sort.Strings(problems)
	return problems
}
