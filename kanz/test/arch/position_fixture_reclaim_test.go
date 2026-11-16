package arch

import (
	"crypto/sha256"
	"fmt"
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

// The inventory follows test-local helpers and function literals, including
// actual Go Projector/relay calls and the Rust request builders. It deliberately
// does not claim a proof of arbitrary Go dataflow or exact subject equality.
// The twice-reused canonical broker and sentinel oracle prove those boundaries.
// A registration hidden in a branch or placed after publication is insufficient.
type positionFixtureFunction struct {
	file *ast.File
	decl *ast.FuncDecl
	path string
}

type positionFixtureProgram struct {
	fset                             *token.FileSet
	files                            map[string]*ast.File
	functions                        map[string][]positionFixtureFunction
	foreignRecoveryHasNoPositionEdge bool
}

func positionFixtureImports(file *ast.File) map[string]string {
	result := map[string]string{}
	for _, item := range file.Imports {
		path, _ := strconv.Unquote(item.Path.Value)
		name := filepath.Base(path)
		if path == "github.com/nats-io/nats.go" {
			name = "nats"
		}
		if item.Name != nil {
			name = item.Name.Name
		}
		result[name] = path
	}
	return result
}

func positionFixtureCallName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}

func positionFixtureCandidate(file *ast.File) bool {
	position, broker, writer := false, false, false
	imports := positionFixtureImports(file)
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.BasicLit:
			if n.Kind == token.STRING {
				value, _ := strconv.Unquote(n.Value)
				position = position || strings.HasPrefix(value, "risk.position.")
			}
		case *ast.SelectorExpr:
			position = position || n.Sel.Name == "PositionFor" || n.Sel.Name == "VenuePositionFor" ||
				n.Sel.Name == "PositionChanged" || n.Sel.Name == "VenuePositionChanged" || n.Sel.Name == "ReclaimOMSPositions"
		case *ast.CallExpr:
			name := positionFixtureCallName(n)
			writer = writer || name == "Publish" || name == "NewProjector" || name == "runResult" || name == "Process" || name == "HandleRecovery" || name == "Run"
			if selector, ok := n.Fun.(*ast.SelectorExpr); ok {
				if alias, ok := selector.X.(*ast.Ident); ok {
					path := imports[alias.Name]
					broker = broker || (name == "Connect" && path == "github.com/nats-io/nats.go") ||
						(name == "DialNATS" && path == "github.com/eighred/kanz/pkg/bus")
				}
			}
		}
		return true
	})
	return position && broker && writer
}

func (p *positionFixtureProgram) add(path string, file *ast.File) {
	p.files[path] = file
	for _, declaration := range file.Decls {
		if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Body != nil {
			key := filepath.Dir(path) + "/" + fn.Name.Name
			p.functions[key] = append(p.functions[key], positionFixtureFunction{file, fn, path})
		}
	}
}

type positionFixtureFlow struct{ reclaimed, live bool }

