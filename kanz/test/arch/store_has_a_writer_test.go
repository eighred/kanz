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

// A STORE THAT IS READ IN PRODUCTION AND NEVER WRITTEN IN PRODUCTION IS AN
// EMPTY TABLE SERVING ZEROS.
//
// #509 again, one level below where the other two guards can see it.
//
// internal/marketdata/terms is the contract-terms store. It has a production
// READER: termsource.Provider (internal/risk/termsource/provider.go — its
// TermsAsOf and Curve call store.LatestAsOf), constructed at
// services/risk-engine/cmd/risk-engine/main.go and handed straight to
// compute.RegisterFIRisk on the next line.
// DV01, Duration, Convexity and SpreadDuration are registered off it.
//
// It has NO production WRITER. terms.Postgres.Put is called from exactly two
// places, both _test.go, and the only non-test construction of a
// reference.v1.ContractTerms in the module is postgres.go — the unmarshal
// target inside the READ path. Nothing in this repository has ever produced a
// contract term.
//
// So four fixed-income measures are registered, served, and computing over an
// empty table. Every bond in every portfolio resolves to no terms and the
// measures return zero — which is exactly what a portfolio holding no bonds
// returns. "Nothing configured" and "checked, and fine" look the same, and the
// only thing that says otherwise is a counter (kanz_risk_fi_terms_missing_total,
// registered by the risk-engine composition root in main.go) that nobody is
// required to look at.
//
// # Why the two existing dark-capability guards are both green on this
//
// no_dark_capability_test.go works at IMPORT granularity, and
// internal/marketdata/terms IS imported — by termsource and by the risk-engine
// composition root. The package is bright.
//
// no_dark_measure_seam_test.go works at SEAM granularity, and RegisterFIRisk IS
// called by the risk-engine composition root (main.go). The seam is live.
//
// Both are telling the truth. The capability is wired, the seam is called, and
// the data is not there. That gap is what this guard closes.
//
// # What counts as a store, and what the rule misses
//
// A named type declared in a package under internal/ — the module's private
// tree, both kanz/internal/... and services/<svc>/internal/... — with BOTH:
//
//   - an exported method whose name begins with a WRITE verb, and
//   - an exported method whose name begins with a READ verb,
//
// where each of those methods takes context.Context as its FIRST parameter.
//
// THE CONTEXT IS THE DURABILITY TEST, and it is doing real work rather than
// decorating the rule. A call that crosses to Postgres, Redis, Kubernetes or a
// broker takes a Context because it can block and must be cancellable. Without
// that clause the verb rule alone matches 58 types rather than 26, and the 32 it
// adds have no table behind them at all — server.Readiness (Set(bool) /
// Ready() bool, one atomic
// per service), internal/risk.Cache (the RISK-11 degraded-mode in-memory cache),
// livequote.LiveQuotes (a last-tick map) — and a guard that reports those is a
// guard somebody switches off.
//
// What it misses, stated here rather than discovered later:
//
//  1. VERB NAMING IS A CONVENTION, NOT A TYPE. A store whose writer is called
//     Ingest, Apply or Materialize has no write-shaped method under this rule,
//     so it is not a store here and cannot be flagged. That is the rule's real
//     hole: the defect is invisible precisely when the writer is named
//     unusually. The read side has the same hole, but a store with an unusual
//     READ name simply drops out — it is the write side that produces a false
//     pass.
//  2. A DURABLE STORE THAT TAKES NO CONTEXT — file-backed, embedded — is dark
//     here. Nothing in this tree is shaped that way today.
//  3. THIS IS A WHOLE-STORE CHECK, NOT A PER-METHOD ONE. It asks "is this table
//     ever written in production", so ONE live writer discharges it.
//     schema-registry storage.Put has zero production callers while Register
//     writes the same table through the same interface; this guard is silent
//     there, correctly, and store_capability_caller_test.go is the guard that
//     owns per-method deadness on named interfaces.
//  4. IT OVER-INCLUDES, deliberately unfixed. operator/internal/grpcsrv.Server
//     and operator/internal/provision.Provisioner are service surfaces, not
//     stores; their List*/Add*/Set* methods take a ctx and satisfy the shape.
//     They pass, because a surface that is read is also written. A name-based
//     exclusion list would be a second thing to keep true, and the cost of
//     leaving them in is that someone eventually reads a slightly odd entry in
//     the failure message.
//
// # What counts as a caller
//
// A CALL EXPRESSION `x.Method(...)` in a non-test .go file, where `x` is not an
// import alias in that file, and the file is either in the store's own package
// or in a package that IMPORTS it. Three parts, each of which was earned:
//
//   - CALL FORM, NOT BARE SELECTOR. internal/risk/termsource/provider.go
//     contains `pricing.Put` — an option-type CONSTANT — and it imports
//     internal/marketdata/terms. A selector-only match reads that as a writer
//     for terms.Postgres.Put and this whole guard passes on the one defect it
//     was built for. The `(` is the difference.
//   - NOT AN IMPORT ALIAS. `storage.ParseRef(...)` is a package-level function,
//     not a method on a store value; excluding identifiers that name an import
//     keeps package functions from vouching for same-named methods.
//   - IMPORT-SCOPED, NEVER BY BARE NAME. `.Put(` is called from eight different
//     packages here. Requiring the caller to import the store's package stops
//     datamaster's Put from vouching for terms' Put — the same standard, and
//     the same reason, as no_dark_measure_seam_test.go states for seams.
//
// IN-PACKAGE CALLERS COUNT, and that is a DELIBERATE DEPARTURE from the seam
// guard. A seam exists to be handed a registry at a composition root, so an
// intra-package call proves nothing about it. A store's writer is usually the
// projector sitting next to it: services/audit/internal/audit/projector.go
// calls p.store.Append(ctx, rec) inside package audit, and NewProjector is
// wired at services/audit/cmd/audit/main.go. Every audit event in
// production goes through that line. Excluding it would have forced an
// exemption on a store that is written constantly — an exemption that states
// something false about a healthy store is how a guard earns its deletion.
//
// Resolution is at PACKAGE + METHOD granularity, not package + type + method,
// because it cannot be finer without a type checker and because finer would be
// meaningless anyway: these stores come in (Memory, Postgres) pairs behind one
// interface, and a caller holds the interface. A call to `.Get(` in a file
// importing datamaster/store vouches for MemoryGoldenStore.Get and
// PostgresGolden.Get alike, which is the correct answer for both.
//
// # The blind spot
//
// A writer that is itself reached only by dead code counts as live here. That is
// the same limitation the import-granularity guard has, one level finer, and it
// is why the exemption map below is the durable record: the reason a store is
// unwritten survives independently of what a call graph can prove.
//
// THAT IS NOT HYPOTHETICAL, AND IT IS ABOUT TO MATTER. As of 2026-08-16
// internal/marketdata/termsload exists and builds a reference.v1.ContractTerms
// (okx.go) but does not yet call Put. The moment it does, this guard
// resolves a writer and the terms exemption below goes stale — CORRECTLY, but
// only if termsload is also CONSTRUCTED somewhere. A loader that calls Put and
// that nothing builds would turn this guard green while the table stays as empty
// as it is today. no_dark_capability_test.go is what covers that half: it fails
// if termsload has no importer. Neither guard alone closes the loop; whoever
// lands the loader owes both a caller and a composition root, and should confirm
// the fix by watching kanz_risk_fi_terms_missing_total stop climbing rather than
// by watching this file pass.

