package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// A row owner and a relay owner must name the service that commits the domain
// state. A copied accounting owner in OMS would otherwise strand or misattribute
// a valid FACT while every SQL ownership predicate still worked (#1325).
func TestOutboxOwnersFollowServiceBoundaries(t *testing.T) {
	count := 0
	walkGoFiles(t, moduleRoot(t), "services", token.NewFileSet(), func(rel string, f *ast.File) {
		if strings.HasSuffix(rel, "_test.go") {
			return
		}
		n, problems := outboxOwnerProblems(rel, f)
		count += n
		for _, problem := range problems {
			t.Error(problem)
		}
	})
	if count < 20 {
		t.Fatalf("found only %d outbox owner declarations; expected writers and relays in three services", count)
	}
}

func outboxOwnerProblems(rel string, f *ast.File) (count int, problems []string) {
	alias := importAlias(f, outboxImportPath)
	if alias == "" {
		return 0, nil
	}
	want := ""
	for service, symbol := range map[string]string{
		"oms": "ServiceOMS", "accounting": "ServiceAccounting", "datamaster": "ServiceDatamaster", "lake-sink": "ServiceLakeSink",
	} {
		if strings.HasPrefix(rel, "services/"+service+"/") {
			want = symbol
		}
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
		count++
		if want == "" || len(call.Args) <= arg || !isQualified(call.Args[arg], alias, want) {
			problems = append(problems, fmt.Sprintf("%s: %s must use its owning service constant %s.%s", rel, sel.Sel.Name, alias, want))
		}
		return true
	})
	return count, problems
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
