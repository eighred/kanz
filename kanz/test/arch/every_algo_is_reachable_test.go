package arch

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// EVERY EXECUTION ALGORITHM IS REACHABLE FROM THE REGISTRY, AND NOTHING REACHES
// ONE AROUND IT (#868).
//
// # The failure this exists to catch
//
// Before #868, TWAP was a free function and services/oms/internal/schedule called
// it by name. The seam that replaced it — an Algo interface, algo.Registered() as
// the one list, algo.Lookup as the one resolution — is only worth anything if two
// properties hold, and NEITHER is enforced by the compiler:
//
//  1. An algorithm that is written is REACHABLE. A type implementing Algo but
//     missing from Registered() compiles, passes its own unit tests, and can never
//     be selected by any order. Nothing fails. The author sees green and ships an
//     algorithm the platform cannot use — and, worse, an order naming it is
//     refused in production with "this build implements [TWAP]" while the code for
//     it is sitting right there.
//
//  2. Nothing REACHES AN ALGORITHM AROUND THE REGISTRY. A caller writing
//     algo.TWAP(plan) again has re-hardwired the decision the seam removed: that
//     call ignores the algorithm the ORDER names, so a VWAP parent would be worked
//     as TWAP and every fill attributed to an algorithm that never ran. It also
//     compiles, and the schedule it produces is perfectly valid — which is exactly
//     why no test catches it.
//
// # How the list is DERIVED
//
// The implementation set is not written here. It is computed from the source: the
// Algo interface's method set is read out of the AST, every type in the package is
// matched against it by SIGNATURE (parameter and result TYPES, names ignored), and
// what matches is what must appear in Registered(). So an algorithm this guard has
// never seen is caught the moment it is written, which a hand-listed guard could
// not do — a hand list is the same drift one layer up.
//
// Comments are detached (parser.ParseFile with mode 0) because a guard that greps
// source matches its own prose: this file's paragraphs are full of "algo.TWAP",
// and so are the package's, and a textual check would fire on them and pass on a
// real caller written a line differently.
//
// # What it cannot check
//
// That a registered algorithm is CORRECT, that Lookup's answer is the one the
// order meant, or that the wire enum maps to a name the registry knows. The first
// two are internal/execution/algo's own tests; the third is
// services/oms/internal/order's TestScheduleAlgo_TheWireEnumIsResolvedByDerivationNotASwitch,
// which is where the enum lives.

const (
	// algoPkg is the tree that owns the seam. Module-relative, forward slashes.
	algoPkg = "internal/execution/algo"
	// algoIface is the interface an execution algorithm satisfies.
	algoIface = "Algo"
	// algoRegistry is the function that must construct every one of them.
	algoRegistry = "Registered"
	// algoLookup is the resolution, which must read the registry rather than
	// repeat it.
	algoLookup = "Lookup"
	// algoPlanner is the closed-form TWAP function. It may be called from inside
	// the package (it IS the registered implementation's body) and from tests
	// (which assert the arithmetic directly); a call from anywhere else is the
	// order path bypassing the seam.
	algoPlanner = "TWAP"
	// algoImport is the import path a caller reaching around the seam would carry.
	algoImport = "github.com/eighred/kanz/internal/execution/algo"
)

// algoUnregisteredExempt maps a type in internal/execution/algo to an argued
// reason it may implement Algo without being registered, and the issue that
// retires the entry.
//
// EMPTY, AND THAT IS THE POINT. An entry here is a decision that this build
// contains an execution algorithm no order can ever select, which is either dead
// code or a half-finished feature that looks finished.
var algoUnregisteredExempt = map[string]string{}

// algoDirectCallExempt maps a module-relative file to an argued reason it may
// call the closed-form planner directly instead of going through the registry.
//
// ALSO EMPTY. Every entry here is a call site where the algorithm the ORDER names
// is ignored, and the schedule it produces is indistinguishable from a correct
// one — so the cost of an entry is an unattributable execution, not a crash.
var algoDirectCallExempt = map[string]string{}

