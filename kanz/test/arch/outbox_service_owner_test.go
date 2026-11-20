package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"
)

const custodyBatchOutboxFile = "services/accounting/internal/custody/batch_postgres.go"

// A row owner and a relay owner must name the service that commits the domain
// state. A copied accounting owner in OMS would otherwise strand or misattribute
// a valid FACT while every SQL ownership predicate still worked (#1325).
func TestOutboxOwnersFollowServiceBoundaries(t *testing.T) {
	count := 0
	foundImporter := false
	walkGoFiles(t, moduleRoot(t), "services", token.NewFileSet(), func(rel string, f *ast.File) {
		if strings.HasSuffix(rel, "_test.go") {
			return
		}
		n, problems := outboxOwnerProblems(rel, f)
		if rel == custodyBatchOutboxFile {
			foundImporter = true
		}
		count += n
		for _, problem := range problems {
			t.Error(problem)
		}
	})
	if !foundImporter {
		t.Errorf("missing signed custody importer at %s", custodyBatchOutboxFile)
	}
	if count < 20 {
		t.Fatalf("found only %d outbox owner declarations; expected writers and relays in three services", count)
	}
}

func outboxOwnerProblems(rel string, f *ast.File) (count int, problems []string) {
	alias := importAlias(f, outboxImportPath)
	if alias == "." {
		return 0, []string{rel + ": outbox owners require a qualified import"}
	}
	if alias == "" && rel != custodyBatchOutboxFile {
		return 0, nil
	}
	want := ""
	for service, symbol := range map[string]string{
		"oms": "ServiceOMS", "accounting": "ServiceAccounting", "datamaster": "ServiceDatamaster", "lake-sink": "ServiceLakeSink",
	} {
		if strings.HasPrefix(rel, "services/"+sourceDirectoryName(service)+"/") {
			want = symbol
		}
	}
	batchSites := map[string]int{}
	ast.PreorderStack(f, nil, func(n ast.Node, stack []ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != alias {
			return true
		}
		var arg int
		switch sel.Sel.Name {
		case "Enqueue":
			arg = 2
		case "NewPostgres":
			arg = 1
		default:
			return true
		}
		call, ok := stack[len(stack)-1].(*ast.CallExpr)
		if !ok || call.Fun != sel {
			problems = append(problems, fmt.Sprintf("%s: %s requires a direct call with an explicit outbox owner", rel, sel.Sel.Name))
			return true
		}
		count++
		owner := want
		if rel == custodyBatchOutboxFile && custodyBatchOwnsCall(sel.Sel.Name, stack) {
			owner = "ServiceCustodyBatch"
			batchSites[sel.Sel.Name]++
		}
		if owner == "" || len(call.Args) <= arg || !isQualified(call.Args[arg], alias, owner) {
			problems = append(problems, fmt.Sprintf("%s: %s must use its owning service constant %s.%s", rel, sel.Sel.Name, alias, owner))
		}
		return true
	})
	if rel == custodyBatchOutboxFile {
		for _, method := range []string{"NewPostgres", "Enqueue"} {
			if batchSites[method] != 1 {
				problems = append(problems, fmt.Sprintf("%s: expected one signed batch %s site, found %d", rel, method, batchSites[method]))
			}
		}
	}
	return count, problems
}

// The import login's restrictive RLS policy admits only the custody-batch
// statement producer. Ordinary Accounting rows and relays have their own owner.
func custodyBatchOwnsCall(method string, stack []ast.Node) bool {
	for _, n := range slices.Backward(stack) {
		switch fn := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.FuncDecl:
			if method == "NewPostgres" {
				return fn.Recv == nil && fn.Name.Name == "NewBatchImporter"
			}
			if fn.Name.Name != "Import" || !receiverIs(fn, "BatchImporter") {
				return false
			}
			_, ptr := fn.Recv.List[0].Type.(*ast.StarExpr)
			return method == "Enqueue" && ptr
		}
	}
	return false
}

