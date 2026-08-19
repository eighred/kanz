package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// AN RPC SERVED WITH NO CALLER IS A CONTROL NOBODY CAN REACH (#560).
//
// # The case this exists for
//
// order.v1.ListPendingApprovals was served from #410: implemented in
// services/oms/internal/grpcsrv, registered at the OMS composition root, tested.
// Its only reference outside that service was a fakeClient in a _test.go file
// written to satisfy the generated interface. For the whole of that time an
// approver had no way to discover what needed a second signature, and #539's
// approve route would have been unusable without it — the route refuses an empty
// digest with 400, and a held order is deliberately absent from the orders table.
//
// Nothing caught it. The code compiled, the OMS suite passed, and the double
// answered "nothing pending" convincingly enough that its OWN comment had to
// warn about it:
//
//	The day one does, this must be given the same record-the-request shape
//	ListOrders has above — a double that silently answers "nothing pending"
//	would certify a queue nobody can see.
//
// # Why no_dark_capability_test.go cannot see this
//
// That guard answers "does this package have an importer", and its doc says so:
// it works at IMPORT granularity, which is the blind spot #509 names.
// services/oms/internal/order has many importers. The uncalled thing was a
// METHOD. This is the sibling that asks the other question, rather than a
// rewrite that would make both answers vaguer.
//
// # Why the AST rather than a grep
//
// Three guards in this suite have passed with the checked thing deleted because
// they matched their own comments or a nearby step name. A call site is a
// syntactic fact, so this reads *ast.CallExpr and a grep can never come back.
// It also gives the distinction that matters for free: implementing a method is
// a FuncDecl and calling it is a CallExpr, so a server implementing the RPC does
// not count as reaching it.
//
// # Test callers do not count, and that is the whole point
//
// no_dark_capability_test.go deliberately counts test importers, because a
// package exercised by tests is reachable. The opposite is true here: a
// fakeClient satisfying a generated interface is precisely what made
// ListPendingApprovals look consumed for months. Counting it would reproduce the
// bug exactly.

// # WHAT THIS PROVES, AND THE WEAKER THING IT ACTUALLY CHECKS
//
// A CALL SITE IS NOT A REACHABLE CALLER, and this guard cannot tell the
// difference. It was written expecting one survivor — inference.v1.Predict —
// and Predict turned out to HAVE a production call site:
//
//	internal/prediction/sync_client.go:193   c.stub.Predict(callCtx, pbFV)
//
// inside a client nothing constructs. NewSyncClient's only callers are in
// resilience_test.go, so no deployment reaches that line.
//
// STILL TRUE AFTER #112 LANDED, which is the part worth re-reading. #112 wired
// internal/prediction/registry at risk-engine and its dark-capability exemption
// is retired — but it wired a REPORTER of the model plane, not a scorer, and
// NewSyncClient still has no production constructor. So the pair of guards is
// blind to this in the mirror-image way it always was: no_dark_capability works
// at import granularity and now sees an imported package, this one works at call
// granularity and sees the call. The retirement of that exemption must not be
// read as "Go scores predictions" — both guards' docs say so, and #416 is where
// the decision about what acts on a prediction lives.
//
// So the pair of them brackets the property without either owning it: a method
// nothing calls fails here, a package nothing imports fails there, and a call
// inside an unwired package passes both. That third case is inference.v1.Predict
// and it is #416's, it is tracked, and it is written down here so a green run is
// not read as "every served RPC is reachable in production". It is not what this
// asserts.
//
// The exemption map is EMPTY for the same reason pendingGRPCDomainChecks is: an
// empty default-deny list is the honest state when nothing is exempt, and the
// dead-entry arm below stops one being added and then quietly outliving its
// repair.
var servedRPCExempt = map[string]string{}

func TestEveryServedRPCHasACaller(t *testing.T) {
	root := moduleRoot(t)
	schema := loadProtoSchema(t, filepath.Join(filepath.Dir(root), "kanz-schemas", "proto"))

	// NON-VACUITY, part one: the proto scan. An empty rpc list would pass this
	// guard having examined nothing, which is the failure mode it exists to
	// prevent one level up.
	if len(schema.rpcs) < 15 {
		t.Fatalf("found only %d rpcs across kanz-schemas/proto — the schema scan is broken, not "+
			"the estate", len(schema.rpcs))
	}

	called := callSitesByMethod(t, root)

	// NON-VACUITY, part two: the anchor. ListPendingApprovals is the method this
	// guard was written for, and #539 gave it a production caller in the gateway.
	// If the call-site scan stops seeing that, the guard has gone quiet.
	if len(called["ListPendingApprovals"]) == 0 {
		t.Fatalf("ListPendingApprovals has no detected caller — it does have one "+
			"(services/api-gateway/internal/gateway), so the CallExpr scan has stopped matching. "+
			"Found call sites for %d methods.", len(called))
	}

	var dark []string
	seenExempt := map[string]bool{}

	for _, r := range schema.rpcs {
		if len(called[r.method]) > 0 {
			continue
		}
		if reason, ok := servedRPCExempt[r.method]; ok {
			seenExempt[r.method] = true
			t.Logf("%s.%s: no caller in this module, tracked — %s", r.service, r.method, reason)
			continue
		}
		dark = append(dark, r.service+"."+r.method)
	}

	if len(dark) > 0 {
		sort.Strings(dark)
		t.Errorf("%d rpc(s) are served with NO non-test caller anywhere in this module: %v.\n\n"+
			"This is invisible to every other signal: the service compiles, its own tests pass, "+
			"and a generated-interface double in a _test.go answers convincingly. "+
			"order.v1.ListPendingApprovals sat like this from #410 until #539, and for that whole "+
			"time an approver could not discover a single order awaiting their signature.\n\n"+
			"Call it, or add an entry to servedRPCExempt naming the ISSUE that will — or the "+
			"caller outside this module, if the only legitimate one is the SPA, an external "+
			"client or kanz-py.", len(dark), dark)
	}

	// DEAD-ENTRY ARM. An exemption for a method that now HAS a caller means
	// somebody wired it; the entry must go, or the next reader is told a lie
	// about the estate.
	for method, reason := range servedRPCExempt {
		if !seenExempt[method] {
			t.Errorf("exemption for %q is stale — the rpc now has a caller, or it no longer exists "+
				"in kanz-schemas. Delete the entry (%s)", method, reason)
		}
	}
}

// callSitesByMethod returns method name → the production files calling it.
//
// PRODUCTION ONLY: _test.go files are skipped, because a double satisfying the
// generated interface is what this guard is looking past.
func callSitesByMethod(t *testing.T, root string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == "vendor" || n == "gen" || n == "testdata" || strings.HasPrefix(n, ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, pErr := parser.ParseFile(fset, path, nil, 0)
		if pErr != nil {
			// A file this suite cannot parse is not a silent pass: the whole
			// module builds, so an unparseable file means the walk is wrong.
			t.Fatalf("parse %s: %v", path, pErr)
		}
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			// A CALL, NOT A DECLARATION. The server implementing the rpc is a
			// FuncDecl and never reaches here, which is the distinction that
			// makes "served" and "reached" separable at all.
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				out[sel.Sel.Name] = append(out[sel.Sel.Name], rel)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}
