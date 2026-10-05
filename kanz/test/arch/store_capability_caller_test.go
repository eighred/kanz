package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A CAPABILITY DECLARED ON A STORE INTERFACE MUST HAVE A PRODUCTION CALLER.
//
// This is #229's actual root cause, and it is not a performance bug.
//
// ledger.Store declared SaveSnapshot. Postgres implemented it — a correct
// upsert against a real table with RLS. MemoryStore implemented it. A test
// exercised it. 0001_ledger.sql created ledger_snapshots and documented it as
// "a periodic snapshot (PERS-01 stance) bounds replay to the journal tail".
// MaterializeCurrent read the snapshot back and folded only the tail past its
// watermark, and carried a comment saying it was "bounded by the snapshot".
//
// Nothing called SaveSnapshot. Not once, anywhere in the tree.
//
// So ledger_snapshots was a permanently empty table, LoadSnapshot always
// returned ErrNoSnapshot, and every NAV request folded the portfolio's entire
// lifetime journal. Five artefacts — an interface entry, two implementations, a
// migration, and a comment — all described a mechanism that had never once
// executed in production, and every one of them read as evidence that it had.
// The build was green, the tests were green, and the guarantee was fictional.
//
// A missing caller is invisible to every other gate we have. The compiler is
// happy (the method satisfies an interface). Vet is happy. Coverage is happy —
// a test called it. Only this asks the question that matters: does anything
// that RUNS IN PRODUCTION use this?
//
// SCOPE. Store interfaces only, named below. These are the seams where "the
// durable backend supports X" is asserted as a platform property, so an
// unreachable method is a false claim rather than dead code. It deliberately
// does NOT police every interface in the tree: plenty exist to be implemented
// by callers, and a guard that misfired on those would be turned off.
var storeCapabilityInterfaces = []struct {
	// file holds the interface declaration.
	file string
	// name is the interface's type name.
	name string
}{
	{"services/accounting/internal/ledger/store.go", "Store"},
	{"services/alternatives/internal/fund/store.go", "Store"},
	{"services/audit/internal/audit/store.go", "Store"},
}

// storeCapabilityExempt is the default-deny allow-list of interface methods
// permitted to have no production caller, keyed "<iface>.<Method>". An entry
// needs the issue that retires it, and the dead-entry arm below fails the build
// if one stops matching — an exemption must not outlive its repair.
// #1343 removed the unused fund-wide enumeration. audit.Store.Get was never
// unused: handleGet and lineage.Reconstruct have called it since AUDIT-01.
// Retain that operational API and reject an exemption masking its callers.
// This scanner resolves selector names, not receiver types; a matching name
// still needs caller/type review before claiming a reachable domain workflow.
var storeCapabilityExempt = map[string]string{}

func TestStoreCapabilitiesHaveProductionCallers(t *testing.T) {
	root := moduleRoot(t)

	// Every non-test .go file in the module: the set of places a production
	// caller could live.
	prod := productionSources(t, root)
	if len(prod) == 0 {
		// NON-VACUITY. A scanner that finds no sources passes this guard no
		// matter how many orphaned capabilities the tree carries.
		t.Fatal("found zero non-test .go files — the scanner is broken, not the code")
	}

	seenExempt := map[string]bool{}
	totalMethods := 0

	for _, target := range storeCapabilityInterfaces {
		path := filepath.Join(root, filepath.FromSlash(target.file))
		pkg, methods := interfaceMethods(t, path, target.name)
		if len(methods) == 0 {
			// NON-VACUITY, per interface: a rename or a moved file would make
			// every assertion below trivially true.
			t.Fatalf("found no methods on %s in %s — the interface moved or was renamed, "+
				"and this guard is now asserting nothing", target.name, target.file)
		}
		totalMethods += len(methods)

		for _, m := range methods {
			key := pkg + "." + target.name + "." + m
			callers := callSitesOf(prod, m)
			if reason, ok := storeCapabilityExempt[key]; ok {
				seenExempt[key] = true
				if len(callers) > 0 {
					t.Errorf("storeCapabilityExempt %s is stale: %d production selector call(s) now satisfy this guard; review their receiver types and remove the exemption", key, len(callers))
				} else {
					t.Logf("exempt: %s has no production caller (%s)", key, reason)
				}
				continue
			}
			if len(callers) == 0 {
				t.Errorf("%s is declared on the %s.%s interface and implemented, but NOTHING "+
					"outside a _test.go file calls it.\n"+
					"That is #229: SaveSnapshot had an interface entry, two implementations, a "+
					"migration and a comment describing what it guaranteed — and no caller, so the "+
					"guarantee had never once executed while every artefact read as evidence that "+
					"it had.\n"+
					"Wire it up, delete it, or add it to storeCapabilityExempt with the issue that "+
					"retires it.", key, pkg, target.name)
			}
		}
	}

	if totalMethods == 0 {
		t.Fatal("inspected zero interface methods — the guard is broken")
	}

	// DEAD-ENTRY ARM. An exemption that no longer matches a real method is an
	// exemption that outlived its repair, and it would silently permit a future
	// method of the same name.
	for key := range storeCapabilityExempt {
		if !seenExempt[key] {
			t.Errorf("storeCapabilityExempt has %q, but no such interface method exists — the "+
				"exemption outlived its repair; delete it", key)
		}
	}
}

// productionSources returns path -> contents for every non-test .go file in the
// module, excluding generated SDKs and the arch tests themselves.
func productionSources(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable subtree is not this test's business
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".claude", "vendor", "gen", "kanz-schemas-go":
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		if strings.HasSuffix(name, ".pb.go") || strings.HasSuffix(name, "_grpc.pb.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		out[path] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// interfaceMethods parses path and returns its package name plus the method
// names declared on the named interface.
func interfaceMethods(t *testing.T, path, iface string) (string, []string) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var methods []string
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != iface {
			return true
		}
		it, ok := ts.Type.(*ast.InterfaceType)
		if !ok {
			return true
		}
		for _, field := range it.Methods.List {
			if _, isFunc := field.Type.(*ast.FuncType); !isFunc {
				continue // an embedded interface, not a method
			}
			for _, name := range field.Names {
				methods = append(methods, name.Name)
			}
		}
		return false
	})
	sort.Strings(methods)
	return f.Name.Name, methods
}

// callSitesOf returns the files containing a SELECTOR CALL `.Method(`.
//
// The leading dot is what makes this work without a type checker, and it is not
// incidental. A declaration is `func (p *Postgres) SaveSnapshot(` and an
// interface entry is `SaveSnapshot(ctx context.Context, ...)` — neither has a
// dot before the name, so neither counts itself as a caller. Only an actual
// invocation through a value (`st.SaveSnapshot(ctx, snap)`) matches. The
// declaring file is therefore searched like any other: MaterializeCurrent lives
// beside the interface it reads through, and it is a real caller.
//
// A regex-grade check on purpose, matching this tree's other arch guards. A
// whole-module type-checked call graph would be a second thing to get wrong,
// and the failure this catches — ZERO call sites anywhere — does not need
// resolution precision. The direction of any imprecision is safe: a same-named
// method on an unrelated type could mask a genuinely dead capability, but
// nothing here can invent a caller for a method the tree never invokes.
func callSitesOf(sources map[string]string, method string) []string {
	needle := "." + method + "("
	var hits []string
	for path, body := range sources {
		if strings.Contains(body, needle) {
			hits = append(hits, path)
		}
	}
	sort.Strings(hits)
	return hits
}