func TestOutboxOwnerGuardRejectsCopiedOrImplicitOwners(t *testing.T) {
	for _, tc := range []struct {
		call string
		bad  bool
	}{
		{`ob.Enqueue(ctx, tx, ob.ServiceOMS, fact)`, false},
		{`ob.NewPostgres(pool, ob.ServiceOMS)`, false},
		{`ob.Enqueue(ctx, tx, ob.ServiceAccounting, fact)`, true},
		{`ob.NewPostgres(pool, "oms")`, true},
		{`ob.Enqueue(ctx, tx, owner, fact)`, true},
	} {
		t.Run(tc.call, func(t *testing.T) {
			src := "package order\nimport ob \"" + outboxImportPath + "\"\nfunc write() { " + tc.call + " }"
			f, err := parser.ParseFile(token.NewFileSet(), "fixture.go", src, 0)
			if err != nil {
				t.Fatal(err)
			}
			count, problems := outboxOwnerProblems("services/oms/internal/order/fixture.go", f)
			if count != 1 || (len(problems) != 0) != tc.bad {
				t.Fatalf("count=%d problems=%v, want rejected=%v", count, problems, tc.bad)
			}
		})
	}
}

func TestOutboxOwnerGuardBindsTheSignedBatchImporter(t *testing.T) {
	const src = `package custody
import ob "github.com/eighred/kanz/internal/outbox"
func NewBatchImporter() { ob.NewPostgres(pool, ob.ServiceCustodyBatch) }
func (*BatchImporter) Import() { ob.Enqueue(ctx, tx, ob.ServiceCustodyBatch, fact) }
func (*Postgres) save() { ob.Enqueue(ctx, tx, ob.ServiceAccounting, fact) }
`
	for _, tc := range []struct {
		name, path, from, to string
	}{
		{name: "scoped owners"},
		{name: "copied to sibling file", path: "services/accounting/internal/custody/other.go"},
		{name: "copied to OMS", path: "services/oms/internal/order/other.go"},
		{name: "moved relay", from: "func NewBatchImporter", to: "func newRelay"},
		{name: "moved writer", from: "(*BatchImporter) Import", to: "(*BatchImporter) append"},
		{name: "wrong receiver", from: "(*BatchImporter) Import", to: "(*Postgres) Import"},
		{name: "wrong receiver form", from: "(*BatchImporter) Import", to: "(BatchImporter) Import"},
		{name: "cross-owner relay", from: "pool, ob.ServiceCustodyBatch", to: "pool, ob.ServiceAccounting"},
		{name: "cross-owner writer", from: "tx, ob.ServiceCustodyBatch", to: "tx, ob.ServiceAccounting"},
		{name: "cross-owner ordinary writer", from: "tx, ob.ServiceAccounting", to: "tx, ob.ServiceCustodyBatch"},
		{name: "implicit owner", from: "pool, ob.ServiceCustodyBatch", to: "pool, owner"},
		{name: "indirect call", from: "ob.Enqueue(ctx, tx, ob.ServiceCustodyBatch", to: "enqueue := ob.Enqueue; enqueue(ctx, tx, ob.ServiceCustodyBatch"},
		{name: "missing enqueue", from: "ob.Enqueue(ctx, tx, ob.ServiceCustodyBatch, fact)", to: "other()"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := custodyBatchOutboxFile
			if tc.path != "" {
				path = tc.path
			}
			fixture := src
			if tc.from != "" {
				if strings.Count(src, tc.from) != 1 {
					t.Fatalf("mutation must match exactly once: %q", tc.from)
				}
				fixture = strings.Replace(src, tc.from, tc.to, 1)
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, fixture, 0)
			if err != nil {
				t.Fatal(err)
			}
			n, problems := outboxOwnerProblems(path, f)
			if tc.path == "" && tc.from == "" {
				if n != 3 || len(problems) != 0 {
					t.Fatalf("valid owners: count=%d problems=%v", n, problems)
				}
			} else if len(problems) == 0 {
				t.Fatal("mutated owner or call site accepted")
			}
		})
	}
}
