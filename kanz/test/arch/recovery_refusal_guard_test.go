package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestRecoveryRefusalGuardRequiresBlockInTheActualErrorBranch(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`next, err := ApplyFill(st, fill, now); if err != nil { if e := p.BlockRecovery(ctx,c,st,"reason",now); e != nil { return e }; return err }; return nil`, true},
		{`next, err := ApplyFill(st, fill, now); if err != nil { return err }; p.BlockRecovery(ctx,c,st,"reason",now); return nil`, false},
		{`next, err := ApplyFill(st, fill, now); if other != nil { p.BlockRecovery(ctx,c,st,"reason",now) }; return nil`, false},
		{`next, err := ApplyFill(st, fill, now); if err != nil { logger.Error("blocked") }; return nil`, false},
	} {
		f, err := parser.ParseFile(token.NewFileSet(), "fixture.go", "package fixture; func apply(){"+tc.body+"}", 0)
		if err != nil {
			t.Fatal(err)
		}
		fn := f.Decls[0].(*ast.FuncDecl)
		handled, found := applyFillRefusalHandled(fn.Body)
		if !found || handled != tc.want {
			t.Fatalf("handled=%v found=%v body=%s", handled, found, tc.body)
		}
	}
}
