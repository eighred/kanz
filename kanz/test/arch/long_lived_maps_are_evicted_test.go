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

// EVERY LONG-LIVED MAP IN A COVERED PACKAGE MUST HAVE AN EVICTOR (#805, #834, #814).
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
// The second scope is the same shape one service out (#834): the api-gateway's
// quota middleware held a token bucket and an in-flight counter per PRINCIPAL
// and evicted from neither, so a gateway that stayed up across staff turnover,
// service accounts and rotated subjects accumulated one entry per identity it
// had ever served. The gateway is the sole entry point for POST /v1/orders, so
// its heap is on the critical path for placing an order at all.
//
// The third scope is the same shape on the CONTROL plane (#814). The pre-trade
// compliance gate's say-it-once warning ledger was a bare map keyed partly by
// the order's instrument id — a free-form string off SubmitOrder that nothing
// on the path validates beyond "not empty" — so a caller entitled to one
// portfolio grew a permanent entry per invented instrument on the enforcement
// point that decides whether capital moves, and every one of those orders was
// REFUSED, so it cost its author nothing. The same eleven lines existed a second
// time on the mandate registry, which is how a repair to one would have missed
// the other.
//
// # Why the guard is about the FIELD and not about a number
//
// A test can show today's map is bounded. What it cannot notice is the NEXT map
// added to a type that lives for the process — a per-subject counter, a
// per-tenant cache, a per-consumer watermark. Every one of those is a leak on
// the same shape, and none of them fails anything until a pod dies of it.
//
// So this is default-deny over the map-typed fields of each covered package's
// long-lived types: each one must be named by a delete( on ITS OWN receiver
// somewhere in the package, or exempted on purpose with the reason.
//
// # Why it is keyed by Type.field and not by field name
//
// Field names repeat. The middleware package held `buckets` on TWO types and
// only one of them ever got an evictor — under a name-keyed check the fixed one
// would have vouched for the unfixed one, and the guard would have gone green
// over a live leak. Attribution is therefore by the enclosing method's receiver:
// delete(q.inflight, k) inside func (q *quota) counts for quota.inflight and for
// nothing else. A delete on a field the guard cannot attribute is an ERROR, not
// a pass — see the unattributed arm below.
//
// # Why it reads the AST with comments detached
//
// Three guards in this tree have already passed while asserting nothing, because
// a regex over raw source matched their own explanatory prose. The paragraph you
// are reading cannot satisfy anything below.

// evictionScope is one package under this guard, plus the anchors that prove the
// walk actually reached it. The anchors are NON-VACUITY, not policy: they are
// existing fields whose disappearance means the scan drifted rather than the
// estate got cleaner, so every assertion below would pass over an empty set.
type evictionScope struct {
	dir string
	// minFiles is a floor on non-test files parsed — a moved or renamed package
	// otherwise scans zero files and reports nothing wrong.
	minFiles int
	// mustFind are Type.field names the struct walk has to see.
	mustFind []string
	// mustEvict is a Type.field that has had an evictor since it was written. It
	// also proves receiver attribution works: it only resolves if the delete site
	// was tied back to its own type.
	mustEvict string
}

var evictionScopes = []evictionScope{
	{
		dir:       "pkg/bus",
		minFiles:  5,
		mustFind:  []string{"Producer.sequence", "DedupWindow.expiry"},
		mustEvict: "DedupWindow.expiry",
	},
	{
		dir:       "services/api-gateway/internal/middleware",
		minFiles:  5,
		mustFind:  []string{"quota.inflight", "bucketSet.buckets", "replayCache.entries"},
		mustEvict: "replayCache.entries",
	},
	{
		dir:      "internal/compliance",
		minFiles: 8,
		mustFind: []string{"sayOnce.volatile", "MandateRegistry.byKey"},
		// MandateRegistry.rejected has had its evictor since Put was written, and
		// it predates this scope — so it proves receiver attribution here without
		// vouching for the field #814 repaired.
		mustEvict: "MandateRegistry.rejected",
	},
}

