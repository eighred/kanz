package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// Bounded syntax ownership: the top-level pipe join must precede the one Wait.
// Runtime cancellation/drain behavior is independently exercised on Linux.
func lakeWorkerPipeOrder(source string) error {
	f, err := parser.ParseFile(token.NewFileSet(), "worker.go", source, 0)
	if err != nil {
		return err
	}
	var join, wait token.Pos
	waits, joins := 0, 0
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Invoke" || fn.Recv == nil || len(fn.Recv.List[0].Names) != 1 || fn.Recv.List[0].Names[0].Name != "op" {
			continue
		}
		for _, stmt := range fn.Body.List {
			a, ok := stmt.(*ast.AssignStmt)
			if ok && len(a.Lhs) == 1 && len(a.Rhs) == 1 && lakeNodeText(a.Lhs[0]) == "pipes" && lakeNodeText(a.Rhs[0]) == "joinWorkerPipes(workCtx, writeDone, errorDone)" {
				join = stmt.Pos()
				joins++
			}
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && lakeNodeText(call.Fun) == "cmd.Wait" {
				waits++
				wait = call.Pos()
			}
			return true
		})
	}
	if joins != 1 || waits != 1 || join >= wait {
		return fmt.Errorf("worker Wait must follow owned pipe join")
	}
	return nil
}

func TestLakeWorkerPipeJoinWiring(t *testing.T) {
	source := string(mustReadArchFile(t, filepath.Join(moduleRoot(t), "services/lake-sink/internal/snapshot/worker.go")))
	if err := lakeWorkerPipeOrder(source); err != nil {
		t.Fatal(err)
	}
	for _, change := range [][2]string{
		{"pipes := joinWorkerPipes(workCtx, writeDone, errorDone)", "pipes := workerPipeJoins{}"},
		{"joinWorkerPipes(workCtx, writeDone, errorDone)", "joinWorkerPipes(workCtx, writeDone, writeDone)"},
		{"pipes := joinWorkerPipes(workCtx, writeDone, errorDone)", "cmd.Wait(); pipes := joinWorkerPipes(workCtx, writeDone, errorDone)"},
	} {
		if !strings.Contains(source, change[0]) {
			t.Fatal("mutation anchor absent")
		}
		if lakeWorkerPipeOrder(strings.Replace(source, change[0], change[1], 1)) == nil {
			t.Fatal("pipe ordering weakening accepted")
		}
	}
}