// storeWithoutAWriterExempt maps "<package>.<Type>" to the issue that will
// supply the writer, and to WHAT THE WRITER IS. An entry saying only "not wired
// yet" is how a store stays empty for a year while four measures report zero off
// it. The dead-entry arm below deletes the entry for you the day a writer lands.
var storeWithoutAWriterExempt = map[string]string{
	"internal/marketdata/terms.Postgres": "#509 — the contract-terms store. Put has TWO callers " +
		"and both are _test.go (internal/marketdata/terms/postgres_test.go, " +
		"internal/risk/termsource/provider_test.go); the only non-test construction of a " +
		"reference.v1.ContractTerms in the module is postgres.go, which is the unmarshal " +
		"target on the way OUT. Nothing has ever produced a term. The reader is live and reaches " +
		"production: termsource.Provider is built at services/risk-engine/cmd/risk-engine/" +
		"main.go and handed to compute.RegisterFIRisk on the next line, so DV01, Duration, " +
		"Convexity and SpreadDuration are registered and computing over an empty table today — " +
		"every bond resolves to no terms and every measure returns zero, indistinguishable from a " +
		"portfolio holding no bonds. THE WRITER IS NAMED AND IS BEING BUILT: " +
		"internal/marketdata/termsload, a contract-terms loader that produces ContractTerms and " +
		"calls terms.Postgres.Put. Until it lands, kanz_risk_fi_terms_missing_total " +
		"(main.go) is the only thing in the estate that says the measures are hollow. " +
		"SECOND, SMALLER GAP IN THE SAME STORE, tracked by the same issue and NOT separately " +
		"exempt because this guard is whole-store: ChainAsOf has no production caller either, so " +
		"the option-chain read path is dark on top of being unfed.",
}

