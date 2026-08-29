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

// EVERY LONG-LIVED MAP IN pkg/bus MUST HAVE AN EVICTOR (#805).
//
// # What this is protecting
//
// Producer.sequence was keyed by {event_type, partition_key}, and partition_key
// ON THE ORDER PATH IS THE ORDER ID — the OMS stamps it in the one builder every
// order FACT goes through, and the api-gateway, both venue adapters and
// optimization do the same. One Producer per process, no evictor, roughly 2-6
// permanent entries per order the process had ever touched, for the life of the
// pod.
//
// It was the platform's highest-throughput long-lived map and it was monotonic.
// At institutional rates the OMS heap grows until the pod is OOM-killed — on the
// execution path that means orders in flight at an unknown state and a restart
// that has to reconcile them. It arrives as a memory eviction rather than an
// error, so nothing on the trading path reports it until the pod dies.
//
// The whole package had exactly three delete( calls and all three belonged to
// DedupWindow.
//
// # Why the guard is about the FIELD and not about a number
//
// A test can show today's map is bounded. What it cannot notice is the NEXT map
// added to a type that lives for the process — a per-subject counter, a
// per-tenant cache, a per-consumer watermark. Every one of those is a leak on
// the same shape, and none of them fails anything until a pod dies of it.
//
// So this is default-deny over the map-typed fields of pkg/bus's long-lived
// types: each one must be named by a delete( somewhere in the package, or
// exempted on purpose with the reason.
//
// # Why it reads the AST with comments detached
//
// Three guards in this tree have already passed while asserting nothing, because
// a regex over raw source matched their own explanatory prose. The paragraph you
// are reading cannot satisfy anything below.

const busDir = "pkg/bus"

// mapEvictionExempt names a map field that may live without an evictor, and why.
//
// DEFAULT-DENY: a field on a long-lived type, not listed here and not deleted
// from anywhere, fails. An entry is a claim that the key space is BOUNDED BY
// CONSTRUCTION — not that the map is small today, and not that nobody has seen
// it grow.
//
// IT IS EMPTY, AND THE FIRST DRAFT OF THIS GUARD IS WHY THAT MATTERS. It carried
// an invented entry for a field that does not exist, and the dead-entry arm
// below caught it on the first run. An exemption nobody checks is worse than no
// exemption, because it reads as a decision somebody made.
var mapEvictionExempt = map[string]string{}

func TestEveryLongLivedMapInTheBusHasAnEvictor(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, filepath.FromSlash(busDir))

	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", busDir, err)
	}

	fset := token.NewFileSet()
	mapFields := map[string]string{} // field name -> "Type.field"
	deleted := map[string]bool{}     // field names appearing as delete(x.field, …)
	files := 0

	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		// Mode 0: comments are not attached, so this cannot match its own prose.
		file, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", p, perr)
		}
		files++

		ast.Inspect(file, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.TypeSpec:
				st, ok := v.Type.(*ast.StructType)
				if !ok {
					return true
				}
				// LONG-LIVED IS "GUARDED BY A MUTEX", and that is a discriminator
				// rather than a proxy. A map that needs a lock is one more than one
				// goroutine reaches, which means it belongs to something the process
				// holds — a Producer, a window, a registry. A map on a per-message
				// value like Message.Headers dies with the message and cannot leak,
				// and the first draft of this guard flagged exactly that before this
				// arm existed.
				if !isMutexGuarded(st) {
					return true
				}
				for _, f := range st.Fields.List {
					if _, isMap := f.Type.(*ast.MapType); !isMap {
						continue
					}
					for _, name := range f.Names {
						mapFields[name.Name] = v.Name.Name + "." + name.Name
					}
				}
			case *ast.CallExpr:
				id, ok := v.Fun.(*ast.Ident)
				if !ok || id.Name != "delete" || len(v.Args) == 0 {
					return true
				}
				if sel, ok := v.Args[0].(*ast.SelectorExpr); ok {
					deleted[sel.Sel.Name] = true
				}
			}
			return true
		})
	}

	// NON-VACUITY 1: the package was actually read.
	if files < 5 {
		t.Fatalf("parsed only %d non-test file(s) in %s — the package moved and this guard is "+
			"scanning nothing", files, busDir)
	}
	// NON-VACUITY 2: the field scan found the maps this guard exists for. If the
	// struct walk breaks, every assertion below passes over an empty set.
	for _, must := range []string{"sequence", "expiry"} {
		if mapFields[must] == "" {
			t.Fatalf("the struct scan did not find a map field named %q — it is one of the two "+
				"this guard was written about, so the walk has drifted and its verdict is empty",
				must)
		}
	}
	// NON-VACUITY 3: the delete scan can find one. DedupWindow.expiry has had an
	// evictor since it was written; if this stops being seen, the delete walk is
	// broken rather than the estate clean.
	if !deleted["expiry"] {
		t.Fatal("the delete scan found no delete(…expiry…) — DedupWindow has evicted from it since " +
			"it was written, so this walk is broken and would report every map as unevicted or " +
			"none of them")
	}

	var unevicted []string
	usedExempt := map[string]bool{}
	for field, where := range mapFields {
		if deleted[field] {
			continue
		}
		if reason, ok := mapEvictionExempt[field]; ok {
			usedExempt[field] = true
			t.Logf("%s: exempt — %s", where, reason)
			continue
		}
		unevicted = append(unevicted, where)
	}

	if len(unevicted) > 0 {
		sort.Strings(unevicted)
		t.Errorf("these long-lived maps have no delete( anywhere in %[2]s: %[1]v.\n\n"+
			"A map on a type that lives as long as the process, with a key that grows with "+
			"traffic, is a leak that reports nothing until the pod is OOM-killed. On the "+
			"execution path that means orders in flight at an unknown state and a restart that "+
			"has to reconcile them — which is what Producer.sequence cost, keyed by order id, in "+
			"the platform's highest-throughput map (#805).\n\n"+
			"Give it an evictor (Producer.gcSequence and DedupWindow.gc are the two shapes this "+
			"package already uses), or add it to mapEvictionExempt with the argument that its key "+
			"space is bounded BY CONSTRUCTION — not that it looks small today.", unevicted, busDir)
	}

	_ = usedExempt

	// DEAD-ENTRY ARM: an exemption that matches no field, or one that has since
	// grown an evictor, is a claim nobody is checking any more.
	for field, reason := range mapEvictionExempt {
		if mapFields[field] == "" {
			t.Errorf("exemption for %q (%s) matches no map field in %s — delete it", field, reason, busDir)
			continue
		}
		if deleted[field] {
			t.Errorf("exemption for %q (%s) is stale: the field now HAS an evictor, so the "+
				"exemption is recording permission nobody needs", field, reason)
		}
	}
}

// isMutexGuarded reports whether a struct holds a sync.Mutex or sync.RWMutex —
// this guard's test for "the process holds this, and more than one goroutine
// reaches it".
func isMutexGuarded(st *ast.StructType) bool {
	for _, f := range st.Fields.List {
		sel, ok := f.Type.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		pkg, isIdent := sel.X.(*ast.Ident)
		if !isIdent || pkg.Name != "sync" {
			continue
		}
		if sel.Sel.Name == "Mutex" || sel.Sel.Name == "RWMutex" {
			return true
		}
	}
	return false
}
