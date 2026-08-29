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

// THE TWO MUTATIONS THAT DECIDE A HELD ORDER'S FATE MUST EXCLUDE EACH OTHER (#796).
//
// # What this is protecting
//
// A proposal held for a second signature has exactly two ways to stop being
// pending: somebody signs it (Claim) or its deadline is announced
// (AnnounceExpiry). They run in different processes on different triggers — an
// approval off the bus, and a ticker in the composition root — and each writes a
// DIFFERENT column, so neither is excluded by the other's write by accident.
//
// AnnounceExpiry always carried `approver = ''`. Claim carried nothing in the
// other direction, and MemoryProposals had the same omission, so the two stores
// AGREED and every service-level test in the OMS ran on a seam that certified
// the gap. An approval whose Go-side deadline check passed could then land on a
// proposal the sweeper had already announced dead: the estate received a
// terminal ORDER_REJECTED and afterwards ORDER_APPROVED, ORDER_ACCEPTED and
// fills for the same order_id. Two terminal answers to one command, with capital
// moving on a maker-checker approval the platform had declared dead.
//
// # Why a guard rather than only the tests
//
// The behavioural tests (services/oms/internal/order/proposals_expiry_claim_test.go)
// prove the property for the pair that exists TODAY. What they cannot notice is a
// THIRD writer of this row, or a rewrite of either statement that drops a clause
// while the tests still pass because they only drive one interleaving. The
// predicates are four tokens in two files; this is where the four are read
// together.
//
// # Why it reads the AST with comments detached
//
// Three guards in this tree have already passed while asserting nothing, because
// a regex over raw source matched their own explanatory prose. Everything below
// is matched inside a named function's BODY, parsed with comments not attached —
// so the paragraph you are reading cannot satisfy it.

// proposalsFile is the OMS's proposal store, both backends in one file.
const proposalsFile = "services/oms/internal/order/proposals.go"

// exclusionPair names, per method, what its predicate must read for the pair to
// be proper. The SQL tokens are matched in the method's string literals; the Go
// ones in its selector expressions.
var exclusionPair = []struct {
	method string
	sql    []string
	fields []string
	why    string
}{
	{
		method: "Claim",
		sql:    []string{"approver = ''", "expiry_announced_at IS NULL"},
		why: "an approval must not decide a proposal whose expiry the estate has already been " +
			"told about — that is two terminal answers to one command, and capital moving on an " +
			"approval the platform declared dead",
	},
	{
		method: "AnnounceExpiry",
		sql:    []string{"approver = ''", "expiry_announced_at IS NULL"},
		why: "an expiry must not be announced for a proposal somebody signed (the deadline " +
			"stopped mattering) nor announced twice (the sweeper would republish the same " +
			"terminal FACT on every tick, forever)",
	},
}

// memoryExclusion is the same pair in the in-memory backend, which decides with
// Go comparisons rather than a WHERE clause. It is listed separately because a
// fix spelled only in SQL leaves every service-level test in the OMS running on
// a seam that still accepts what Postgres refuses.
var memoryExclusion = map[string][]string{
	"Claim":          {"Approver", "ExpiryAnnouncedAt"},
	"AnnounceExpiry": {"Approver", "ExpiryAnnouncedAt"},
}

func TestAClaimAndItsExpiryExcludeEachOther(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, filepath.FromSlash(proposalsFile))

	fset := token.NewFileSet()
	// Mode 0: comments are not attached, so this cannot match its own prose.
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", proposalsFile, err)
	}

	// methods[receiverType][methodName] = body
	methods := map[string]map[string]*ast.BlockStmt{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Recv == nil || len(fn.Recv.List) == 0 {
			continue
		}
		recv := proposalReceiverName(fn.Recv.List[0].Type)
		if methods[recv] == nil {
			methods[recv] = map[string]*ast.BlockStmt{}
		}
		methods[recv][fn.Name.Name] = fn.Body
	}

	// NON-VACUITY 1: both backends were found. A moved or renamed store would
	// otherwise leave every assertion below checking an empty map.
	for _, recv := range []string{"PostgresProposals", "MemoryProposals"} {
		if len(methods[recv]) == 0 {
			t.Fatalf("no methods on %s were parsed out of %s — the store moved or was renamed, and "+
				"this guard is asserting nothing", recv, proposalsFile)
		}
	}

	// THE DURABLE HALF: the predicates live in the UPDATE.
	for _, want := range exclusionPair {
		body, ok := methods["PostgresProposals"][want.method]
		if !ok {
			t.Errorf("PostgresProposals has no %s — the pair cannot be checked", want.method)
			continue
		}
		sql := strings.Join(proposalSQLIn(body), "\n")
		for _, clause := range want.sql {
			if !strings.Contains(sql, clause) {
				t.Errorf("PostgresProposals.%s no longer carries %q in its predicate.\n\n%s\n\n"+
					"Its rows-affected is supposed to be the VERDICT: every condition that decides "+
					"whether this mutation may happen belongs in the WHERE, because a SELECT to "+
					"check first reopens check-then-act between two pods.",
					want.method, clause, want.why)
			}
		}
	}

	// NON-VACUITY 2: the string-literal matcher can say NO, and is scoped to one
	// method. Put is an INSERT and has never had the expiry predicate; a matcher
	// that scanned the whole file would "find" it here.
	if body, ok := methods["PostgresProposals"]["Put"]; ok {
		if strings.Contains(strings.Join(proposalSQLIn(body), "\n"), "expiry_announced_at IS NULL") {
			t.Error("PostgresProposals.Put appears to carry the expiry predicate. Either the " +
				"statement changed, or this guard is reading the whole file instead of one method " +
				"— in which case its findings above mean nothing.")
		}
	} else {
		t.Error("PostgresProposals has no Put — the scoping arm of this guard cannot run")
	}

	// THE IN-MEMORY HALF: the same pair, spelled as Go comparisons.
	for method, fields := range memoryExclusion {
		body, ok := methods["MemoryProposals"][method]
		if !ok {
			t.Errorf("MemoryProposals has no %s — the two stores cannot be compared", method)
			continue
		}
		read := selectorNamesIn(body)
		var missing []string
		for _, f := range fields {
			if !read[f] {
				missing = append(missing, f)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			t.Errorf("MemoryProposals.%s does not read %v.\n\n"+
				"A fix spelled only in SQL leaves this seam accepting what Postgres refuses — and "+
				"this seam is what every service-level test in services/oms/internal/order runs "+
				"on, so the suite would certify the defect. The two stores are two implementations "+
				"of one contract and must refuse the same things.", method, missing)
		}
	}
}

// proposalReceiverName renders a method receiver's type name, pointer or not.
func proposalReceiverName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.StarExpr:
		return proposalReceiverName(v.X)
	case *ast.Ident:
		return v.Name
	default:
		return ""
	}
}

// proposalSQLIn returns every string literal in a function body — where the
// SQL lives.
func proposalSQLIn(body *ast.BlockStmt) []string {
	var out []string
	ast.Inspect(body, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			out = append(out, lit.Value)
		}
		return true
	})
	return out
}

// selectorNamesIn returns the set of field/method names selected in a body, so
// `p.ExpiryAnnouncedAt` registers as "ExpiryAnnouncedAt".
func selectorNamesIn(body *ast.BlockStmt) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			out[sel.Sel.Name] = true
		}
		return true
	})
	return out
}