func (p *positionFixtureProgram) nativeMode(expr ast.Expr, owner positionFixtureFunction, depth int, values map[string]ast.Expr) string {
	if depth == 0 {
		return ""
	}
	switch value := expr.(type) {
	case *ast.BasicLit:
		if value.Kind == token.STRING {
			result, _ := strconv.Unquote(value.Value)
			return result
		}
	case *ast.BinaryExpr:
		if value.Op == token.ADD {
			left, right := p.nativeMode(value.X, owner, depth-1, values), p.nativeMode(value.Y, owner, depth-1, values)
			if left != "" && right != "" {
				return left + right
			}
		}
	case *ast.Ident:
		if expr := values[value.Name]; expr != nil {
			return p.nativeMode(expr, owner, depth-1, values)
		}
		var result string
		ast.Inspect(owner.decl.Body, func(node ast.Node) bool {
			if assign, ok := node.(*ast.AssignStmt); ok {
				for i, lhs := range assign.Lhs {
					if name, ok := lhs.(*ast.Ident); ok && name.Name == value.Name && i < len(assign.Rhs) {
						result = p.nativeMode(assign.Rhs[i], owner, depth-1, values)
					}
				}
			}
			return true
		})
		return result
	case *ast.CompositeLit:
		for _, element := range value.Elts {
			if pair, ok := element.(*ast.KeyValueExpr); ok {
				if key, ok := pair.Key.(*ast.BasicLit); ok && key.Value == `"mode"` {
					return p.nativeMode(pair.Value, owner, depth-1, values)
				}
				if key, ok := pair.Key.(*ast.Ident); ok && key.Name == "URL" {
					return p.nativeMode(pair.Value, owner, depth-1, values)
				}
			}
		}
	case *ast.CallExpr:
		if selector, ok := value.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Getenv" && len(value.Args) == 1 {
			if alias, ok := selector.X.(*ast.Ident); ok && positionFixtureImports(owner.file)[alias.Name] == "os" {
				return p.nativeMode(value.Args[0], owner, depth-1, values)
			}
		}
		functions := p.functions[filepath.Dir(owner.path)+"/"+positionFixtureCallName(value)]
		if len(functions) == 1 {
			fn := functions[0]
			parameters := map[string]ast.Expr{}
			index := 0
			for _, field := range fn.decl.Type.Params.List {
				for _, name := range field.Names {
					if index < len(value.Args) {
						parameters[name.Name] = value.Args[index]
					}
					index++
				}
			}
			var result string
			ast.Inspect(fn.decl.Body, func(node ast.Node) bool {
				if ret, ok := node.(*ast.ReturnStmt); ok && len(ret.Results) == 1 {
					result = p.nativeMode(ret.Results[0], fn, depth-1, parameters)
				}
				return true
			})
			return result
		}
	}
	return ""
}

