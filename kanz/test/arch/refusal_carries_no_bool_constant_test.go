package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// #1084: every production if is a conservative superset of guarded refusals.
// Deriving this set avoids a second list of gates and catches void helpers,
// wrappers, methods and closures without guessing which calls return errors.
// The matcher covers literal/package-local named Boolean composition, not all
// Go constant expressions, predicate correctness or runtime reachability. Plain
// Boolean arguments/comparisons remain valid; their nested logical operators
// are still checked. Imported constants and arithmetic folding are outside it.
type refusalCondition struct {
	path, function string
	start, end     int
	expression     ast.Expr
	scope          *refusalScope
}

type refusalInspection struct {
	files      int
	conditions []refusalCondition
	violations []string
}

type refusalScanner struct {
	set    *token.FileSet
	result refusalInspection
}

// Only lexical binding is needed: a variable named false must not become a
// literal. This does not infer types or calls. Visibility starts after a local
// declaration, while package declarations are visible across their files.
type refusalBinding struct {
	value   ast.Expr
	scope   *refusalScope
	visible token.Pos
}

type refusalScope struct {
	parent   *refusalScope
	bindings map[string][]refusalBinding
}

func newRefusalScope(parent *refusalScope) *refusalScope {
	return &refusalScope{parent: parent, bindings: map[string][]refusalBinding{}}
}

func (s *refusalScope) bind(name string, value ast.Expr, visibility token.Pos, expressionScope *refusalScope) {
	s.bindings[name] = append(s.bindings[name], refusalBinding{value: value, scope: expressionScope, visible: visibility})
}

func (s *refusalScope) lookup(name string, position token.Pos) (refusalBinding, bool) {
	for scope := s; scope != nil; scope = scope.parent {
		bindings := scope.bindings[name]
		for i := len(bindings) - 1; i >= 0; i-- {
			if bindings[i].visible == token.NoPos || position >= bindings[i].visible {
				return bindings[i], true
			}
		}
	}
	return refusalBinding{}, false
}

func inspectRefusalConstants(sources map[string]string) (refusalInspection, error) {
	s := &refusalScanner{set: token.NewFileSet()}
	paths := make([]string, 0, len(sources))
	for path := range sources {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	files := map[string]*ast.File{}
	packages := map[string]*refusalScope{}
	fileScopes := map[string]*refusalScope{}
	for _, path := range paths {
		f, err := parser.ParseFile(s.set, path, sources[path], parser.SkipObjectResolution)
		if err != nil {
			return s.result, err
		}
		files[path] = f
		s.result.files++
		pkg := refusalPackage(path, f)
		if packages[pkg] == nil {
			packages[pkg] = newRefusalScope(nil)
		}
		fileScope := newRefusalScope(packages[pkg])
		fileScopes[path] = fileScope
		for _, imp := range f.Imports {
			if imp.Name != nil {
				fileScope.bind(imp.Name.Name, nil, token.NoPos, fileScope)
			}
		}
		for _, decl := range f.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Recv == nil {
					packages[pkg].bind(decl.Name.Name, nil, token.NoPos, fileScope)
				}
			case *ast.GenDecl:
				refusalDeclarations(decl, packages[pkg], fileScope, true)
			}
		}
	}
	for _, path := range paths {
		if f := files[path]; f != nil {
			ast.Walk(refusalVisitor{scanner: s, path: path, scope: fileScopes[path], fileScope: fileScopes[path]}, f)
		}
	}
	for _, condition := range s.result.conditions {
		if s.constantLogic(condition.scope, condition.expression, true) {
			s.result.violations = append(s.result.violations, fmt.Sprintf("%s: %s: boolean constant in refusal condition", s.set.Position(condition.expression.Pos()), condition.function))
		}
	}
	if len(s.result.conditions) == 0 {
		return s.result, fmt.Errorf("derived no production if conditions from %d source files; refusal guard is vacuous", s.result.files)
	}
	sort.Strings(s.result.violations)
	return s.result, nil
}

