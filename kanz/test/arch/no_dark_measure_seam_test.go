package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A REGISTRATION SEAM NOBODY CALLS IS A MEASURE THIS PLATFORM DOES NOT SERVE.
//
// no_dark_capability_test.go catches a package nothing imports. It names its own
// blind spot in its own doc — it works at IMPORT granularity — and this guard
// exists because that blind spot bit (#509).
//
// internal/risk/compute is imported constantly: for DefaultRegistry, for the
// measure-name constants, for MeasureFunc. The package is bright. ELEVEN OF ITS
// SEAMS WERE DARK, and the import guard could not see it:
//
//	RegisterFactorRisk    FactorVaR99, SystematicRisk, SpecificRisk    WIRED 2026-08-16
//	                      (#509). Its exemption said NewLiveModelProvider was
//	                      "blocked on a returns/characteristics source" — wrong on
//	                      both counts by then: the returns provider was already
//	                      constructed at the composition root and characteristics
//	                      are optional for a PCA fit. The real blocker was the
//	                      estimation UNIVERSE, which nothing named, and which the
//	                      live state store could already answer.
//	RegisterFIRisk        DV01, Duration, Convexity, SpreadDuration   WIRED 2026-08-16
//	                      (#509) — and its exemption said the blocker was that
//	                      BondTerms "has NO SCHEMA AT ALL", which was wrong: the
//	                      message existed in fixed_income.proto and only the
//	                      ContractTerms oneof lacked a bond case. THE DEAD-ENTRY
//	                      ARM IS WHAT RETIRED IT, on the commit that added the
//	                      caller, rather than the entry outliving its repair.
//	RegisterLiquidityRisk LiquidationHorizon, LVaR99                  WIRED 2026-08-16
//	                      (#509). Its exemption said a liquidity.Provider had "no
//	                      production implementation"; internal/risk/liquiditysource
//	                      is one, and the baseVaR the entry also called missing is
//	                      resolved out of the registry at evaluation time, so there
//	                      was nothing left to supply. Note the wiring registers ONE
//	                      of the two measures: LVaR99 needs a spread and nothing in
//	                      this repository persists a bid or an ask, so the provider
//	                      declares ServesSpread()=false and compute declines it.
//	RegisterGreeks        Delta, Gamma, Vega, Theta, Rho              0 callers
//	RegisterXVA           CVA, DVA, FVA                               0 callers
//	RegisterStructuredRisk StructDuration, StructConvexity, StructWAL   WIRED
//	                      2026-08-20 (#572). Its exemption said a StructuredProvider
//	                      "with no production implementation" was missing, and that
//	                      was true for a reason no other entry had: the SCHEMA could
//	                      not describe a securitization at all — reference.v1.
//	                      StructuredTerms existed and no ContractTerms oneof case,
//	                      no terms.Kind and no Go code reached it, so a provider
//	                      could not have been written. #572 ruled on the schema, and
//	                      the dead-entry arm retired this entry on the commit that
//	                      added the caller.
//	NewRevaluer / NewBondRevaluer / NewLiveModelProvider
//
// The risk engine registers DefaultRegistry plus varmodel.Register — EIGHT
// measures — and `internal/risk/engine.filterMeasures` documents that "unknown
// names are dropped". So a client asking for DV01 on a bond book receives 200
// with DV01 absent, which is indistinguishable from a portfolio holding no
// bonds. "Nothing configured" and "checked, and fine" looked the same.
//
// # What counts as a seam, and why it is derived from types
//
// An exported function under internal/risk/ that TAKES a *Registry or a value
// whose type name ends in Providers. Both mean the same thing: this function
// exists to be handed the engine's registry or a bundle of live data sources at
// a composition root, and nowhere else.
//
// NOT a naming convention. A rule of "functions called Register*" would miss
// NewRevaluer and NewBondRevaluer — two of the eleven — and would break the day
// someone names one Wire or Install. The parameter is the thing that makes it a
// composition-root seam, so the parameter is what this reads.
//
// DefaultRegistry and NewRegistry are excluded by that same rule and correctly
// so: they RETURN a *Registry and take nothing. They are the thing seams are
// handed, not seams.
//
// # What counts as a caller
//
// A call from a DIFFERENT package, in a non-test file, resolved through the
// calling file's import block — never by bare name. A name-only match would let
// any `foo.Register(` in the module vouch for `varmodel.Register`, and the guard
// would pass while checking a weaker property than its name claims.
//
// # The known weakness, stated rather than discovered later
//
// A seam called only by ANOTHER dark seam counts as live here. That is the same
// limitation the import-granularity guard has, one level finer, and it is why
// the exemption list below is the real record: every dark seam names the issue
// and the specific missing provider, so the reason survives independently of
// what the call graph can prove.