// checkBody interprets only ordering relevant to the fixture boundary. Unknown
// external calls do not assert registration; known publication calls fail closed.
func (p *positionFixtureProgram) checkBody(body *ast.BlockStmt, owner positionFixtureFunction,
	state positionFixtureFlow, stack map[*ast.FuncDecl]bool, problems *[]string) positionFixtureFlow {
	closures := map[string]*ast.FuncLit{}
	var expression func(ast.Expr, positionFixtureFlow) positionFixtureFlow
	var statement func(ast.Stmt, positionFixtureFlow) positionFixtureFlow
	expression = func(expr ast.Expr, current positionFixtureFlow) positionFixtureFlow {
		if expr == nil || !current.live {
			return current
		}
		switch value := expr.(type) {
		case *ast.CallExpr:
			name := positionFixtureCallName(value)
			if name == "Cleanup" {
				// Cleanup callbacks execute later in reverse registration order.
				// Check deferred publication against this registration-time state;
				// callback registration never blesses the enclosing live body.
				for _, arg := range value.Args {
					if fn, ok := arg.(*ast.FuncLit); ok {
						p.checkBody(fn.Body, owner, current, stack, problems)
					}
				}
				return current
			}
			for _, arg := range value.Args {
				if _, literal := arg.(*ast.FuncLit); !literal {
					current = expression(arg, current)
				}
			}
			if selector, ok := value.Fun.(*ast.SelectorExpr); ok && (name == "ReclaimSubjects" || name == "ReclaimOMSPositions") {
				if alias, ok := selector.X.(*ast.Ident); ok &&
					((name == "ReclaimSubjects" && positionFixtureImports(owner.file)[alias.Name] == "github.com/eighred/kanz/internal/bustest") || (name == "ReclaimOMSPositions" && positionFixtureImports(owner.file)[alias.Name] == "github.com/eighred/kanz/internal/bustest/positionfixture")) {
					current.reclaimed = true
					return current
				}
			}
			isSubtest := false
			if selector, ok := value.Fun.(*ast.SelectorExpr); ok && name == "Run" {
				if receiver, ok := selector.X.(*ast.Ident); ok && receiver.Name == "t" {
					isSubtest = true
				}
			}
			if isSubtest {
				for _, arg := range value.Args {
					if fn, ok := arg.(*ast.FuncLit); ok {
						p.checkBody(fn.Body, owner, current, stack, problems)
					}
				}
				return current // child cleanup does not register a parent cleanup
			}
			if name == "Fatal" || name == "Fatalf" || name == "Skip" || name == "Skipf" || name == "FailNow" {
				current.live = false
				return current
			}
			if name == "runResult" || name == "run" {
				if len(value.Args) >= 2 {
					mode := p.nativeMode(value.Args[1], owner, 12, nil)
					// The Rust fixture's SQL-only modes enqueue but do not publish.
					// Unknown modes are never treated as evidence of no effect.
					if mode == "position-apply" || mode == "position-snapshot" {
						return current
					}
				}
			}
			if name == "Publish" || name == "Flush" || name == "Handle" || name == "runResult" || name == "run" || name == "Process" || name == "Emit" || name == "HandleRecovery" || name == "Run" {
				if !current.reclaimed {
					*problems = append(*problems, fmt.Sprintf("%s:%d %s reaches %s before owned POSITION reclaim",
						owner.path, p.fset.Position(value.Pos()).Line, owner.decl.Name.Name, name))
				}
				return current
			}
			if fn := closures[name]; fn != nil {
				result := p.checkBody(fn.Body, owner, current, stack, problems)
				current.reclaimed = result.reclaimed
				return current
			}
			key := filepath.Dir(owner.path) + "/" + name
			functions := p.functions[key]
			// An exported selector on an unresolved receiver is not a call to
			// a same-package test adapter merely because the names match (for
			// example HTTP Header.Get versus rustProposals.Get). Actual publish
			// surfaces were handled above; only test-local private methods bind.
			if _, selector := value.Fun.(*ast.SelectorExpr); selector && ast.IsExported(name) {
				functions = nil
			}
			if len(functions) == 1 && !stack[functions[0].decl] {
				next := functions[0]
				stack[next.decl] = true
				result := p.checkBody(next.decl.Body, next, current, stack, problems)
				delete(stack, next.decl)
				current.reclaimed = result.reclaimed
			}
			if fn, ok := value.Fun.(*ast.FuncLit); ok {
				result := p.checkBody(fn.Body, owner, current, stack, problems)
				current.reclaimed = result.reclaimed
			}
		case *ast.UnaryExpr:
			current = expression(value.X, current)
		case *ast.BinaryExpr:
			current = expression(value.X, current)
			if value.Op == token.LAND || value.Op == token.LOR {
				expression(value.Y, current) // possible effects; RHS registration is conditional
			} else {
				current = expression(value.Y, current)
			}
		case *ast.ParenExpr:
			current = expression(value.X, current)
		}
		return current
	}
	statement = func(stmt ast.Stmt, current positionFixtureFlow) positionFixtureFlow {
		if !current.live {
			return current
		}
		switch value := stmt.(type) {
		case *ast.ExprStmt:
			return expression(value.X, current)
		case *ast.AssignStmt:
			for i, expr := range value.Rhs {
				if fn, ok := expr.(*ast.FuncLit); ok && i < len(value.Lhs) {
					if name, ok := value.Lhs[i].(*ast.Ident); ok {
						closures[name.Name] = fn
					}
				} else {
					current = expression(expr, current)
				}
			}
		case *ast.DeclStmt:
			if declaration, ok := value.Decl.(*ast.GenDecl); ok {
				for _, spec := range declaration.Specs {
					if variable, ok := spec.(*ast.ValueSpec); ok {
						for i, expr := range variable.Values {
							if fn, ok := expr.(*ast.FuncLit); ok && i < len(variable.Names) {
								closures[variable.Names[i].Name] = fn
							} else {
								current = expression(expr, current)
							}
						}
					}
				}
			}
		case *ast.IfStmt:
			if value.Init != nil {
				current = statement(value.Init, current)
			}
			current = expression(value.Cond, current)
			left := p.checkBody(value.Body, owner, current, stack, problems)
			right := current
			if value.Else != nil {
				right = statement(value.Else, current)
			}
			switch {
			case !left.live:
				return right
			case !right.live:
				return left
			default:
				current.reclaimed = left.reclaimed && right.reclaimed
			}
		case *ast.BlockStmt:
			return p.checkBody(value, owner, current, stack, problems)
		case *ast.RangeStmt:
			current = expression(value.X, current)
			p.checkBody(value.Body, owner, current, stack, problems)
		case *ast.ForStmt:
			if value.Init != nil {
				current = statement(value.Init, current)
			}
			current = expression(value.Cond, current)
			p.checkBody(value.Body, owner, current, stack, problems)
			if value.Post != nil {
				statement(value.Post, current)
			}
		case *ast.GoStmt:
			expression(value.Call, current)
		case *ast.DeferStmt: // publication from a deferred callback still needs registration now
			expression(value.Call, current)
		case *ast.ReturnStmt:
			for _, expr := range value.Results {
				current = expression(expr, current)
			}
			current.live = false
		case *ast.SwitchStmt:
			if value.Init != nil {
				current = statement(value.Init, current)
			}
			current = expression(value.Tag, current)
			for _, clause := range value.Body.List {
				if c, ok := clause.(*ast.CaseClause); ok {
					for _, expr := range c.List {
						expression(expr, current)
					} // possible case effects, never mandatory cleanup
					p.checkBody(&ast.BlockStmt{List: c.Body}, owner, current, stack, problems)
				}
			}
		}
		return current
	}
	for _, stmt := range body.List {
		state = statement(stmt, state)
	}
	return state
}