func refusalPackage(path string, f *ast.File) string {
	return filepath.ToSlash(filepath.Dir(path)) + ":" + f.Name.Name
}

type refusalVisitor struct {
	scanner          *refusalScanner
	path, function   string
	scope, fileScope *refusalScope
}

func (v refusalVisitor) Visit(n ast.Node) ast.Visitor {
	s := v.scanner
	switch n := n.(type) {
	case *ast.FuncDecl:
		v.scope = newRefusalScope(v.scope)
		refusalParameters(v.scope, n.Type)
		v.function = n.Name.Name
		if n.Recv != nil {
			v.function = refusalReceiverName(n.Recv.List[0].Type) + "." + n.Name.Name
			refusalFields(v.scope, n.Recv)
		}
	case *ast.FuncLit:
		v.scope = newRefusalScope(v.scope)
		refusalParameters(v.scope, n.Type)
		v.function = fmt.Sprintf("%s.func@%d", v.function, s.set.Position(n.Pos()).Line)
	case *ast.BlockStmt, *ast.ForStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.CaseClause, *ast.CommClause:
		v.scope = newRefusalScope(v.scope)
	case *ast.RangeStmt:
		v.scope = newRefusalScope(v.scope)
		if n.Tok == token.DEFINE {
			for _, expr := range []ast.Expr{n.Key, n.Value} {
				if name, ok := expr.(*ast.Ident); ok {
					v.scope.bind(name.Name, nil, n.X.End(), v.scope)
				}
			}
		}
	case *ast.GenDecl:
		if v.scope != v.fileScope {
			refusalDeclarations(n, v.scope, v.scope, false)
		}
	case *ast.AssignStmt:
		if n.Tok == token.DEFINE {
			for _, expr := range n.Lhs {
				if name, ok := expr.(*ast.Ident); ok {
					v.scope.bind(name.Name, nil, n.End(), v.scope)
				}
			}
		}
	case *ast.IfStmt:
		v.scope = newRefusalScope(v.scope)
		file := s.set.File(n.Cond.Pos())
		s.result.conditions = append(s.result.conditions, refusalCondition{path: v.path, function: v.function, start: file.Offset(n.Cond.Pos()), end: file.Offset(n.Cond.End()), expression: n.Cond, scope: v.scope})
	}
	return v
}

func refusalFields(scope *refusalScope, fields *ast.FieldList) {
	if fields != nil {
		for _, field := range fields.List {
			for _, name := range field.Names {
				scope.bind(name.Name, nil, token.NoPos, scope)
			}
		}
	}
}

func refusalParameters(scope *refusalScope, signature *ast.FuncType) {
	refusalFields(scope, signature.TypeParams)
	refusalFields(scope, signature.Params)
	refusalFields(scope, signature.Results)
}

func refusalDeclarations(decl *ast.GenDecl, target, expressionScope *refusalScope, global bool) {
	var previous []ast.Expr
	for _, spec := range decl.Specs {
		visible := spec.End()
		if global {
			visible = token.NoPos
		}
		switch spec := spec.(type) {
		case *ast.TypeSpec:
			target.bind(spec.Name.Name, nil, visible, expressionScope)
		case *ast.ValueSpec:
			if len(spec.Values) != 0 {
				previous = spec.Values
			}
			for i, name := range spec.Names {
				var value ast.Expr
				if decl.Tok == token.CONST && i < len(previous) {
					value = previous[i]
				}
				target.bind(name.Name, value, visible, expressionScope)
			}
		}
	}
}

func refusalReceiverName(expr ast.Expr) string {
	switch expr := expr.(type) {
	case *ast.Ident:
		return expr.Name
	case *ast.StarExpr:
		return refusalReceiverName(expr.X)
	case *ast.IndexExpr:
		return refusalReceiverName(expr.X)
	case *ast.IndexListExpr:
		return refusalReceiverName(expr.X)
	case *ast.ParenExpr:
		return refusalReceiverName(expr.X)
	}
	return "method"
}