// storeRef identifies a store-shaped type by its module-relative package
// directory and type name.
type storeRef struct {
	pkg string // e.g. "internal/marketdata/terms"
	typ string // e.g. "Postgres"
}

func (s storeRef) String() string { return s.pkg + "." + s.typ }

// storeWriteVerbs and storeReadVerbs are method-name prefixes. Both lists are
// conventions this tree already follows; see hole (1) in the doc above for what
// that costs.
var (
	storeWriteVerbs = []string{
		"Put", "Insert", "Save", "Write", "Create", "Append",
		"Upsert", "Record", "Store", "Register", "Add", "Set", "Update", "Delete",
	}
	storeReadVerbs = []string{
		"Get", "Query", "Latest", "List", "Load", "Fetch", "Read", "Find",
		"Lookup", "All", "Count", "Bars", "Chain", "Range", "Scan", "Exists", "Has",
	}
)

// storeMethods is the read/write split for one store-shaped type.
type storeMethods struct {
	reads  map[string]bool
	writes map[string]bool
}

func TestEveryStoreReadInProductionIsAlsoWrittenInProduction(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	// ===== 1. Find every store-shaped type under an internal/ tree =====
	found := map[storeRef]*storeMethods{}
	storePkgs := map[string]bool{}
	walkGoFiles(t, root, ".", fset, func(rel string, f *ast.File) {
		pkgDir := filepath.ToSlash(filepath.Dir(rel))
		if !isModuleInternalPkg(pkgDir) {
			return
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || !fn.Name.IsExported() {
				continue
			}
			typ := storeReceiverName(fn.Recv.List[0].Type)
			if typ == "" || !takesContextFirst(fn.Type.Params) {
				continue
			}
			ref := storeRef{pkgDir, typ}
			m := found[ref]
			if m == nil {
				m = &storeMethods{reads: map[string]bool{}, writes: map[string]bool{}}
				found[ref] = m
			}
			// Writes are matched FIRST: "Add"/"AddAll" and "Store*" are writes,
			// and "All"/"Scan" are reads, so an unordered test would let AddAll
			// land on the read side and hide a store with no live writer.
			switch {
			case hasVerbPrefix(fn.Name.Name, storeWriteVerbs):
				m.writes[fn.Name.Name] = true
			case hasVerbPrefix(fn.Name.Name, storeReadVerbs):
				m.reads[fn.Name.Name] = true
			}
		}
	})

	stores := map[storeRef]*storeMethods{}
	for ref, m := range found {
		if len(m.reads) > 0 && len(m.writes) > 0 {
			stores[ref] = m
			storePkgs[ref.pkg] = true
		}
	}

	// NON-VACUITY, first arm: this module is 26 services deep and durable
	// everywhere. Finding almost no stores means the walk, the verb lists or the
	// context clause broke, and the guard would pass having inspected nothing —
	// which is the exact failure mode it exists to catch one level down.
	if len(stores) < 15 {
		t.Fatalf("found only %d store-shaped types under internal/ — the walk, the verb lists or "+
			"the context-first rule is broken, not the estate", len(stores))
	}

	// ===== 2. Resolve callers through each file's import block =====
	called := map[storeRef]bool{} // storeRef.typ holds a METHOD name here
	walkGoFiles(t, root, ".", fset, func(rel string, f *ast.File) {
		callerPkg := filepath.ToSlash(filepath.Dir(rel))
		// Which store packages this file can hold a value from: the ones it
		// imports, plus its own if it is a store package.
		reachable := map[string]bool{}
		if storePkgs[callerPkg] {
			reachable[callerPkg] = true
		}
		// Every import's local identifier, so a package-level call like
		// storage.ParseRef( is not mistaken for a method call.
		pkgIdents := map[string]bool{}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			name := filepath.Base(path)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			pkgIdents[name] = true
			if dir, ok := strings.CutPrefix(path, modulePath+"/"); ok && storePkgs[dir] {
				reachable[dir] = true
			}
		}
		if len(reachable) == 0 {
			return
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
			// A CALL, and not one qualified by a package name. `pricing.Put` in
			// termsource/provider.go is an option-type constant in a file that
			// imports the terms store; counting selectors rather than calls
			// makes that constant vouch for terms.Postgres.Put, and this guard
			// then passes on the one defect it was written for.
			if id, ok := sel.X.(*ast.Ident); ok && pkgIdents[id.Name] {
				return true
			}
			for pkg := range reachable {
				called[storeRef{pkg, sel.Sel.Name}] = true
			}
			return true
		})
	})

	// ===== 3. Default-deny: read in production, never written in production ===
	var unwritten []string
	seenExempt := map[string]bool{}
	bothLive := 0

	refs := make([]storeRef, 0, len(stores))
	for ref := range stores {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].String() < refs[j].String() })

	for _, ref := range refs {
		m := stores[ref]
		liveRead := anyCalled(called, ref.pkg, m.reads)
		liveWrite := anyCalled(called, ref.pkg, m.writes)
		if liveRead && liveWrite {
			bothLive++
			continue
		}
		if !liveRead {
			// Not this guard's finding. A store nothing reads yet serves no
			// zeros to anybody; no_dark_capability_test.go owns whether the
			// package is reachable at all.
			continue
		}
		if reason, ok := storeWithoutAWriterExempt[ref.String()]; ok {
			seenExempt[ref.String()] = true
			t.Logf("%s: read in production, never written — tracked: %s", ref, reason)
			continue
		}
		unwritten = append(unwritten, ref.String()+" reads="+joinSorted(m.reads)+
			" writes="+joinSorted(m.writes))
	}

	// NON-VACUITY, second arm: most stores in this tree ARE read and written, so
	// if the count of stores resolving to both collapses, the caller resolution
	// is broken and every store looks unwritten. Without this, the correct
	// response to a bug in THIS FILE would look like "add twenty exemptions".
	//
	// THE THRESHOLD IS CALIBRATED AGAINST A MUTATION, not guessed. 25 of 26
	// stores resolve to both today. Breaking only the cross-package half of the
	// resolution — corrupting the modulePath prefix so no import matches — takes
	// it to 9, because in-package callers still resolve and hold up a floor. A
	// threshold of 5 SURVIVED that mutation and would have shipped an arm that
	// fires for nothing; 18 fails it while leaving room for stores to come and go.
	if bothLive < 18 {
		t.Fatalf("only %d of %d stores resolved to BOTH a production reader and a production "+
			"writer — the caller resolution is broken. marketdata/store, accounting/ledger, "+
			"audit, datamaster/store and wealth/book are all read and written and must resolve",
			bothLive, len(stores))
	}

	if len(unwritten) > 0 {
		sort.Strings(unwritten)
		t.Errorf("%d durable store(s) have a production READER and NO production WRITER: %v.\n"+
			"That is an empty table serving zeros. internal/marketdata/terms is why this guard "+
			"exists (#509): its reader reaches production through termsource.Provider into "+
			"compute.RegisterFIRisk, so DV01/Duration/Convexity/SpreadDuration are registered and "+
			"computing over a table nothing has ever written — and a bond book returning zero is "+
			"indistinguishable from a portfolio holding no bonds.\n"+
			"Neither other guard can see this. no_dark_capability_test.go works at IMPORT "+
			"granularity and the package IS imported; no_dark_measure_seam_test.go works at SEAM "+
			"granularity and RegisterFIRisk IS called. Both are green and the data is not there.\n"+
			"Wire a writer, or add an entry to storeWithoutAWriterExempt NAMING THE ISSUE AND THE "+
			"WRITER THAT WILL FILL IT.",
			len(unwritten), unwritten)
	}

	// DEAD-ENTRY ARM. An exemption for a store that now has a writer means
	// somebody built the loader; one for a store that no longer matches the rule
	// means it was renamed, moved, or lost its reader. Either way the entry
	// asserts something about the estate that is no longer true, and an
	// exemption must not outlive its repair.
	for name, reason := range storeWithoutAWriterExempt {
		if seenExempt[name] {
			continue
		}
		t.Errorf("exemption for %q is stale — the store now has a production writer, has lost its "+
			"production reader, or no longer matches the store rule. Delete the entry (%s)",
			name, reason)
	}
}