func (p *positionFixtureProgram) check() (map[string]bool, []string) {
	inventory, problems := map[string]bool{}, []string{}
	for path, file := range p.files {
		if p.foreignRecoveryHasNoPositionEdge && (strings.HasPrefix(path, "services/oms/tools/recovery_conformance/") || path == "services/accounting/internal/consume/recovery_integration_test.go") {
			continue
		}
		candidate := positionFixtureCandidate(file)
		// New fixture files may reach the broker/subject builders through a
		// helper in another test file. Do not require Connect in the caller.
		for _, declaration := range file.Decls {
			if fn, ok := declaration.(*ast.FuncDecl); ok && strings.HasPrefix(fn.Name.Name, "Test") {
				candidate = candidate || p.reachesPositionFixture(positionFixtureFunction{file, fn, path}, map[*ast.FuncDecl]bool{})
			}
		}
		if !candidate {
			continue
		}
		inventory[path] = true
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			owner := positionFixtureFunction{file, fn, path}
			if !p.reachesPositionFixture(owner, map[*ast.FuncDecl]bool{}) {
				continue
			}
			p.checkBody(fn.Body, owner, positionFixtureFlow{live: true}, map[*ast.FuncDecl]bool{fn: true}, &problems)
		}
	}
	sort.Strings(problems)
	return inventory, problems
}

func (p *positionFixtureProgram) reachesPositionFixture(owner positionFixtureFunction, seen map[*ast.FuncDecl]bool) bool {
	position, broker, writer := false, false, false
	var visit func(positionFixtureFunction)
	visit = func(fn positionFixtureFunction) {
		if seen[fn.decl] || fn.decl.Body == nil {
			return
		}
		seen[fn.decl] = true
		imports := positionFixtureImports(fn.file)
		ast.Inspect(fn.decl.Body, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.BasicLit:
				if n.Kind == token.STRING {
					value, _ := strconv.Unquote(n.Value)
					position = position || strings.HasPrefix(value, "risk.position.")
				}
			case *ast.SelectorExpr:
				position = position || n.Sel.Name == "PositionFor" || n.Sel.Name == "VenuePositionFor" || n.Sel.Name == "PositionChanged" || n.Sel.Name == "VenuePositionChanged" || n.Sel.Name == "ReclaimOMSPositions" || n.Sel.Name == "OrderFilled" || n.Sel.Name == "OrderPartiallyFilled" || n.Sel.Name == "ExecutionRecovered"
			case *ast.CallExpr:
				name := positionFixtureCallName(n)
				writer = writer || name == "Publish" || name == "NewProjector" || name == "runResult" || name == "Process" || name == "Emit" || name == "HandleRecovery" || name == "Run"
				if selector, ok := n.Fun.(*ast.SelectorExpr); ok {
					if alias, ok := selector.X.(*ast.Ident); ok {
						primary := true
						if name == "Connect" && len(n.Args) > 0 {
							primary = p.nativeMode(n.Args[0], fn, 12, nil) != "TEST_TV_NATS_URL"
						}
						if name == "DialNATS" && len(n.Args) > 1 {
							primary = p.nativeMode(n.Args[1], fn, 12, nil) != "TEST_TV_NATS_URL"
						}
						broker = broker || primary && ((name == "Connect" && imports[alias.Name] == "github.com/nats-io/nats.go") || (name == "DialNATS" && imports[alias.Name] == "github.com/eighred/kanz/pkg/bus"))
					}
					if ast.IsExported(name) {
						return true
					}
				}
				callees := p.functions[filepath.Dir(fn.path)+"/"+name]
				if len(callees) == 1 {
					visit(callees[0])
				}
			}
			return true
		})
	}
	visit(owner)
	return position && broker && writer
}