func (s *refusalScanner) boolConstant(scope *refusalScope, expr ast.Expr, seen map[ast.Expr]bool) (bool, bool) {
	if seen[expr] {
		return false, false
	}
	seen[expr] = true
	defer delete(seen, expr)
	switch expr := expr.(type) {
	case *ast.ParenExpr:
		return s.boolConstant(scope, expr.X, seen)
	case *ast.Ident:
		if binding, bound := scope.lookup(expr.Name, expr.Pos()); bound {
			if binding.value != nil {
				return s.boolConstant(binding.scope, binding.value, seen)
			}
			return false, false // A variable named true/false is not a constant.
		}
		if expr.Name == "true" || expr.Name == "false" {
			return expr.Name == "true", true
		}
	case *ast.UnaryExpr:
		if expr.Op == token.NOT {
			value, ok := s.boolConstant(scope, expr.X, seen)
			return !value, ok
		}
	case *ast.CallExpr:
		if name, ok := expr.Fun.(*ast.Ident); ok && name.Name == "bool" && len(expr.Args) == 1 && !expr.Ellipsis.IsValid() {
			if _, shadowed := scope.lookup("bool", name.Pos()); !shadowed {
				return s.boolConstant(scope, expr.Args[0], seen)
			}
		}
	case *ast.BinaryExpr:
		left, leftOK := s.boolConstant(scope, expr.X, seen)
		right, rightOK := s.boolConstant(scope, expr.Y, seen)
		if expr.Op == token.LAND && ((leftOK && !left) || (rightOK && !right)) {
			return false, true
		}
		if expr.Op == token.LOR && ((leftOK && left) || (rightOK && right)) {
			return true, true
		}
		if leftOK && rightOK {
			switch expr.Op {
			case token.LAND:
				return left && right, true
			case token.LOR:
				return left || right, true
			case token.EQL:
				return left == right, true
			case token.NEQ:
				return left != right, true
			}
		}
	}
	return false, false
}

func (s *refusalScanner) constantLogic(scope *refusalScope, expr ast.Expr, logical bool) bool {
	if _, constant := s.boolConstant(scope, expr, map[ast.Expr]bool{}); logical && constant {
		return true
	}
	switch expr := expr.(type) {
	case *ast.ParenExpr:
		return s.constantLogic(scope, expr.X, logical)
	case *ast.UnaryExpr:
		return expr.Op == token.NOT && s.constantLogic(scope, expr.X, logical)
	case *ast.BinaryExpr:
		operandsAreLogic := expr.Op == token.LAND || expr.Op == token.LOR
		return s.constantLogic(scope, expr.X, operandsAreLogic) || s.constantLogic(scope, expr.Y, operandsAreLogic)
	case *ast.FuncLit:
		return false // Its own if statements are visited with callable attribution.
	}
	// Calls, indices, assertions and composite expressions can hide a logical
	// operand. A plain check(false) is an argument, while check(false && bad)
	// contains the same disabled logic as a top-level condition.
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if n == expr {
			return true
		}
		if child, ok := n.(ast.Expr); ok {
			found = s.constantLogic(scope, child, false) || found
			return false
		}
		return true
	})
	return found
}

func refusalEstateSources(t *testing.T) map[string]string {
	t.Helper()
	root := moduleRoot(t)
	// Git excludes ignored generated/build output and includes new nonignored Go
	// source. Do not walk into another checkout or the .claude worktree surface.
	command := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z", "--", "*.go")
	command.Dir = root
	raw, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]string{}
	for _, path := range strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00") {
		if !refusalProductionPath(path) {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		sources[path] = string(body)
	}
	return sources
}

func refusalProductionPath(path string) bool {
	if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".claude" || part == ".git" || part == "vendor" || part == "testdata" {
			return false
		}
	}
	return true
}

