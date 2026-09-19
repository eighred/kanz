package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// #1040 chooses an operator-facing consumer, not financial-state mutation.
// This bounded catalog guards the two venue discrepancy subjects; it does not
// pretend to prove consumer coverage for every dynamically assembled subject.
func TestVenueDiscrepanciesReachOperatorEvidence(t *testing.T) {
	root := moduleRoot(t)
	parse := func(path string) *ast.File {
		t.Helper()
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, path), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	config := parse("services/audit/internal/config/config.go")
	var subjects []string
	ast.Inspect(config, func(n ast.Node) bool {
		v, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for _, name := range v.Names {
			if name.Name == "DefaultSubjects" {
				ast.Inspect(v, func(n ast.Node) bool {
					if b, ok := n.(*ast.BasicLit); ok && b.Kind == token.STRING {
						s, err := strconv.Unquote(b.Value)
						if err != nil {
							t.Fatal(err)
						}
						subjects = append(subjects, s)
					}
					return true
				})
			}
		}
		return true
	})
	var handled []string
	ast.Inspect(parse("services/audit/internal/audit/discrepancy.go"), func(n ast.Node) bool {
		if c, ok := n.(*ast.CaseClause); ok {
			for _, expr := range c.List {
				if b, ok := expr.(*ast.BasicLit); ok && b.Kind == token.STRING {
					s, _ := strconv.Unquote(b.Value)
					handled = append(handled, s)
				}
			}
		}
		return true
	})
	tenancy := filepath.Join(root, "infra/nats/tenancy.yaml")
	pub, sub := servicePublishPermissions(t, tenancy), serviceSubscribeAllow(t, tenancy)
	for _, constant := range []string{"SubjectStateHealed", "SubjectBalanceRecon"} {
		subject := subjectLiteral(t, filepath.Join(root, "internal/execution/exchange_common.go"), regexp.MustCompile(constant+`\s*=\s*"([^"]+)"`), constant)
		if !covered(subject, handled) || !covered(subject, subjects) {
			t.Errorf("%s has no configured typed operator evidence consumer", subject)
		}
		permission, exists := sub["spiffe://kanz.internal/ns/kanz-services/sa/audit"]
		if !exists || !covered(subject, permission.allow) || permDenies(subject, permission) {
			t.Errorf("audit cannot subscribe to %s", subject)
		}
		for _, venue := range []string{"venue-binance", "venue-okx"} {
			permission, exists := pub["spiffe://kanz.internal/ns/kanz-services/sa/"+venue]
			if !exists || !covered(subject, permission.allow) || permDenies(subject, permission) {
				t.Errorf("%s cannot publish %s", venue, subject)
			}
		}
	}
	hasCall := func(f *ast.File, receiver, method string) bool {
		found := false
		ast.Inspect(f, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if s, ok := c.Fun.(*ast.SelectorExpr); ok {
					if id, ok := s.X.(*ast.Ident); ok && id.Name == receiver && s.Sel.Name == method {
						found = true
					}
				}
			}
			return true
		})
		return found
	}
	main := parse("services/audit/cmd/audit/main.go")
	if !hasCall(main, "audit", "WithDiscrepancyObserver") {
		t.Error("audit projection outcome metric is not wired")
	}
	subscribed := false
	ast.Inspect(main, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok || len(c.Args) != 4 {
			return true
		}
		s, ok := c.Fun.(*ast.SelectorExpr)
		if !ok || s.Sel.Name != "Subscribe" {
			return true
		}
		h, ok := c.Args[3].(*ast.SelectorExpr)
		if !ok || h.Sel.Name != "Handle" {
			return true
		}
		id, ok := h.X.(*ast.Ident)
		if ok && id.Name == "projector" {
			subscribed = true
		}
		return true
	})
	if !subscribed {
		t.Error("audit projector has no live subscription")
	}
	for _, venue := range []string{"venue-binance", "venue-okx"} {
		if !hasCall(parse("services/"+venue+"/cmd/"+venue+"/main.go"), "execution", "NewDiscrepancyPublisher") {
			t.Errorf("%s lacks discrepancy publish outcomes", venue)
		}
	}
}