func positionForeignRecoveryInputPaths(repo string) ([]string, error) {
	checkPath := func(relative string, directory bool) error {
		path := repo
		parts := strings.Split(filepath.ToSlash(relative), "/")
		for i, part := range parts {
			if part == "" || part == "." || part == ".." {
				return fmt.Errorf("invalid recovery input path")
			}
			path = filepath.Join(path, part)
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			if i < len(parts)-1 || directory {
				if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
					return fmt.Errorf("recovery input directory is not ordinary")
				}
			} else if !info.Mode().IsRegular() {
				return fmt.Errorf("recovery input is not regular")
			}
		}
		return nil
	}
	paths := []string{"kanz/internal/fillfact/recovery.go", "kanz/services/oms/internal/position/projector.go", "kanz/services/oms/cmd/oms/main.go", "kanz/services/oms/internal/config/config.go", "kanz/services/accounting/internal/consume/recovery_integration_test.go"}
	for _, owner := range []struct{ dir, suffix string }{{"kanz/services/oms/tools/recovery_conformance", ".go"}, {"rust/crates/oms_recovery/src", ".rs"}} {
		if err := checkPath(owner.dir, true); err != nil {
			return nil, err
		}
		if err := filepath.WalkDir(filepath.Join(repo, owner.dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("linked recovery input")
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("nonregular recovery input")
			}
			if !strings.HasSuffix(path, owner.suffix) {
				return nil
			}
			relative, err := filepath.Rel(repo, path)
			if err != nil {
				return err
			}
			paths = append(paths, filepath.ToSlash(relative))
			return nil
		}); err != nil {
			return nil, err
		}
	}
	sort.Strings(paths)
	for i, path := range paths {
		if i > 0 && path == paths[i-1] {
			return nil, fmt.Errorf("duplicate recovery input")
		}
		if err := checkPath(path, false); err != nil {
			return nil, err
		}
	}
	return paths, nil
}

// #1014: this existing owner publishes only strict foreign-tenant recovery
// facts; the system OMS refuses those before its book. This is a reviewed
// absence of a runtime edge, not an exemption for a tools directory. Bind every
// current caller, native producer and admission/configuration source byte.
// #1597 adds sub-eight-place recovery assertions; #1610 observes startup and
// process-join outcomes in deadline/worker controls. Those reviewed changes keep
// the foreign-tenant admission premise; every remaining input is unchanged.
// Inventory every Go source file in the recovery owner, not only tests: a new
// helper, linked input or changed byte removes this classification for review.
func positionForeignRecoveryHasNoPositionEdge(t *testing.T, root string) bool {
	t.Helper()
	repo := filepath.Join(root, "..")
	paths, err := positionForeignRecoveryInputPaths(repo)
	if err != nil {
		return false
	}
	h := sha256.New()
	for _, path := range paths {
		raw, err := os.ReadFile(filepath.Join(repo, path))
		if err != nil {
			return false
		}
		_, _ = h.Write([]byte(path + "\x00"))
		_, _ = h.Write([]byte(strings.ReplaceAll(string(raw), "\r\n", "\n")))
		_, _ = h.Write([]byte{0})
	}
	// Independently reconstructed from canonical Git LF blobs: sorted path,
	// NUL, normalized source bytes, NUL; all Go files still total 38 inputs.
	if len(paths) != 38 || fmt.Sprintf("%x", h.Sum(nil)) != "910810599648278c6071d8da90428199ed6a6451693e8a81d748b7e352dc7b4c" {
		return false
	}
	workflow, err := os.ReadFile(filepath.Join(repo, ".github/workflows/kanz-ci.yml"))
	if err != nil {
		return false
	}
	start := strings.SplitN(string(workflow), "      - name: Start the OMS on the bus (for the M1 loop certification)", 2)
	if len(start) != 2 {
		return false
	}
	step := strings.SplitN(start[1], "      - name:", 2)[0]
	if strings.Count(step, "OMS_TENANT=__system__") != 1 || !strings.Contains(step, "/tmp/oms > /tmp/oms.log") {
		return false
	}
	t.Log("foreign recovery owners have no POSITION admission edge: 38 exact reviewed source files, exact recovery tenancy, system fixture")
	return true
}

