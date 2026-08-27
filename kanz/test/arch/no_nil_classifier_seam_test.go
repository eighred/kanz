package arch

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A CLASSIFIER SEAM NO COMPOSITION ROOT FILLS IS A COMPLIANCE CONTROL THAT
// CANNOT FIRE AND A STRESS TEST THAT RETURNS THE BOOK IT WAS ASKED TO SHOCK.
//
// # What went wrong without it
//
// There are two Classifier interfaces for instrument reference data —
// compliance.Classifier and factor.Classifier — and one implementation of each,
// both of them in-memory maps whose only constructors are _test.go files. Every
// production seam was nil or uncalled (#640):
//
//	services/oms/cmd/oms            NewPreTradeGate(..., nil, ...)   pre-trade gate
//	services/compliance/cmd/...     NewMonitor(..., nil, ...)        post-trade monitor
//	services/risk-engine/cmd/...    engine.WithClassifier UNCALLED   every scenario
//
// None of the three failed. A mandate naming the SECTOR dimension bucketed every
// holding under the empty key, so "no more than 10% in TECH" looked up a bucket
// that was not there, took the not-held branch, and ADMITTED a book that was
// entirely technology — recording the decision as a pass. A DENY list never
// matched. And `applySectorShock` returned at its nil check, so GFC_2008 and
// COVID_2020 came back through the live POST /v1/portfolios/{id}/scenario route
// reporting no impact.
//
// # Why the existing guards could not see it
//
// no_dark_capability_test.go works at IMPORT granularity, and both
// internal/compliance and internal/risk/compute/factor are imported constantly,
// so the packages are bright while the seams are dark. Its own doc names that
// blind spot. no_dark_measure_seam_test.go closes it for internal/risk/compute's
// registration seams by reading the PARAMETER TYPE — a *Registry or a *Providers
// bundle — and Classifier is neither, so `engine.WithClassifier` with zero
// callers in the whole module sat outside both nets.
//
// # What this checks
//
// Every exported function taking a parameter whose TYPE NAME is Classifier is a
// composition-root seam. Each one must resolve to one of three states, and two
// of them require a named exemption:
//
//	WIRED  a cross-package non-test caller passes something that is not `nil`
//	NIL    a cross-package non-test caller passes a literal nil        → exempt
//	DARK   no cross-package non-test caller at all                     → exempt
//
// TYPE, NOT NAME, for no_dark_measure_seam_test.go's reason: a rule of
// "parameters called classifier" breaks the day someone writes `c Classifier`,
// and a rule of "functions called With*" would miss NewPreTradeGate and
// NewMonitor, which are two of the three.
//
// # The known weakness, stated rather than discovered later
//
// This reads SYNTAX. `scenario.WithClassifier(e.classifier)` resolves as WIRED
// because the argument is not the identifier `nil` — and at runtime
// e.classifier IS nil, because engine.WithClassifier has no caller. The guard
// catches that one level up, where it is visible: engine.WithClassifier is DARK
// and carries its own exemption. A seam filled only from another dark seam is
// the same limitation the import-granularity guard has, one level finer, which
// is why the exemption text below is the real record.