// darkSeamExempt maps "<package>.<Func>" to the issue tracking its wiring.
//
// EVERY ENTRY NAMES THE MISSING PIECE, not just an issue number. "Not wired yet"
// without saying what is absent is how eleven seams stayed dark long enough for
// the analytics above them to be benchmarked (#471) before anyone noticed they
// do not reach production.
var darkSeamExempt = map[string]string{
	"internal/risk/compute.RegisterGreeks": "#509 — Delta/Gamma/Vega/Theta/Rho. CORRECTED " +
		"2026-08-16: this entry used to say the TermsProvider was unconstructed and that " +
		"SpotProvider had zero implementations. Both are now false. termsource.Provider is built " +
		"at services/risk-engine/cmd/risk-engine/main.go for the FI seam and satisfies the option " +
		"seam with the same value; internal/risk/spotsource is the production SpotProvider. Curve " +
		"is satisfiable too — RegisterGreeks defaults it to FlatCurve(0) and *curve.Curve " +
		"implements pricing.DiscountCurve directly. ONE SEAM BLOCKS THIS, and it is not a Go " +
		"problem: VolProvider needs a volsurface.QuoteProvider, which needs OBSERVED OPTION " +
		"PREMIUMS, and nothing in this repository carries them. market-ingest opens one websocket " +
		"per configured (instrument, venue) and the deployed maps are spot-only; the vendor Source " +
		"is an unimplemented SDK seam serving synthetic prices; backfill writes bars rather than " +
		"price_observations. Ahead of even that, the contract-terms store HAS NO PRODUCTION " +
		"WRITER — terms.Postgres.Put has only test callers — so the chain a QuoteProvider would " +
		"read is empty, and the FI measures wired in #519 are computing over an empty terms table " +
		"today (kanz_risk_fi_terms_missing_total is what says so). A contract-terms loader is the " +
		"first honest step and it pays for FI before it pays for Greeks. Note also that wiring " +
		"this OVERWRITES the DefaultRegistry Delta placeholder, so it changes an already-served " +
		"measure rather than adding five.",
	"internal/risk/compute.RegisterXVA": "#509 — CVA/DVA/FVA. Needs an XVAProvider (exposure " +
		"profiles + counterparty credit curves). internal/risk/pricing/credit holds the calibrator " +
		"and is itself dark for want of a live CDS quote source (#113, #203), so this cannot be wired " +
		"to anything real without synthesising counterparty spreads — which #345 rules out " +
		"explicitly: a SUCCESSFUL calibration of invented quotes is worse than a failed one.",
	"internal/risk/compute.NewRevaluer": "#509 — full-revaluation scenario shocks, as opposed to " +
		"the sensitivity approximation. Takes GreeksProviders, so it is blocked on exactly what " +
		"RegisterGreeks is blocked on.",
	"internal/risk/compute.NewBondRevaluer": "#509 — curve-shift revaluation for bonds. Takes " +
		"FIProviders, so it is blocked on the same missing BondTerms schema.",
	"internal/risk/scenario.EvaluateCurveShift": "#509 — needs a BondRevaluer, which is " +
		"NewBondRevaluer, which is dark. Reachable the moment the FI branch is.",
	"internal/risk/scenario.EvaluateReval": "#509 — needs a Revaluer, which is NewRevaluer, which " +
		"is dark. Reachable the moment the Greeks branch is.",
	"internal/risk/scenario.EvaluateFactorShock": "#509 — needs a *factormodel.Model, produced by " +
		"factormodel.Fit through NewLiveModelProvider, which is dark.",
	"internal/risk/compute/var.RegisterMonteCarlo": "#509 — MODEL-01e Monte-Carlo VaR. The " +
		"historical sibling (varmodel.Register) IS wired at risk-engine/main.go:202, so this one is " +
		"dark by CHOICE rather than by a missing provider: it takes the same ReturnsProvider. " +
		"Deciding whether to serve both is the open question, and until it is answered the platform " +
		"ships a Monte-Carlo VaR nobody can request.",
	"internal/risk/compute.RegisterMarginRisk": "#408 control 4 — LiquidationProximity. Needs a " +
		"MarginProvider, and TWO PIECES OF ANOTHER SERVICE'S DEPLOY-TIME CONFIG are what is " +
		"missing, neither of them a Go problem. (a) PORTFOLIO -> VENUE ACCOUNT: the bindings are " +
		"execution.AccountBindings, parsed by execution.ParseBindings from the OMS's own " +
		"environment at OMS startup, which is also where ErrAccountShared refuses to start on a " +
		"shared account (#415). risk-engine holds no such config, and giving it a second copy " +
		"would be a second answer that can disagree with the first — on precisely the mapping " +
		"#415 made load-bearing. (b) VENUE SYMBOL -> MARK: collateral.v1." +
		"VenueLiquidationPrice.venue_symbol is the exchange's own spelling and deliberately NOT a " +
		"Kanz instrument_id, because the adapter's symbol map is a ONE-WAY instrument -> symbol " +
		"table; inverting it needs the venue adapter's configuration, and internal/risk/spotsource " +
		"is keyed by InstrumentID. So nothing inside the risk module can pair a liquidation price " +
		"with a price to measure it against. Both are held by the OMS and the venue adapters, and " +
		"the honest wiring is a provider built where they live rather than a third copy of either. " +
		"NOTE also #417: control 1 is unproven against a live venue (#70), so no leveraged " +
		"position exists on this estate for this measure to have read yet.",
}