func TestPositionForeignRecoveryClassificationBindsNonTestHelpers(t *testing.T) {
	source := filepath.Join(moduleRoot(t), "..")
	paths, err := positionForeignRecoveryInputPaths(source)
	if err != nil || len(paths) != 38 {
		t.Fatalf("reviewed recovery input inventory: %d %v", len(paths), err)
	}
	repo := t.TempDir()
	for _, path := range append(paths, ".github/workflows/kanz-ci.yml") {
		raw, err := os.ReadFile(filepath.Join(source, path))
		if err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(repo, path)
		if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination, raw, 0644); err != nil {
			t.Fatal(err)
		}
	}
	root := filepath.Join(repo, "kanz")
	if !positionForeignRecoveryHasNoPositionEdge(t, root) {
		t.Fatal("unchanged 38-input positive refused")
	}
	const helper = "kanz/services/oms/tools/recovery_conformance/unreviewed_helper.go"
	if err := os.WriteFile(filepath.Join(repo, helper), []byte("package recoveryconformance\nfunc unreviewedHelper() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	changed, err := positionForeignRecoveryInputPaths(repo)
	helperIndex := sort.SearchStrings(changed, helper)
	if err != nil || len(changed) != 39 || helperIndex == len(changed) || changed[helperIndex] != helper {
		t.Fatal("non-test helper did not enter the complete input inventory")
	}
	if positionForeignRecoveryHasNoPositionEdge(t, root) {
		t.Fatal("unreviewed non-POSITION helper retained the foreign classification")
	}
	if err := os.Remove(filepath.Join(repo, helper)); err != nil {
		t.Fatal(err)
	}
	if !positionForeignRecoveryHasNoPositionEdge(t, root) {
		t.Fatal("restored 38-input positive refused")
	}
	if err := os.Symlink("recovery_test.go", filepath.Join(repo, helper)); err != nil {
		t.Fatal(err)
	}
	if _, err := positionForeignRecoveryInputPaths(repo); err == nil || positionForeignRecoveryHasNoPositionEdge(t, root) {
		t.Fatal("linked recovery input retained the foreign classification")
	}
}

// The one cross-process fixture has a different owner: its seed receipt survives
// two go-test processes and its controller performs exact cleanup after proof.
// This declaration is source-specific, not an exemption for arbitrary tools.
func TestPositionFixtureControllerDeclaresSeparateLifecycle(t *testing.T) {
	root := moduleRoot(t)
	controllers := 0
	err := filepath.WalkDir(filepath.Join(root, "test/backing"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		if !positionFixtureCandidate(file) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if filepath.ToSlash(rel) != "test/backing/position_reclaim/main.go" {
			t.Errorf("undeclared cross-process POSITION publisher: %s", rel)
			return nil
		}
		controllers++
		publish, purge, exact, read, write := 0, 0, 0, 0, 0
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch positionFixtureCallName(call) {
			case "Publish":
				publish++
			case "Purge":
				purge++
			case "WithPurgeSubject":
				if len(call.Args) == 1 {
					if arg, ok := call.Args[0].(*ast.SelectorExpr); ok && arg.Sel.Name == "SentinelSubject" {
						exact++
					}
				}
			case "readReceipt":
				read++
			case "writeReceipt":
				write++
			}
			return true
		})
		if publish != 1 || purge != 1 || exact != 1 || read != 1 || write != 1 {
			t.Errorf("controller seed/read/snapshot/exact-cleanup lifecycle drifted: %d/%d/%d/%d/%d", publish, purge, exact, read, write)
		}
		return nil
	})
	if err != nil || controllers != 1 {
		t.Fatalf("controller declaration non-vacuity: %d %v", controllers, err)
	}
	// The runner must bind host Docker/start identity separately from the
	// controller's actual server/stream and framed-byte identities.
	proof, err := os.ReadFile(filepath.Join(root, "../.github/scripts/position_reclaim_proof.py"))
	if err != nil || !strings.Contains(string(proof), "stable_broker(self.before,docker_identity())") {
		t.Fatal("controller lost parent Docker identity proof")
	}
}

