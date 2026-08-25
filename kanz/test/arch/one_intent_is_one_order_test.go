package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// A MINTED ORDER ID MUST NEVER REACH THE BROKER AS THE DEDUP KEY (#723).
//
// POST /v1/orders mints an order id when the body carries none. That is a
// convenience, and it is safe exactly while the CLIENT has named the operation
// with an Idempotency-Key — then the value the broker collapses on is the
// header, and the minted id is merely the order's name.
//
// With no header the minted id became both, and it is new on every attempt. So
// the retry of an ambiguous timeout published a second SubmitOrder under a
// second Nats-Msg-Id, and three independent at-most-once layers missed for that
// one reason: JetStream's server-side dedup (pkg/bus/producer.go stamps the key
// as Nats-Msg-Id), the OMS consumer's dedup.Claim (pkg/bus/consumer.go), and the
// gateway's own middleware.Idempotency (which passes through when the header is
// absent). internal/execution then stamps order_id straight into the venue
// clOrdId, so one client intent became two live orders at the exchange.
//
// WHY A GUARD AND NOT A COMMENT. The refusal is one `if` on a branch that is
// itself conditional, and deleting it re-greens every unit test that supplies a
// key — which is most of them. The property is also an ORDERING one, invisible
// to any check that merely asks whether the refusal exists: a refusal placed
// after orderid.Mint still mints, and a refusal placed after publish has already
// placed the order it claims to refuse.
//
// # WHY THIS PARSES THE AST
//
// A guard that grepped this tree for "Idempotency-Key" would match the
// paragraphs above, the handler's own explanatory comment, and the header name
// in a dozen tests. The sibling tenant-scope guard was written after a
// Dockerfile guard was found passing on a COMMENT containing the flag it
// checked. So every check below resolves identifiers and compares positions.
const submitHandlerFile = "../../services/api-gateway/internal/orders/orders.go"

func TestSubmitRefusesAnOrderItCannotDedup(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, submitHandlerFile, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", submitHandlerFile, err)
	}

	submit := findMethod(file, "submit")
	if submit == nil {
		t.Fatalf("no method submit in %s — this guard has lost its subject and is proving nothing",
			submitHandlerFile)
	}

	// The branch under test is the one that mints: `if cmd.GetOrderId() == ""`.
	// Locating it by its CALL rather than by source text means a rename of the
	// variable cannot silently move the guard off the branch it guards.
	mintBranch := findMintBranch(submit)
	if mintBranch == nil {
		t.Fatal("submit no longer has a branch calling orderid.Mint. If minting was removed the " +
			"defect is gone and so is this guard — delete it. If it MOVED, the refusal must move " +
			"with it, and this guard must be retargeted.")
	}

	mintPos := callPos(mintBranch, "orderid", "Mint")
	refusePos := guardedRefusalPos(mintBranch, "refuseIfUnnameable")

	if refusePos == token.NoPos {
		t.Fatalf("%s: the mint branch does not refuse via `if h.refuseIfUnnameable(...) { return }`. "+
			"A submit with neither order_id nor Idempotency-Key cannot be made at-most-once, so "+
			"minting one publishes a fresh key on every retry and the venue receives one live "+
			"order per attempt (#723).", fset.Position(mintPos))
	}
	if refusePos > mintPos {
		t.Fatalf("%s: the refusal is at line %d but orderid.Mint is at line %d — a refusal that "+
			"runs after the mint does not prevent it. The order is the property (#723).",
			submitHandlerFile, fset.Position(refusePos).Line, fset.Position(mintPos).Line)
	}

	assertRefusalIsReal(t, fset, file)
}

// assertRefusalIsReal resolves what refuseIfUnnameable DOES, because a method of
// that name whose body is `return false` would satisfy every check above. An
// earlier draft of the sibling #721 guard accepted "calls PrincipalFromContext"
// as proof of tenant scoping and a mutation deleting the tenant survived it. A
// guard proves what it RESOLVED, not what its name claims.
func assertRefusalIsReal(t *testing.T, fset *token.FileSet, file *ast.File) {
	t.Helper()

	fn := findMethod(file, "refuseIfUnnameable")
	if fn == nil {
		t.Fatal("no method refuseIfUnnameable — the refusal above resolves to nothing")
	}

	var readsHeader, refuses400 bool
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		// r.Header.Get("Idempotency-Key") — the STRING is the point. A refusal
		// that reads some other header refuses the wrong thing.
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Get" {
			for _, arg := range call.Args {
				if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING &&
					lit.Value == `"Idempotency-Key"` {
					readsHeader = true
				}
			}
		}
		// writeError(w, http.StatusBadRequest, ...) — resolved as a selector on
		// http, so a local constant named StatusBadRequest cannot stand in.
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "writeError" {
			for _, arg := range call.Args {
				if sel, ok := arg.(*ast.SelectorExpr); ok && sel.Sel.Name == "StatusBadRequest" {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "http" {
						refuses400 = true
					}
				}
			}
		}
		return true
	})

	pos := fset.Position(fn.Pos())
	if !readsHeader {
		t.Errorf("%s: refuseIfUnnameable never reads the \"Idempotency-Key\" header, so it cannot "+
			"be deciding whether the client named the operation", pos)
	}
	if !refuses400 {
		t.Errorf("%s: refuseIfUnnameable never calls writeError with http.StatusBadRequest — it "+
			"reports a refusal it does not send, and the order is published anyway", pos)
	}
}

// findMethod returns the named func or method declaration, or nil.
func findMethod(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == name && fn.Body != nil {
			return fn
		}
	}
	return nil
}

// findMintBranch returns the innermost *ast.IfStmt in fn whose body calls
// orderid.Mint. Identified by the call, not by the condition's source text.
func findMintBranch(fn *ast.FuncDecl) *ast.IfStmt {
	var found *ast.IfStmt
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		if callPos(ifStmt, "orderid", "Mint") != token.NoPos {
			found = ifStmt
		}
		return true
	})
	return found
}

// callPos reports the position of a pkg.Fn call within n, or token.NoPos.
func callPos(n ast.Node, pkg, fn string) token.Pos {
	pos := token.NoPos
	ast.Inspect(n, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != fn {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == pkg {
			pos = call.Pos()
		}
		return true
	})
	return pos
}

// guardedRefusalPos finds `if <recv>.<method>(...) { ... return }` within n and
// returns its position. The RETURN is required: calling the refusal and then
// carrying on to publish is the defect with a reassuring line of code on top.
func guardedRefusalPos(n ast.Node, method string) token.Pos {
	pos := token.NoPos
	ast.Inspect(n, func(node ast.Node) bool {
		ifStmt, ok := node.(*ast.IfStmt)
		if !ok {
			return true
		}
		call, ok := ifStmt.Cond.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != method {
			return true
		}
		for _, stmt := range ifStmt.Body.List {
			if _, ok := stmt.(*ast.ReturnStmt); ok {
				pos = ifStmt.Pos()
			}
		}
		return true
	})
	return pos
}