func TestEveryExecutionAlgoIsReachableFromTheRegistry(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, filepath.FromSlash(algoPkg))

	fset := token.NewFileSet()
	var (
		ifaceMethods map[string]string // method name -> signature by type
		// methods by receiver type name -> method name -> signature
		byReceiver  = map[string]map[string]string{}
		registered  = map[string]bool{}
		sawRegistry bool
		sawLookup   bool
		lookupReads bool
		nonTest     int
	)

	for _, f := range goFilesUnder(t, dir) {
		if strings.HasSuffix(f.rel, "_test.go") {
			// A TEST MAY DEFINE AN UNREGISTERED ALGORITHM. That is how the seam's
			// own tests prove an algorithm can refuse on UNKNOWN without shipping
			// one, and a test type is not something an order can name.
			continue
		}
		nonTest++
		file, err := parser.ParseFile(fset, filepath.Join(dir, filepath.FromSlash(f.rel)), f.body, 0)
		if err != nil {
			t.Fatalf("parse %s/%s: %v", algoPkg, f.rel, err)
		}

		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				if d.Tok != token.TYPE {
					continue
				}
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok || ts.Name.Name != algoIface {
						continue
					}
					it, ok := ts.Type.(*ast.InterfaceType)
					if !ok {
						continue
					}
					ifaceMethods = map[string]string{}
					for _, m := range it.Methods.List {
						ft, ok := m.Type.(*ast.FuncType)
						if !ok || len(m.Names) == 0 {
							continue // an embedded interface; not a method this guard can match
						}
						ifaceMethods[m.Names[0].Name] = algoFuncSig(fset, ft)
					}
				}

			case *ast.FuncDecl:
				if d.Recv != nil {
					recv := algoReceiverType(d.Recv)
					if recv == "" {
						continue
					}
					if byReceiver[recv] == nil {
						byReceiver[recv] = map[string]string{}
					}
					byReceiver[recv][d.Name.Name] = algoFuncSig(fset, d.Type)
					continue
				}
				switch d.Name.Name {
				case algoRegistry:
					sawRegistry = true
					// EVERY TYPE CONSTRUCTED IN THE REGISTRY'S BODY, however it is
					// written: T{}, &T{}, or T(...) as a conversion.
					ast.Inspect(d.Body, func(n ast.Node) bool {
						switch v := n.(type) {
						case *ast.CompositeLit:
							if id, ok := v.Type.(*ast.Ident); ok {
								registered[id.Name] = true
							}
						case *ast.CallExpr:
							if id, ok := v.Fun.(*ast.Ident); ok {
								registered[id.Name] = true
							}
						}
						return true
					})
				case algoLookup:
					sawLookup = true
					// LOOKUP MUST READ THE REGISTRY, not carry a second list. A
					// switch here would resolve names the registry does not publish
					// (and refuse ones it does), and the two would drift apart
					// silently because both compile.
					ast.Inspect(d.Body, func(n ast.Node) bool {
						if call, ok := n.(*ast.CallExpr); ok {
							if id, ok := call.Fun.(*ast.Ident); ok && id.Name == algoRegistry {
								lookupReads = true
							}
						}
						return true
					})
				}
			}
		}
	}

	// ===== NON-VACUITY. Every one of these is a way this guard passes while
	// checking nothing, and each has to fail loudly rather than skip. =====
	if nonTest == 0 {
		t.Fatalf("no non-test Go files under %s — the package moved and this guard is protecting "+
			"an empty directory", algoPkg)
	}
	if ifaceMethods == nil {
		t.Fatalf("no %s interface found in %s — the seam was renamed or removed, and every "+
			"assertion below would pass by matching nothing", algoIface, algoPkg)
	}
	if len(ifaceMethods) == 0 {
		t.Fatalf("%s.%s declares no methods — EVERY type in the package satisfies it vacuously, "+
			"so this guard would demand they all be registered or (equivalently) prove nothing",
			algoPkg, algoIface)
	}
	if !sawRegistry {
		t.Fatalf("no %s() found in %s — there is no registry to be reachable from", algoRegistry, algoPkg)
	}
	if !sawLookup {
		t.Fatalf("no %s() found in %s — nothing resolves a name to an algorithm", algoLookup, algoPkg)
	}
	if len(registered) == 0 {
		t.Fatalf("%s() constructs nothing — this build can work no order at all", algoRegistry)
	}
	if !lookupReads {
		t.Errorf("%s() does not call %s() — the resolution has become a second list, and the two "+
			"will drift: an algorithm added to one and not the other either cannot be selected or "+
			"is selected while unregistered, and both compile", algoLookup, algoRegistry)
	}

	// ===== (1) EVERY IMPLEMENTATION IS REGISTERED =====
	var (
		impls      []string
		offenders  []string
		seenExempt = map[string]bool{}
	)
	for recv, methods := range byReceiver {
		if recv == algoIface || !algoSatisfies(methods, ifaceMethods) {
			continue
		}
		impls = append(impls, recv)
		if registered[recv] {
			continue
		}
		if reason, ok := algoUnregisteredExempt[recv]; ok {
			seenExempt[recv] = true
			t.Logf("%s: exempt — %s", recv, reason)
			continue
		}
		offenders = append(offenders, recv)
	}
	if len(impls) == 0 {
		t.Fatalf("no type in %s implements %s — the interface exists and nothing satisfies it, so "+
			"the reachability check below is comparing two empty sets", algoPkg, algoIface)
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("%d execution algorithm(s) in %s implement %s and are not constructed in %s(): %s\n\n"+
			"An unregistered algorithm compiles, passes its own tests, and can never be selected by "+
			"any order — Lookup walks %s() comparing Name(), so what is not there does not exist. "+
			"The symptom in production is an order refused with \"this build implements [...]\" while "+
			"the code for the algorithm it named is in the binary.\n"+
			"Add it to %s(), or add an argued entry to algoUnregisteredExempt.",
			len(offenders), algoPkg, algoIface, algoRegistry, strings.Join(offenders, ", "),
			algoRegistry, algoRegistry)
	}
	for name, reason := range algoUnregisteredExempt {
		if !seenExempt[name] {
			t.Errorf("exemption for %q (%s) matches nothing — the type was registered, renamed or "+
				"deleted; remove the entry so it cannot silently license the next one", name, reason)
		}
	}

	// ===== (2) NOTHING REACHES AN ALGORITHM AROUND THE REGISTRY =====
	algoDirectCallers(t, root, fset)
}