func TestPositionPublishersRegisterOwnedCleanup(t *testing.T) {
	root := moduleRoot(t)
	p := &positionFixtureProgram{fset: token.NewFileSet(), files: map[string]*ast.File{}, functions: map[string][]positionFixtureFunction{}}
	p.foreignRecoveryHasNoPositionEdge = positionForeignRecoveryHasNoPositionEdge(t, root)
	for _, dir := range []string{"internal", "services", "pkg", "cmd", "tools", "test/load"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(p.fset, path, nil, 0)
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			p.add(filepath.ToSlash(relative), file)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	inventory, problems := p.check()
	// Non-vacuity binds the scanner to the independently reviewed direct and
	// indirect writer surfaces. Removing/moving one requires an explicit review.
	required := []string{
		"internal/bustest/reclaim_broker_test.go", "internal/platform/subject/compaction_integration_test.go",
		"internal/risk/state/state_time_integration_test.go", "services/compliance/internal/monitor/arming_test.go",
		"services/compliance/internal/monitor/position_identity_integration_test.go", "services/compliance/internal/monitor/position_time_integration_test.go",
		"services/compliance/cmd/compliance/lifecycle_integration_test.go", "services/compliance/cmd/compliance/reevaluate_snapshot_integration_test.go",
		"services/oms/internal/position/publish_order_integration_test.go", "services/oms/internal/position/recovery_integration_test.go",
		"services/oms/tools/store_conformance/position_test.go", "services/oms/tools/store_conformance/projector_test.go",
		"services/webhook_ingest/internal/ingest/positions_nats_test.go", "services/webhook_ingest/internal/ingest/positions_unknown_nats_test.go",
		"services/webhook_ingest/internal/ingest/integration_test.go", "services/webhook_ingest/internal/ingest/large_notional_integration_test.go", "services/webhook_ingest/internal/ingest/domain_boundary_integration_test.go",
		"services/venue_binance/internal/binance/binance_overfill_broker_test.go", "services/venue_binance/internal/binance/execution_price_domain_test.go", "services/venue_okx/internal/okx/execution_price_domain_test.go",
		"services/oms/internal/order/recovery_service_test.go",
	}
	known := map[string]bool{}
	for _, path := range required {
		known[path] = true
		if !inventory[path] {
			t.Errorf("POSITION publisher inventory lost %s", path)
		}
	}
	for path := range inventory {
		if !known[path] {
			t.Logf("additional resolved POSITION candidate: %s", path)
		}
	}
	for _, problem := range problems {
		t.Error(problem)
	}
	// Removing the source qualification must re-enter default-deny for those
	// actual callers, rather than silently treating their directory as safe.
	if p.foreignRecoveryHasNoPositionEdge {
		p.foreignRecoveryHasNoPositionEdge = false
		_, unqualified := p.check()
		p.foreignRecoveryHasNoPositionEdge = true
		foreign, accounting := false, false
		for _, problem := range unqualified {
			foreign = foreign || strings.Contains(problem, "services/oms/tools/recovery_conformance/")
			accounting = accounting || strings.Contains(problem, "services/accounting/internal/consume/recovery_integration_test.go")
		}
		if !foreign || !accounting {
			t.Error("foreign-recovery classification became vacuous or bypassed default-deny")
		}
	}
	t.Logf("reviewed %d AST-discovered real-broker POSITION fixture files", len(inventory))
	// No runtime may acquire the privileged testing helper merely because the
	// helper itself is an ordinary .go package file.
	for _, dir := range []string{"internal", "services", "pkg", "cmd"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				t.Error(err)
				return nil
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				t.Error(err)
				return nil
			}
			for _, imported := range positionFixtureImports(file) {
				if imported == "github.com/eighred/kanz/internal/bustest" || imported == "github.com/eighred/kanz/internal/bustest/positionfixture" {
					relative, _ := filepath.Rel(root, path)
					// This one declared test-support dependency avoids the outbox
					// in-package-test cycle. The complete compiled graph separately
					// rejects every ordinary importer outside these two helper owners.
					if filepath.ToSlash(relative) == "internal/bustest/positionfixture/receipts.go" && imported == "github.com/eighred/kanz/internal/bustest" {
						continue
					}
					t.Errorf("runtime imports test cleanup: %s", path)
				}
			}
			return nil
		})
	}
}