// nilClassifierExempt maps a seam — or a specific (caller → seam) nil — to the
// reason it is not wired.
//
// EVERY ENTRY NAMES THE MISSING PIECE, not just an issue number, because "not
// wired yet" is what let this survive long enough for three controls to be built
// on top of it. Keys are "<callerPkg> -> <seamPkg>.<Func>" for a nil argument and
// "<seamPkg>.<Func>" for a seam with no caller at all.
var nilClassifierExempt = map[string]string{
	// THE THREE SEAMS THIS GUARD WAS BUILT FOR ARE GONE FROM THIS MAP (#640).
	//
	// services/oms -> compliance.NewPreTradeGate, services/compliance ->
	// monitor.NewMonitor and risk/engine.WithClassifier each carried an entry
	// here saying the same thing: no production Classifier existed to pass,
	// because the reference data lived in datamaster's golden_records with NO
	// ISSUER COLUMN AT ALL, a read endpoint that omitted sector, and no client in
	// any consuming service. All four of those are now built — the issuer runs
	// through RefRow -> VendorRecord -> SecurityMaster, handleSecurity serves
	// sector, issuer_id and as_of, and internal/refdata is the one cache both
	// Classifier interfaces project from. The dead-entry check below is what
	// removed them: it failed the moment the seams were wired, which is the
	// retirement mechanism working rather than somebody remembering.
	//
	// The two below were FOUND BY THIS GUARD when it first ran. None of them is
	// on the three paths #640 was filed about; all of them are the same absence
	// one door over, and they are recorded rather than quietly left out, because
	// an exemption list that covers only the seams somebody already knew about is
	// a list that will not catch the next one.
	//
	// TWO MORE LEFT BY BEING WIRED. optimization.Propose and .CheckMandate were
	// dark because services/optimization's handlePropose ran Optimize and
	// Rebalance inline and skipped the mandate check entirely, so every proposal
	// it returned was MandateUnchecked and /v1/orders refused to materialize it.
	// The service now resolves a mandate from a MandateRegistry fed by the same
	// lifecycle.v1.ConfigChanged FACTs the OMS consumes, and calls Propose. The
	// dead-entry check below removed both entries.
	//
	// THEY NAME #751, NOT #640. #640 is closed — its own three seams are wired
	// and proven against a running datamaster — and an exemption pointing at a
	// closed issue names nothing that will retire it, which is the one thing this
	// map may not do.
	//
	// THERE WERE FIVE. internal/risk/compute/factor.SectorExposure left by being
	// FIXED rather than exempted: it was never an unfilled seam, only one this
	// guard could not see, because its caller factor.ComputeExposure is in its own
	// package. The rule now resolves a same-package caller that is itself wired
	// (see 2b), so the seam reports what it always was, and the dead-entry check
	// below is what removed the entry.
	"internal/performance.BucketBySector": "#751 — Brinson sector attribution. TWO THINGS ARE " +
		"MISSING and only one is reference data. performance.Classifier has NO implementation " +
		"anywhere in the module, not even a StaticClassifier; and services/performance sidesteps " +
		"the question entirely — attributionRequest carries already-bucketed []perf.SectorData " +
		"straight off the wire and hands it to perf.SingleAttribution, so nothing ever converts " +
		"instrument-level returns into sector buckets. The client is the classifier. Retiring this " +
		"needs a request shape carrying instrument-level rows as well as a classifier to bucket " +
		"them with.",
}

// classifierSeam is one composition-root seam: an exported function taking a
// parameter whose type name is Classifier.
type classifierSeam struct {
	pkg   string // module-relative package dir
	name  string
	index int // flattened parameter index of the Classifier argument
}

func (s classifierSeam) String() string { return s.pkg + "." + s.name }