// seamRef is a composition-root seam: an exported function that takes the
// engine's registry or a bundle of live providers.
type seamRef struct {
	pkg  string // module-relative package dir, e.g. "internal/risk/compute"
	name string
}

func (s seamRef) String() string { return s.pkg + "." + s.name }

func TestNoMeasureSeamIsDarkAndUntracked(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	// ===== 1. Find every seam under internal/risk/ =====
	seams := map[seamRef]bool{}
	seamPkgs := map[string]bool{}
	walkGoFiles(t, root, "internal/risk", fset, func(rel string, f *ast.File) {
		pkgDir := filepath.ToSlash(filepath.Dir(rel))
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !fn.Name.IsExported() || fn.Type.Params == nil {
				continue
			}
			if !takesRegistryOrProviders(fn.Type.Params) {
				continue
			}
			seams[seamRef{pkgDir, fn.Name.Name}] = true
			seamPkgs[pkgDir] = true
		}
	})

	// NON-VACUITY, first half: the scan must actually find the seam family. A
	// broken walk would report zero dark seams and pass having checked nothing —
	// which is this guard's own failure mode, and the one it exists to prevent
	// one level down.
	if len(seams) < 15 {
		t.Fatalf("found only %d composition-root seams under internal/risk — the walk or the "+
			"parameter rule is broken, not the estate", len(seams))
	}

	// ===== 2. Count callers, resolved through each file's import block =====
	called := map[seamRef]bool{}
	walkGoFiles(t, root, ".", fset, func(rel string, f *ast.File) {
		callerPkg := filepath.ToSlash(filepath.Dir(rel))
		// Alias → module-relative package dir, for the seam packages only.
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
			// QUALIFIED ONLY, and only from a DIFFERENT package. A bare-name
			// match would let any foo.Register( in the module vouch for
			// varmodel.Register; an intra-package call proves nothing about
			// whether a composition root reaches it.
			if dir, ok := alias[id.Name]; ok && dir != callerPkg {
				called[seamRef{dir, sel.Sel.Name}] = true
			}
			return true
		})
	})

	// ===== 3. Default-deny =====
	var dark []string
	seenExempt := map[string]bool{}
	live := 0

	for s := range seams {
		if called[s] {
			live++
			continue
		}
		if reason, ok := darkSeamExempt[s.String()]; ok {
			seenExempt[s.String()] = true
			t.Logf("%s: dark, tracked — %s", s, reason)
			continue
		}
		dark = append(dark, s.String())
	}

	// NON-VACUITY, second half: some seams MUST resolve as live, or the caller
	// resolution is broken and every seam looks dark — which would make the
	// exemption list grow to cover a bug in this file.
	if live < 3 {
		t.Fatalf("only %d of %d seams resolved to a caller — the import-alias resolution is "+
			"broken. ComputeMeasures, engine.New and varmodel.Register are all wired and must "+
			"resolve", live, len(seams))
	}

	if len(dark) > 0 {
		sort.Strings(dark)
		t.Errorf("%d composition-root seam(s) under internal/risk have NO caller in any other "+
			"package: %v.\n"+
			"A registration seam nobody calls is a MEASURE THIS PLATFORM DOES NOT SERVE — and it "+
			"does not fail, it is dropped: internal/risk/engine.filterMeasures drops unknown names, "+
			"so a client asking for it gets 200 with the measure absent, indistinguishable from a "+
			"portfolio that has none of that instrument.\n"+
			"no_dark_capability_test.go cannot see this: internal/risk/compute is imported "+
			"constantly, so the package is bright while its seams are dark (#509).\n"+
			"Wire it at a composition root, or add an entry to darkSeamExempt NAMING THE ISSUE AND "+
			"THE MISSING PROVIDER.",
			len(dark), dark)
	}

	// DEAD-ENTRY ARM: an exemption for a seam that now has a caller means
	// somebody wired it, and for a seam that no longer exists means it was
	// renamed or removed. Either way the entry is a claim about the estate that
	// is no longer true, and it must not outlive its repair.
	for name, reason := range darkSeamExempt {
		if seenExempt[name] {
			continue
		}
		t.Errorf("exemption for %q is stale — the seam now has a caller, or no longer exists "+
			"under the parameter rule. Delete the entry (%s)", name, reason)
	}
}