func TestPositionPublisherGuardRejectsMissingLateAndConditionalReclaim(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"before", `b.ReclaimSubjects(t, js, topic); producer.Publish(ctx, event)`, false},
		{"missing", `producer.Publish(ctx, event)`, true},
		{"late", `producer.Publish(ctx, event); b.ReclaimSubjects(t, js, topic)`, true},
		{"conditional", `if optional { b.ReclaimSubjects(t, js, topic) }; producer.Publish(ctx, event)`, true},
		{"closure-before", `publish := func(){producer.Publish(ctx,event)}; b.ReclaimSubjects(t,js,topic); publish()`, false},
		{"closure-late", `publish := func(){producer.Publish(ctx,event)}; publish(); b.ReclaimSubjects(t,js,topic)`, true},
		{"closure-return", `setup := func() int {return 1}; _ = setup(); producer.Publish(ctx,event)`, true},
		{"short-circuit-and", `setup:=func()bool{b.ReclaimSubjects(t,js,topic);return true}; if optional && setup(){}; producer.Publish(ctx,event)`, true},
		{"short-circuit-or", `setup:=func()bool{b.ReclaimSubjects(t,js,topic);return true}; if optional || setup(){}; producer.Publish(ctx,event)`, true},
		{"var-initializer", `var outcome=producer.Publish(ctx,event); _=outcome`, true},
		{"for-condition", `for producer.Publish(ctx,event) {break}`, true},
		{"for-post", `for ; optional; producer.Publish(ctx,event) {}`, true},
		{"native-unknown", `runResult(t, map[string]any{"mode":"new-position-mode"}); producer.Publish(ctx,event)`, true},
		{"child-owner-before", `p.ReclaimOMSPositions(t,js,"fund","instrument","venue"); producer.Publish(ctx,event)`, false},
		{"child-owner-late", `producer.Publish(ctx,event); p.ReclaimOMSPositions(t,js,"fund","instrument","venue")`, true},
		{"wrong-reclaim-owner", `b.ReclaimOMSPositions(t,js,"fund","instrument","venue"); producer.Publish(ctx,event)`, true},
		{"cleanup-publish-before-reclaim", `t.Cleanup(func(){producer.Publish(ctx,event)}); b.ReclaimSubjects(t,js,topic)`, true},
		{"cleanup-publish-after-reclaim", `b.ReclaimSubjects(t,js,topic); t.Cleanup(func(){producer.Publish(ctx,event)})`, false},
		{"cleanup-does-not-register-parent", `t.Cleanup(func(){b.ReclaimSubjects(t,js,topic)}); producer.Publish(ctx,event)`, true},
		{"switch-initializer", `switch producer.Publish(ctx,event); value {}`, true},
		{"switch-tag", `switch producer.Publish(ctx,event) {}`, true},
		{"switch-case-expression", `switch {case producer.Publish(ctx,event):}`, true},
		{"switch-case-cleanup-is-conditional", `setup:=func()bool{b.ReclaimSubjects(t,js,topic);return true}; switch {case setup():}; producer.Publish(ctx,event)`, true},
		{"switch-body-cleanup-is-conditional", `switch {case optional: b.ReclaimSubjects(t,js,topic)}; producer.Publish(ctx,event)`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &positionFixtureProgram{fset: token.NewFileSet(), files: map[string]*ast.File{}, functions: map[string][]positionFixtureFunction{}}
			source := `package fixture; import b "github.com/eighred/kanz/internal/bustest"; import p "github.com/eighred/kanz/internal/bustest/positionfixture"; import n "github.com/nats-io/nats.go"; func TestFixture(t *testing.T) {n.Connect(url); topic := "risk.position.changed.t.p.i"; ` + tc.body + `}`
			file, err := parser.ParseFile(p.fset, "fixture_test.go", source, 0)
			if err != nil {
				t.Fatal(err)
			}
			p.add("fixture_test.go", file)
			inventory, problems := p.check()
			if len(inventory) != 1 || (len(problems) != 0) != tc.want {
				t.Fatalf("inventory=%v problems=%v", inventory, problems)
			}
		})
	}
}