// mapEvictionExempt names a map field ("Package: Type.field") that may live
// without an evictor, and why.
//
// DEFAULT-DENY: a field on a long-lived type, not listed here and not deleted
// from anywhere, fails. An entry is a claim that the key space is BOUNDED BY
// CONSTRUCTION — not that the map is small today, and not that nobody has seen
// it grow.
//
// IT WAS EMPTY, AND THE FIRST DRAFT OF THIS GUARD IS WHY THAT MATTERS. It carried
// an invented entry for a field that does not exist, and the dead-entry arm
// below caught it on the first run. An exemption nobody checks is worse than no
// exemption, because it reads as a decision somebody made.
//
// The three entries below arrived with the internal/compliance scope. Each names
// a key space whose every component is written by the ESTATE — a portfolio that
// passed entitlement, or a mandate an operator published — as opposed to the
// instrument id that made the gate's other ledger #814.
var mapEvictionExempt = map[string]string{
	"internal/compliance: sayOnce.stable": "every key's variable part is (tenant, portfolio). " +
		"An order reaches the gate only after order.delegatedAndEntitled, so a \"user:\" issuer can " +
		"name only a portfolio the gateway stamped from its verified principal, and the two machine " +
		"issuers on order.order.submit take theirs from a strategy signal or a rebalance proposal. " +
		"The registry's keys are narrower still: warnMisfiled and warnSystemFallback only form one " +
		"for a portfolio that ALREADY has a published mandate. Evicting here would be wrong rather " +
		"than merely unnecessary — an ungoverned portfolio is ungoverned all day, so a horizon would " +
		"re-announce it forever. The caller-keyed half of the same type is sayOnce.volatile, which " +
		"IS swept.",
	"internal/compliance: MandateRegistry.byKey": "keyed by (tenant, portfolio) and written only by " +
		"Put, whose only caller is the mandate replay off a COMPACTED config subject. An entry " +
		"exists because an operator published a mandate for that portfolio; nothing a trading " +
		"caller sends can create one.",
	"internal/compliance: MandateRegistry.tenantsByPortfolio": "same writer and same source as " +
		"byKey — one entry per portfolio some tenant has published a mandate for. It exists so a " +
		"missed lookup can say WHY (#243), and it cannot outgrow the set of published mandates.",
}