// takesRegistryOrProviders reports whether any PARAMETER is a *Registry or a
// value whose type name ends in "Providers", qualified or not.
//
// Parameters only, deliberately. DefaultRegistry and NewRegistry RETURN a
// *Registry and take nothing — they are what a seam is handed, not seams.
func takesRegistryOrProviders(params *ast.FieldList) bool {
	for _, field := range params.List {
		if typeNameOf(field.Type) {
			return true
		}
	}
	return false
}

// typeNameOf reports whether an expression names Registry (by pointer) or a
// *Providers type, seeing through the package qualifier so compute.Registry and
// Registry are the same thing to this rule.
func typeNameOf(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.StarExpr:
		return typeNameOf(t.X)
	case *ast.SelectorExpr:
		return isSeamTypeName(t.Sel.Name)
	case *ast.Ident:
		return isSeamTypeName(t.Name)
	}
	return false
}

func isSeamTypeName(name string) bool {
	return name == "Registry" || strings.HasSuffix(name, "Providers")
}

// walkGoFiles parses every non-test .go file under root/sub and hands each to fn
// with its module-relative path.
func walkGoFiles(t *testing.T, root, sub string, fset *token.FileSet, fn func(rel string, f *ast.File)) {
	t.Helper()
	base := filepath.Join(root, filepath.FromSlash(sub))
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// testdata and vendor hold code that is not this module's estate.
			if n := d.Name(); n == "testdata" || n == "vendor" || n == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		parsed, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		fn(filepath.ToSlash(rel), parsed)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", sub, err)
	}
}