// algoDirectCallers fails on any file outside internal/execution/algo that calls
// the closed-form planner directly instead of resolving the order's own algorithm
// through the registry.
func algoDirectCallers(t *testing.T, root string, fset *token.FileSet) {
	t.Helper()

	var (
		offenders  []string
		seenExempt = map[string]bool{}
		sawSeamUse bool
	)

	for _, f := range goFilesUnder(t, root) {
		// The package itself is where the planner LIVES, and a test may call it
		// directly to assert the arithmetic closed-form — that is the oracle the
		// seam was measured against.
		if strings.HasPrefix(f.rel, algoPkg+"/") || strings.HasSuffix(f.rel, "_test.go") {
			continue
		}
		if !strings.Contains(f.body, algoImport) {
			continue // cheap pre-filter; the AST below is what decides
		}
		file, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(f.rel)), f.body, 0)
		if err != nil {
			continue // not a buildable Go file in this configuration; other guards own that
		}

		local := ""
		for _, imp := range file.Imports {
			if strings.Trim(imp.Path.Value, `"`) != algoImport {
				continue
			}
			local = "algo"
			if imp.Name != nil {
				local = imp.Name.Name
			}
		}
		if local == "" || local == "_" {
			continue
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != local {
				return true
			}
			switch sel.Sel.Name {
			case algoRegistry, algoLookup, "Run":
				// NON-VACUITY, the subject half: at least one caller must actually
				// be going THROUGH the seam, or this arm is scanning a codebase
				// where nothing schedules anything and passes by finding nothing.
				sawSeamUse = true
			case algoPlanner:
				if reason, ok := algoDirectCallExempt[f.rel]; ok {
					seenExempt[f.rel] = true
					t.Logf("%s: exempt — %s", f.rel, reason)
					return true
				}
				offenders = append(offenders, f.rel)
			}
			return true
		})
	}

	if !sawSeamUse {
		t.Fatalf("no file outside %s resolves an algorithm through %s(), %s() or Run() — the seam "+
			"is wired to nothing, and this arm would pass on a codebase that had removed it entirely",
			algoPkg, algoRegistry, algoLookup)
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("%d file(s) call %s.%s directly instead of resolving the order's own algorithm: %s\n\n"+
			"That call ignores the algorithm the ORDER names, so a parent asking for anything else "+
			"is worked as TWAP and every fill is attributed to an algorithm that never ran. It "+
			"compiles, and the schedule it produces is a perfectly valid TWAP schedule — which is "+
			"why no behavioural test catches it.\n"+
			"Use %s.Run(plan, state, market), which resolves plan.Algo through the registry and "+
			"REFUSES a name this build does not implement.",
			len(offenders), "algo", algoPlanner, strings.Join(offenders, ", "), "algo")
	}
	for rel, reason := range algoDirectCallExempt {
		if !seenExempt[rel] {
			t.Errorf("exemption for %q (%s) matches nothing — the call was removed or the file "+
				"moved; remove the entry", rel, reason)
		}
	}
}