func TestRefusalCarriesNoBoolConstant(t *testing.T) {
	sources := refusalEstateSources(t)
	result, err := inspectRefusalConstants(sources)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.violations) != 0 {
		t.Fatalf("constant logic disables a derived refusal control:\n%s", strings.Join(result.violations, "\n"))
	}
	t.Logf("%d source files, %d derived production if conditions", result.files, len(result.conditions))
	// Mutate a real derived condition, then require the original file AND
	// callable in the diagnostic. The production source on disk is never changed.
	condition := result.conditions[0]
	for _, prefix := range []string{"false &&", "true ||"} {
		t.Run(prefix, func(t *testing.T) {
			mutant := make(map[string]string, len(sources))
			for path, body := range sources {
				mutant[path] = body
			}
			body := sources[condition.path]
			mutant[condition.path] = body[:condition.start] + prefix + " (" + body[condition.start:condition.end] + ")" + body[condition.end:]
			observed, err := inspectRefusalConstants(mutant)
			if err != nil {
				t.Fatal(err)
			}
			for _, violation := range observed.violations {
				if strings.Contains(violation, condition.path+":") && strings.Contains(violation, ": "+condition.function+":") {
					return
				}
			}
			t.Fatalf("%s escaped for %s:%s: %v", prefix, condition.path, condition.function, observed.violations)
		})
	}
}