// isModuleInternalPkg reports whether a module-relative package directory is in
// the module's private tree — kanz/internal/... or services/<svc>/internal/...
//
// pkg/ is excluded on purpose: it is this module's PUBLIC surface, and a type
// there may legitimately have no in-module writer because its consumers are
// outside the module. cmd/ is excluded because a composition root holds stores
// rather than declaring them.
func isModuleInternalPkg(pkgDir string) bool {
	return pkgDir == "internal" || strings.HasPrefix(pkgDir, "internal/") ||
		strings.Contains(pkgDir, "/internal/") || strings.HasSuffix(pkgDir, "/internal")
}

// storeReceiverName returns the receiver's type name, seeing through the
// pointer and through generic instantiation.
func storeReceiverName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return storeReceiverName(t.X)
	case *ast.IndexExpr:
		return storeReceiverName(t.X)
	case *ast.IndexListExpr:
		return storeReceiverName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return ""
}

// takesContextFirst reports whether the first parameter is a context.Context.
//
// This is the durability clause, and it is what separates a store from a
// mutex-guarded map. See the doc comment above for the 28 in-memory types it
// removes and why reporting them would be fatal to the guard.
func takesContextFirst(params *ast.FieldList) bool {
	if params == nil || len(params.List) == 0 {
		return false
	}
	sel, ok := params.List[0].Type.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "context" && sel.Sel.Name == "Context"
}

// hasVerbPrefix reports whether name is, or begins with, one of verbs.
func hasVerbPrefix(name string, verbs []string) bool {
	for _, v := range verbs {
		if strings.HasPrefix(name, v) {
			return true
		}
	}
	return false
}

// anyCalled reports whether ANY of methods has a production caller in pkg.
//
// Any, not all: the property is "is this table ever written in production", and
// one live writer falsifies "the table is empty". Per-method deadness belongs to
// store_capability_caller_test.go.
func anyCalled(called map[storeRef]bool, pkg string, methods map[string]bool) bool {
	for m := range methods {
		if called[storeRef{pkg, m}] {
			return true
		}
	}
	return false
}

// joinSorted renders a method set deterministically for the failure message.
func joinSorted(set map[string]bool) string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return "[" + strings.Join(out, " ") + "]"
}