func TestNoClassifierSeamIsNilOrDarkAndUntracked(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	// ===== 1. Every Classifier seam in the module =====
	seams := map[classifierSeam]bool{}
	seamPkgs := map[string]bool{}
	walkGoFiles(t, root, ".", fset, func(rel string, f *ast.File) {
		pkgDir := filepath.ToSlash(filepath.Dir(rel))
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !fn.Name.IsExported() || fn.Type.Params == nil {
				continue
			}
			if idx, ok := classifierParamIndex(fn.Type.Params); ok {
				seams[classifierSeam{pkgDir, fn.Name.Name, idx}] = true
				seamPkgs[pkgDir] = true
			}
		}
	})

	// NON-VACUITY, first arm: the scan must find the seam family. A broken walk
	// or a broken parameter rule would report zero nil seams and pass having
	// checked nothing — which is the failure mode this whole file exists to catch
	// one level down, and the one #640 was.
	if len(seams) < 4 {
		t.Fatalf("found only %d Classifier seams in the module — the walk or the parameter rule "+
			"is broken, not the estate. compliance.NewPreTradeGate, monitor.NewMonitor, "+
			"risk/engine.WithClassifier and scenario.WithClassifier are all seams and must be "+
			"found: %v", len(seams), seamNames(seams))
	}

	// ===== 2. Resolve cross-package, non-test callers =====
	type callSite struct {
		callerPkg string
		seam      classifierSeam
		passedNil bool
	}
	var calls []callSite
	byName := map[string][]classifierSeam{} // "<pkgdir>.<Func>" → seams
	for s := range seams {
		byName[s.String()] = append(byName[s.String()], s)
	}

	walkGoFiles(t, root, ".", fset, func(rel string, f *ast.File) {
		callerPkg := filepath.ToSlash(filepath.Dir(rel))
		alias := map[string]string{}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			dir, ok := strings.CutPrefix(path, modulePath+"/")
			if !ok || !seamPkgs[dir] {
				continue
			}
			name := filepath.Base(dir)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			alias[name] = dir
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			// QUALIFIED ONLY, and only from a DIFFERENT package — the rule
			// no_dark_measure_seam_test.go settled on. A bare-name match would let
			// any foo.NewMonitor( in the module vouch for this one, and an
			// intra-package call proves nothing about a composition root.
			dir, ok := alias[id.Name]
			if !ok || dir == callerPkg {
				return true
			}
			for _, s := range byName[dir+"."+sel.Sel.Name] {
				calls = append(calls, callSite{
					callerPkg: callerPkg,
					seam:      s,
					passedNil: argIsNil(call, s.index),
				})
			}
			return true
		})
	})

	// ===== 2b. SAME-PACKAGE callers, one hop =====
	//
	// A seam called only from inside its own package used to resolve DARK, and
	// that was wrong for factor.SectorExposure: its caller factor.ComputeExposure
	// IS wired from services/risk-engine, so it runs on every served exposure
	// read while this guard reported it dark (#751).
	//
	// THE WIDENING IS ONE HOP AND CONDITIONAL, not "same-package callers count".
	// A bare "any intra-package caller vouches for it" rule would let a seam
	// called only by its own package's DEAD code resolve WIRED, which is the hole
	// the cross-package rule existed to close. So an intra-package call promotes
	// its callee only when the CALLER IS ITSELF A SEAM THAT RESOLVED WIRED —
	// anchoring the chain in a real composition root. A chain that ends nowhere
	// leaves every link dark, and the outermost link carries the exemption, which
	// is the "catch it one level up" model this guard already runs on.
	//
	// Same-package calls are BARE IDENTIFIERS, not selectors, which is why the
	// qualified walk above cannot see them at all.
	type intraCall struct {
		caller    classifierSeam // the enclosing function, itself a seam
		seam      classifierSeam
		passedNil bool
	}
	var intraCalls []intraCall
	walkGoFiles(t, root, ".", fset, func(rel string, f *ast.File) {
		pkgDir := filepath.ToSlash(filepath.Dir(rel))
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil || !fn.Name.IsExported() || fn.Type.Params == nil {
				continue
			}
			idx, ok := classifierParamIndex(fn.Type.Params)
			if !ok {
				continue // only a seam may vouch for another seam
			}
			caller := classifierSeam{pkgDir, fn.Name.Name, idx}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true // a selector is the qualified case handled above
				}
				for _, s := range byName[pkgDir+"."+id.Name] {
					if s == caller {
						continue // direct recursion vouches for nothing
					}
					intraCalls = append(intraCalls, intraCall{caller, s, argIsNil(call, s.index)})
				}
				return true
			})
		}
	})

	// NON-VACUITY, second arm: some seam must resolve to a caller. If the import
	// alias resolution breaks, no call site is seen at all — so no nil is seen
	// either, and the guard reports a clean estate while checking nothing.
	if len(calls) < 3 {
		t.Fatalf("only %d call(s) resolved to a Classifier seam — the import-alias resolution is "+
			"broken. The OMS gate, the compliance monitor and risk/engine's forward to "+
			"scenario.WithClassifier are all real cross-package calls and must resolve", len(calls))
	}

	// ===== 3. Default-deny =====
	var untracked []string
	seenExempt := map[string]bool{}
	called := map[classifierSeam]bool{}
	wired := 0

	// The cross-package pass first: it is the only thing that can anchor a chain.
	wiredSeam := map[classifierSeam]bool{}
	for _, c := range calls {
		if !c.passedNil {
			wiredSeam[c.seam] = true
		}
	}
	// Then propagate through same-package calls until nothing more resolves. A
	// fixpoint rather than a single sweep, so a chain of three holds together
	// regardless of the order the walk happened to produce.
	for changed := true; changed; {
		changed = false
		for _, ic := range intraCalls {
			if ic.passedNil || !wiredSeam[ic.caller] || wiredSeam[ic.seam] {
				continue
			}
			wiredSeam[ic.seam] = true
			changed = true
		}
	}
	for s := range wiredSeam {
		called[s] = true
	}

	for _, c := range calls {
		called[c.seam] = true
		if !c.passedNil {
			wired++
			continue
		}
		key := c.callerPkg + " -> " + c.seam.String()
		if reason, ok := nilClassifierExempt[key]; ok {
			seenExempt[key] = true
			t.Logf("%s: nil, tracked — %s", key, reason)
			continue
		}
		untracked = append(untracked, key+" (passes nil)")
	}
	for s := range seams {
		if called[s] {
			continue
		}
		key := s.String()
		if reason, ok := nilClassifierExempt[key]; ok {
			seenExempt[key] = true
			t.Logf("%s: dark, tracked — %s", key, reason)
			continue
		}
		untracked = append(untracked, key+" (no caller in any other package)")
	}

	// NON-VACUITY, third arm: at least one seam must resolve as genuinely WIRED.
	// Without it, an argIsNil that answered true for everything — or a caller
	// resolution that lost the argument list — would classify the whole estate as
	// nil, and the exemption list would grow to cover a bug in this file.
	if wired < 1 {
		t.Fatalf("no Classifier seam resolved to a non-nil argument out of %d call(s) — "+
			"argIsNil or the argument indexing is broken. risk/engine's "+
			"scenario.WithClassifier(e.classifier) passes a non-nil expression and must "+
			"resolve as wired", len(calls))
	}

	if len(untracked) > 0 {
		sort.Strings(untracked)
		t.Errorf("%d Classifier seam(s) are nil or dark with no exemption: %v.\n"+
			"A nil classifier does not fail — it makes SECTOR, ISSUER and ASSET_CLASS resolve to "+
			"the empty bucket, so a concentration cap on a sector PASSES regardless of the "+
			"holding, a deny list never matches, and every named stress scenario returns the "+
			"unshocked book (#640).\n"+
			"Wire a reference-data-backed Classifier at the composition root, or add an entry to "+
			"nilClassifierExempt NAMING THE ISSUE AND THE MISSING SOURCE — and make sure the "+
			"unresolvable case REFUSES rather than passing, because an exemption is a record that "+
			"the seam is empty, not permission for it to be silent.",
			len(untracked), untracked)
	}

	// DEAD-ENTRY ARM: an exemption for a seam that is now wired, or that no
	// longer exists under the parameter rule, is a claim about the estate that is
	// no longer true. It must not outlive its repair.
	for name, reason := range nilClassifierExempt {
		if seenExempt[name] {
			continue
		}
		t.Errorf("exemption for %q is stale — the seam is now wired, or no longer exists under "+
			"the parameter rule. Delete the entry (%s)", name, reason)
	}
}

