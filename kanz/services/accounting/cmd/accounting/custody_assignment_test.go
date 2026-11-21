package main

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/custody"
)

func TestCustodyStartupRequiresIndependentEvidenceForDeclaredAccounts(t *testing.T) {
	cfg := baseCfg()
	ctx := bus.WithTenantID(t.Context(), cfg.Tenant)
	cfg.CustodyAccounts = "PF1:CUST-A:arbitrary-id"
	for _, store := range []custody.Store{nil, custody.NewMemoryStore()} {
		cc, err := admittedCustodyConfig(ctx, cfg, store, time.Now())
		if cc.scope != nil || !errors.Is(err, custody.ErrUnprovenAssignment) || !strings.Contains(err.Error(), "custody account assignment admission failed") {
			t.Fatalf("declared account accepted without a proof authority: %+v %v", cc, err)
		}
	}
	cfg.CustodyAccounts = ""
	cfg.CustodyPairs = nil
	if cc, err := admittedCustodyConfig(ctx, cfg, nil, time.Now()); err != nil || cc.scope == nil {
		t.Fatalf("non-custody startup depends on assignment proof: %v", err)
	}
}

// Both the brokerless HTTP route and the scheduler are built by run. Keep the
// declaration-only geometry constructor out of the production composition root.
func TestStartupCannotUseDeclarationOnlyCustodyConfig(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	gotAdmitted := false
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				name, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				if name.Name == "buildCustodyConfig" && fn.Name.Name != "admittedCustodyConfig" {
					t.Errorf("%s calls declaration-only geometry in a runtime path", fn.Name.Name)
				}
				if fn.Name.Name == "run" && name.Name == "admittedCustodyConfig" {
					gotAdmitted = true
				}
				return true
			})
		}
	}
	if !gotAdmitted {
		t.Fatal("accounting startup omitted custody assignment admission")
	}
}