// algoSatisfies reports whether a type's methods cover every method of the
// interface, matched by name AND signature.
//
// BY SIGNATURE, NOT BY NAME ALONE: a type with a Schedule method taking different
// arguments does not implement Algo, and treating it as one would report a
// spurious violation that an author would "fix" by registering something that
// cannot compile.
func algoSatisfies(have, want map[string]string) bool {
	for name, sig := range want {
		if have[name] != sig {
			return false
		}
	}
	return true
}

// algoFuncSig renders a function's parameter and result TYPES, with parameter
// names discarded.
//
// NAMES ARE DISCARDED BECAUSE THEY DIFFER LEGITIMATELY: the interface declares
// Schedule(p Plan, st ParentState, mkt MarketView) and an implementation that
// ignores two of them writes Schedule(p Plan, _ ParentState, _ MarketView).
// Comparing rendered declarations would call those two different methods and this
// guard would report every implementation as unregistered.
func algoFuncSig(fset *token.FileSet, ft *ast.FuncType) string {
	render := func(fl *ast.FieldList) string {
		if fl == nil {
			return ""
		}
		var parts []string
		for _, f := range fl.List {
			var b strings.Builder
			if err := printer.Fprint(&b, fset, f.Type); err != nil {
				b.WriteString("<unprintable>")
			}
			// One entry per NAME, so (a, b Plan) and (a Plan, b Plan) render the
			// same and an unnamed field still counts once.
			n := len(f.Names)
			if n == 0 {
				n = 1
			}
			for range n {
				parts = append(parts, b.String())
			}
		}
		return strings.Join(parts, ",")
	}
	return "(" + render(ft.Params) + ")(" + render(ft.Results) + ")"
}

// algoReceiverType is the bare type name a method is declared on, whether the
// receiver is a value, a pointer, named or unnamed.
func algoReceiverType(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	expr := recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	// A generic receiver arrives as Type[T]; the name is still the base.
	if idx, ok := expr.(*ast.IndexExpr); ok {
		expr = idx.X
	}
	if id, ok := expr.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}