func TestRefusalCarriesNoBoolConstantFixtures(t *testing.T) {
	for _, tc := range []struct {
		name, source, function string
		violation              bool
	}{
		{"error-result", `func admit(bad bool) error { if false && bad { return nil }; return nil }`, "admit", true},
		{"void-wrapper", `func wrapper(bad bool) { if true || bad { reject() } }; func reject() {}`, "wrapper", true},
		{"method", `type Gate struct{}; func (*Gate) Check(bad bool) { if bad && (false) { return } }`, "Gate.Check", true},
		{"generic-method", `type Gate[T any] struct{}; func (*Gate[T]) Check(bad bool) { if true || bad { return } }`, "Gate.Check", true},
		{"closure", `func outer(bad bool) { f := func() { if false && bad { return } }; f() }`, "outer.func@", true},
		{"nested-logic", `func admit(a, b bool) { if a || (b && !true) { return } }`, "admit", true},
		{"logic-in-comparison", `func admit(a, b bool) { if a == (false && b) { return } }`, "admit", true},
		{"constant-comparison", `func admit(bad bool) { if (false && bad) == false { return } }`, "admit", true},
		{"builtin-conversion", `func admit(bad bool) { if bool(false) && bad { return } }`, "admit", true},
		{"logic-in-argument", `func admit(bad bool, check func(bool) bool) { if check(false && bad) { return } }`, "admit", true},
		{"logic-in-index", `func admit(bad bool, allowed map[bool]bool) { if allowed[true || bad] { return } }`, "admit", true},
		{"named-constant", `const disabled = false; func admit(bad bool) { if disabled && bad { return } }`, "admit", true},
		{"local-constant", `func admit(bad bool) { const disabled = false; if disabled && bad { return } }`, "admit", true},
		{"inherited-constant", `const (first = false; disabled); func admit(bad bool) { if disabled && bad { return } }`, "admit", true},
		{"direct-constant", `func admit() { if true { return } }`, "admit", true},
		{"ordinary-condition", `func admit(bad bool) error { if bad { return nil }; return nil }`, "", false},
		{"boolean-comparison", `func admit(ready bool) { if ready == false { return } }`, "", false},
		{"boolean-argument", `func admit(check func(bool) bool) { if check(false) { return } }`, "", false},
		{"initializer-argument", `func admit(check func(bool) bool) { if ready := check(false); !ready { return } }`, "", false},
		{"shadowed-builtin", `func admit(false, bad bool) { if false && bad { return } }`, "", false},
		{"if-init-shadow", `func admit(bad bool) { if false := bad; false && bad { return } }`, "", false},
		{"loop-init-shadow", `func admit(bad bool) { for false := bad; bad; { if false && bad { return }; break } }`, "", false},
		{"range-shadow", `func admit(bad bool, flags map[bool]bool) { for false := range flags { if false && bad { return } } }`, "", false},
		{"named-result-shadow", `func admit(bad bool) (false bool) { if false && bad { return }; return }`, "", false},
		{"shadowed-conversion", `func admit(bool func(bool) bool) { if bool(false) { return } }`, "", false},
		{"selector", `type Flags struct { false bool }; func admit(f Flags, bad bool) { if f.false && bad { return } }`, "", false},
		{"closure-return", `func admit() { if func() bool { return false }() { return } }`, "", false},
		{"comment-and-string", "func admit(bad bool) { /* if false && bad */ _ = `if true || bad`; if bad { return } }", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := inspectRefusalConstants(map[string]string{"gate.go": "package gate\n" + tc.source})
			if err != nil {
				t.Fatal(err)
			}
			if got := len(result.violations) != 0; got != tc.violation {
				t.Fatalf("violation=%v, want %v: %v", got, tc.violation, result.violations)
			}
			if tc.violation && (!strings.Contains(result.violations[0], "gate.go:") || !strings.Contains(result.violations[0], tc.function)) {
				t.Fatalf("missing source/callable attribution: %v", result.violations)
			}
		})
	}
	t.Run("cross-file-constant", func(t *testing.T) {
		result, err := inspectRefusalConstants(map[string]string{
			"constants.go": "package gate; const disabled = false",
			"gate.go":      "package gate; func admit(bad bool) { if disabled && bad { return } }",
		})
		if err != nil || len(result.violations) != 1 {
			t.Fatalf("cross-file constant escaped: %v, %v", result.violations, err)
		}
	})
	t.Run("cross-file-shadow", func(t *testing.T) {
		result, err := inspectRefusalConstants(map[string]string{
			"binding.go": "package gate; var false bool",
			"gate.go":    "package gate; func admit(bad bool) { if false && bad { return } }",
		})
		if err != nil || len(result.violations) != 0 {
			t.Fatalf("variable misclassified as builtin: %v, %v", result.violations, err)
		}
	})
	t.Run("generated-comment-cannot-exempt-production", func(t *testing.T) {
		result, err := inspectRefusalConstants(map[string]string{
			"gate.go": "// Code generated by example. DO NOT EDIT.\npackage gate; func admit(bad bool) { if false && bad { return } }",
		})
		if err != nil || len(result.violations) != 1 || result.files != 1 {
			t.Fatalf("comment bypassed production inventory: %+v, %v", result, err)
		}
	})
	t.Run("local-visibility-and-block-end", func(t *testing.T) {
		result, err := inspectRefusalConstants(map[string]string{
			"gate.go": "package gate; func admit(bad bool) { if false && bad { return }; { false := bad; if false && bad { return } }; if false && bad { return }; false := bad; if false && bad { return } }",
		})
		if err != nil || len(result.violations) != 2 {
			t.Fatalf("declaration visibility changed builtin meaning: %v, %v", result.violations, err)
		}
	})
	for _, sources := range []map[string]string{nil, {"gate.go": "package gate; func admit() {}"}, {"gate.go": "package gate; func"}} {
		if result, err := inspectRefusalConstants(sources); err == nil {
			t.Fatalf("empty/malformed derivation accepted: %+v", result)
		}
	}
}

func TestRefusalCarriesNoBoolConstantInventory(t *testing.T) {
	for _, path := range []string{"pkg/gate.go", "pkg/testdata_helpers/gate.go", "pkg/generated.go", ".claudeish/gate.go"} {
		if !refusalProductionPath(path) {
			t.Errorf("production source omitted: %s", path)
		}
	}
	for _, path := range []string{"", "pkg/gate_test.go", ".claude/worktrees/copy/gate.go", ".git/copy.go", "vendor/dependency/gate.go", "pkg/testdata/mutant.go"} {
		if refusalProductionPath(path) {
			t.Errorf("non-production source included: %s", path)
		}
	}
}
