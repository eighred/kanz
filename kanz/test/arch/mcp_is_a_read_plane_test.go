package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// MCP IS AN AGENT-FACING READ PLANE, AND THE IMPORT GRAPH IS WHERE THAT IS TRUE
// OR NOT (#743).
//
// AGENTS.md states three boundaries for any MCP surface, and calls each one
// worth a guard:
//
//  1. It must never receive, expose, store or proxy a venue credential.
//  2. It must never expose order placement, execution, cancel, amend, or any
//     other trading capability. Read plane only.
//  3. It is not the venue transport, and adding one must not create a second
//     answer to "how does the platform talk to an exchange".
//
// A unit test can show that today's tools are reads. It cannot show that a
// capital action is UNREACHABLE, because reachability is a property of what the
// package can import — and the day somebody adds a tool, the import is what
// arrives first. So this reads the graph.
//
// WHY DENY-BY-DEFAULT ON A LIST OF PACKAGES rather than a list of forbidden
// symbols: a symbol list is a list of the ways somebody already thought of. The
// OMS's order package, the execution router, the venue adapters and the
// credential machinery are the reachable capital surface of this estate; naming
// them and refusing the lot is the only version that survives a package being
// renamed or a new one appearing beside it.
//
// WHAT IT CANNOT CHECK: that a tool the plane DOES expose projects narrowly
// enough. "Server-side filtered and projected" is a judgement about a payload,
// and services/mcp's own tests carry it.

// mcpTree is the plane under guard.
const mcpTree = "services/mcp"

// mcpForbiddenImports are the trees an MCP read plane may not reach, each with
// the boundary it would breach.
var mcpForbiddenImports = map[string]string{
	"internal/execution":                 "the execution router and the Venue seam — boundary 3, and the shortest path to boundary 2",
	"internal/venueadapter":              "venue adapters and exchange auth — boundaries 1 and 3",
	"internal/venueadapter/exchangeauth": "exchange credential signing — boundary 1, the one that is unrecoverable",
	"services/venue-binance":             "a venue connector — boundary 3",
	"services/venue-okx":                 "a venue connector — boundary 3",
	"services/oms/internal/order":        "the OMS order aggregate: submit, amend, cancel — boundary 2",
	"internal/secret":                    "credential material — boundary 1",
	"internal/vault":                     "credential material — boundary 1",
}

// mcpForbiddenSchemas are generated SDK packages whose presence would mean the
// plane is speaking a capital or venue contract, whatever it does with it.
var mcpForbiddenSchemas = map[string]string{
	"kanz-schemas-go/venue/v1": "venue.v1 is the OMS → adapter transport — boundary 3",
	"kanz-schemas-go/order/v1": "order.v1 carries SubmitOrder, AmendOrder and CancelOrder — boundary 2",
}

// mcpImportExempt maps "<file> -> <import>" to the reason it is permitted.
//
// IT IS EMPTY. There is no argued case: a read plane that needs the order
// aggregate or a venue client is not a read plane, and an exemption here would
// be the decision to make it something else — which #743 says requires its own
// issue, not a line in a test.
var mcpImportExempt = map[string]string{}

func TestTheMCPPlaneCannotReachACapitalOrVenueCapability(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, filepath.FromSlash(mcpTree))
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("%s does not exist yet — this guard arms with the plane", mcpTree)
	}

	var breaches []string
	seenExempt := map[string]bool{}
	files := 0

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			t.Fatalf("parse %s: %v — the guard cannot check what it cannot parse", path, perr)
		}
		rel, _ := filepath.Rel(root, path)
		slash := filepath.ToSlash(rel)
		files++
		for _, imp := range f.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				continue
			}
			if reason, bad := mcpBreach(p); bad {
				key := slash + " -> " + p
				if why, ok := mcpImportExempt[key]; ok {
					seenExempt[key] = true
					t.Logf("%s: exempt — %s", key, why)
					continue
				}
				breaches = append(breaches, key+" ("+reason+")")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", mcpTree, err)
	}

	// NON-VACUITY: the plane has source. A walk that found nothing would report a
	// clean graph having read no files at all, which is the failure mode every
	// guard in this tree is written against.
	if files < 2 {
		t.Fatalf("walked %d Go file(s) under %s — the scan is broken, not the plane", files, mcpTree)
	}

	if len(breaches) > 0 {
		sort.Strings(breaches)
		t.Errorf("%d import(s) put a capital or venue capability within reach of the MCP plane: %v.\n"+
			"AGENTS.md: MCP is read-only, deny-by-default, and NOT the venue transport — it may not "+
			"place, cancel or amend orders, may not call a venue, and may not hold a credential. An "+
			"import is how that becomes possible, and it arrives before the tool that uses it. If "+
			"this plane genuinely needs one of these, that is a decision with its own issue (#743 "+
			"out-of-scope), not an entry in mcpImportExempt.", len(breaches), breaches)
	}

	for key, why := range mcpImportExempt {
		if !seenExempt[key] {
			t.Errorf("exemption for %q (%s) matches no import — delete it", key, why)
		}
	}
}

// mcpBreach reports whether an import path crosses one of the three boundaries.
func mcpBreach(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, modulePath+"/")
	if ok {
		for tree, reason := range mcpForbiddenImports {
			if rest == tree || strings.HasPrefix(rest, tree+"/") {
				return reason, true
			}
		}
	}
	for schema, reason := range mcpForbiddenSchemas {
		if strings.Contains(path, schema) {
			return reason, true
		}
	}
	return "", false
}

// THE PLANE MUST NOT GROW A WRITE METHOD BY NAME EITHER (#743 boundary 2).
//
// The import guard above catches reaching a capital capability. This catches the
// other direction: a handler on this plane whose own name claims one. The two
// are different failures — a tool called submit_order that reads nothing is
// still a tool this plane must not offer, and its absence of imports would not
// say so.
func TestTheMCPPlaneDeclaresNoWriteSideCapability(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, filepath.FromSlash(mcpTree))
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("%s does not exist yet", mcpTree)
	}

	// Verbs that name a capital action. Matched on the DECLARED tool names and
	// exported functions, not on comments — a guard that greps prose matches its
	// own explanation, which is how three guards in this tree once passed with
	// the checked thing deleted.
	verbs := []string{"submitorder", "placeorder", "cancelorder", "amendorder",
		"executeorder", "sendorder", "routeorder"}

	var offenders []string
	decls := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		rel, _ := filepath.Rel(root, path)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			decls++
			lower := strings.ToLower(fn.Name.Name)
			for _, v := range verbs {
				if strings.Contains(lower, v) {
					offenders = append(offenders, filepath.ToSlash(rel)+": "+fn.Name.Name)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if decls < 5 {
		t.Fatalf("found %d function declaration(s) under %s — the scan is broken", decls, mcpTree)
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("%d function(s) on the MCP plane name a capital action: %v.\n"+
			"This plane is read-only (#743). A function that claims to place, cancel or amend an "+
			"order does not belong here whatever it currently does — the name is what the next "+
			"person builds on.", len(offenders), offenders)
	}
}