func TestEveryLongLivedMapHasAnEvictor(t *testing.T) {
	root := moduleRoot(t)
	if len(evictionScopes) == 0 {
		t.Fatal("no packages are under this guard — it is asserting nothing")
	}

	usedExempt := map[string]bool{}

	for _, scope := range evictionScopes {
		t.Run(scope.dir, func(t *testing.T) {
			mapFields, deleted, unattributed, files := scanEviction(t, filepath.Join(root, filepath.FromSlash(scope.dir)))

			// NON-VACUITY 1: the package was actually read.
			if files < scope.minFiles {
				t.Fatalf("parsed only %d non-test file(s) in %s — the package moved and this guard is "+
					"scanning nothing", files, scope.dir)
			}
			// NON-VACUITY 2: the struct walk found the maps this scope names.
			for _, must := range scope.mustFind {
				if !mapFields[must] {
					t.Fatalf("the struct scan did not find a map field %q in %s — it is one this guard "+
						"was written about, so the walk has drifted and its verdict is empty",
						must, scope.dir)
				}
			}
			// NON-VACUITY 3: the delete scan can find one, ATTRIBUTED TO ITS TYPE.
			if !deleted[scope.mustEvict] {
				t.Fatalf("the delete scan did not attribute any delete( to %s in %s — that field has had "+
					"an evictor since it was written, so receiver attribution is broken and this walk "+
					"would report every map as unevicted or none of them", scope.mustEvict, scope.dir)
			}

			// UNATTRIBUTED ARM: a delete on a struct field the guard cannot tie to
			// the enclosing method's receiver. Silently counting it for every field
			// of that name is how a name-keyed check let one type's fix vouch for
			// another's leak, so it fails loudly and asks to be taught instead.
			if len(unattributed) > 0 {
				sort.Strings(unattributed)
				t.Errorf("delete( sites in %s that this guard cannot attribute to a receiver: %v.\n\n"+
					"Attribution is what keeps one type's evictor from vouching for another type's "+
					"leak. Either move the delete into a method on the owning type, or extend "+
					"scanEviction to resolve this shape — do not leave it unattributed.",
					scope.dir, unattributed)
			}

			var unevicted []string
			for field := range mapFields {
				if deleted[field] {
					continue
				}
				key := scope.dir + ": " + field
				if reason, ok := mapEvictionExempt[key]; ok {
					usedExempt[key] = true
					t.Logf("%s: exempt — %s", key, reason)
					continue
				}
				unevicted = append(unevicted, field)
			}

			if len(unevicted) > 0 {
				sort.Strings(unevicted)
				t.Errorf("these long-lived maps have no delete( on their own receiver anywhere in %[2]s: %[1]v.\n\n"+
					"A map on a type that lives as long as the process, with a key that grows with "+
					"traffic, is a leak that reports nothing until the pod is OOM-killed. On the "+
					"execution path that means orders in flight at an unknown state and a restart that "+
					"has to reconcile them — which is what Producer.sequence cost, keyed by order id, in "+
					"the platform's highest-throughput map (#805); one service out it was the gateway's "+
					"per-principal quota maps, on the only route that accepts an order (#834).\n\n"+
					"Give it an evictor (bus.Producer.gcSequence, bus.DedupWindow.gc and "+
					"middleware.bucketSet.gc are the shapes the estate already uses), or add "+
					"%[2]q + \": \" + the field to mapEvictionExempt with the argument that its key "+
					"space is bounded BY CONSTRUCTION — not that it looks small today.", unevicted, scope.dir)
			}
		})
	}

	// DEAD-ENTRY ARM: an exemption that matches no field in any scope, or one
	// whose field has since grown an evictor, is a claim nobody is checking any
	// more. It runs outside the subtests so an exemption naming a scope that no
	// longer exists is caught too.
	for key, reason := range mapEvictionExempt {
		if !usedExempt[key] {
			t.Errorf("exemption %q (%s) was never applied — either it names no map field in a "+
				"covered package, or that field now HAS an evictor. Delete it: an exemption nobody "+
				"checks reads as a decision somebody made.", key, reason)
		}
	}
}

// scanEviction parses dir's non-test files and returns the mutex-guarded map
// fields it holds ("Type.field"), the ones a delete( names through their own
// receiver, any delete on a struct field it could not attribute, and the number
// of files read.
func scanEviction(t *testing.T, dir string) (mapFields, deleted map[string]bool, unattributed []string, files int) {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}

	fset := token.NewFileSet()
	mapFields = map[string]bool{}
	deleted = map[string]bool{}

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
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
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
					mapFields[ts.Name.Name+"."+name.Name] = true
				}
			}
			return true
		})

		// Deletes are walked per FuncDecl so the enclosing method's receiver is
		// known. A closure a method RETURNS is still inside that decl — which is
		// where quota.acquire's release func deletes from — so it attributes too.
		for _, d := range file.Decls {
			fn, isFunc := d.(*ast.FuncDecl)
			if !isFunc || fn.Body == nil {
				continue
			}
			recvName, recvType := receiverOf(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok || id.Name != "delete" || len(call.Args) == 0 {
					return true
				}
				sel, ok := call.Args[0].(*ast.SelectorExpr)
				if !ok {
					return true // delete on a local map: not a field, cannot outlive its scope
				}
				base, ok := sel.X.(*ast.Ident)
				if ok && recvName != "" && base.Name == recvName {
					deleted[recvType+"."+sel.Sel.Name] = true
					return true
				}
				unattributed = append(unattributed,
					filepath.Base(p)+": delete("+exprString(sel)+", ...) in "+fn.Name.Name)
				return true
			})
		}
	}
	return mapFields, deleted, unattributed, files
}

// receiverOf returns a method's receiver variable name and its base type name.
// Both are empty for a plain function.
func receiverOf(fn *ast.FuncDecl) (name, typeName string) {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return "", ""
	}
	f := fn.Recv.List[0]
	if len(f.Names) == 1 {
		name = f.Names[0].Name
	}
	typ := f.Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		typeName = id.Name
	}
	if name == "" || typeName == "" {
		return "", ""
	}
	return name, typeName
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