// classifierParamIndex returns the FLATTENED index of the first parameter whose
// type name is Classifier. Flattened because `func F(a, b Classifier)` is one
// ast.Field with two names but two arguments at the call site, and indexing the
// field would point at the wrong argument.
func classifierParamIndex(params *ast.FieldList) (int, bool) {
	idx := 0
	for _, field := range params.List {
		n := len(field.Names)
		if n == 0 {
			n = 1 // unnamed parameter still occupies one position
		}
		if isClassifierType(field.Type) {
			return idx, true
		}
		idx += n
	}
	return 0, false
}

// isClassifierType reports whether an expression names a Classifier, seeing
// through a pointer and a package qualifier so compliance.Classifier,
// factor.Classifier and a bare Classifier are the same thing to this rule.
func isClassifierType(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.StarExpr:
		return isClassifierType(t.X)
	case *ast.SelectorExpr:
		return t.Sel.Name == "Classifier"
	case *ast.Ident:
		return t.Name == "Classifier"
	}
	return false
}

// argIsNil reports whether the argument at idx is the literal identifier `nil`.
//
// A CALL WITH TOO FEW ARGUMENTS IS NOT A NIL, it is a spread (f(g())) or a build
// this guard cannot index, and treating it as nil would put an unexplainable
// entry in the exemption list. Those show up as WIRED, which is the same
// direction of error the "seam filled from another dark seam" limitation has and
// is documented with it above.
func argIsNil(call *ast.CallExpr, idx int) bool {
	if idx >= len(call.Args) {
		return false
	}
	id, ok := call.Args[idx].(*ast.Ident)
	return ok && id.Name == "nil"
}

func seamNames(seams map[classifierSeam]bool) []string {
	out := make([]string, 0, len(seams))
	for s := range seams {
		out = append(out, s.String())
	}
	sort.Strings(out)
	return out
}
