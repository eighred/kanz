package main

// These source recipes bind the composition to the owner-owned constructor,
// canonical envelope validation and stored Resource.Tenant. Runtime graph,
// governance and HTTP tests prove behavior; these checks do not authenticate a
// principal or prove a deployed network boundary.
import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/lineage/internal/server"
)

type lineageRecipe struct{ main, governor, harvest []byte }

func lineageSources(t *testing.T) lineageRecipe {
	t.Helper()
	read := func(path string) []byte {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	return lineageRecipe{main: read("main.go"),
		governor: read(filepath.Join("..", "..", "internal", "governance", "governance.go")),
		harvest:  read(filepath.Join("..", "..", "internal", "harvest", "harvest.go"))}
}
func lineageIdent(e ast.Expr, name string) bool { v, ok := e.(*ast.Ident); return ok && v.Name == name }
func lineageSelector(e ast.Expr, recv, name string) bool {
	v, ok := e.(*ast.SelectorExpr)
	return ok && lineageIdent(v.X, recv) && v.Sel.Name == name
}
func lineageCall(e ast.Expr, recv, name string, args int) bool {
	v, ok := e.(*ast.CallExpr)
	return ok && lineageSelector(v.Fun, recv, name) && len(v.Args) == args
}
func lineageErrorCondition(e ast.Expr) bool {
	v, ok := e.(*ast.BinaryExpr)
	return ok && v.Op == token.NEQ && lineageIdent(v.X, "err") && lineageIdent(v.Y, "nil")
}
func lineageFunction(f *ast.File, name string) *ast.FuncDecl {
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}
func lineageReturnTwo(stmt ast.Stmt) bool {
	r, ok := stmt.(*ast.ReturnStmt)
	if !ok || len(r.Results) != 1 {
		return false
	}
	v, ok := r.Results[0].(*ast.BasicLit)
	return ok && v.Kind == token.INT && v.Value == "2"
}
func lineageStoredTenant(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	return ok && lineageIdent(call.Fun, "string") && len(call.Args) == 1 && lineageCall(call.Args[0], "info", "Owner", 0)
}
func lineageEnvelopeOwner(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	return ok && lineageSelector(call.Fun, "graph", "Owner") && len(call.Args) == 1 && lineageCall(call.Args[0], "env", "GetTenantId", 0)
}
func lineageRecipeProblems(src lineageRecipe) []string {
	var problems []string
	parse := func(name string, source []byte) *ast.File {
		f, err := parser.ParseFile(token.NewFileSet(), name, source, parser.SkipObjectResolution)
		if err != nil {
			problems = append(problems, name+": parse refused")
		}
		return f
	}
	main, gov, harvest := parse("main", src.main), parse("governor", src.governor), parse("harvest", src.harvest)
	if main == nil || gov == nil || harvest == nil {
		return problems
	}
	constructors := 0
	ast.Inspect(main, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if lineageSelector(call.Fun, "graph", "WithCompleteHistory") {
			problems = append(problems, "production completeness declaration")
		}
		if lineageSelector(call.Fun, "graph", "NewMemory") {
			constructors++
			if len(call.Args) != 1 || !lineageSelector(call.Args[0], "cfg", "GraphLimits") {
				problems = append(problems, "global limits not bound to constructor")
			}
		}
		return true
	})
	if constructors != 1 {
		problems = append(problems, "expected exactly one graph constructor")
	}
	run := lineageFunction(main, "run")
	unwind := false
	if run != nil {
		for i, stmt := range run.Body.List {
			a, ok := stmt.(*ast.AssignStmt)
			if !ok || len(a.Lhs) != 2 || len(a.Rhs) != 1 || !lineageCall(a.Rhs[0], "graph", "NewMemory", 1) {
				continue
			}
			if !lineageIdent(a.Lhs[0], "g") || !lineageIdent(a.Lhs[1], "err") || i+1 >= len(run.Body.List) {
				continue
			}
			next, ok := run.Body.List[i+1].(*ast.IfStmt)
			if ok && next.Init == nil && next.Else == nil && lineageErrorCondition(next.Cond) && len(next.Body.List) > 0 && lineageReturnTwo(next.Body.List[len(next.Body.List)-1]) {
				unwind = true
			}
		}
	}
	if !unwind {
		problems = append(problems, "graph construction failure does not refuse startup")
	}

	harvestRun := lineageFunction(main, "runHarvest")
	consumerCount, boundSubscriptions := 0, 0
	if harvestRun != nil {
		ast.Inspect(harvestRun.Body, func(n ast.Node) bool {
			if a, ok := n.(*ast.AssignStmt); ok && len(a.Lhs) == 2 && len(a.Rhs) == 1 {
				if call, ok := a.Rhs[0].(*ast.CallExpr); ok && lineageSelector(call.Fun, "bus", "NewConsumer") {
					consumerCount++
					dlq := false
					for _, arg := range call.Args {
						if lineageCall(arg, "bus", "WithDLQ", 1) && lineageIdent(arg.(*ast.CallExpr).Args[0], "client") {
							dlq = true
						}
						if lineageCall(arg, "bus", "WithRetry", 1) {
							problems = append(problems, "harvest retry was added")
						}
					}
					if !lineageIdent(a.Lhs[0], "consumer") || len(call.Args) == 0 || !lineageIdent(call.Args[0], "client") || !dlq {
						problems = append(problems, "single consumer/DLQ binding changed")
					}
				}
			}
			if r, ok := n.(*ast.ReturnStmt); ok && len(r.Results) == 1 {
				if call, ok := r.Results[0].(*ast.CallExpr); ok && lineageIdent(call.Fun, "runHarvestSubscriptions") {
					boundSubscriptions++
					if len(call.Args) != 6 || !lineageIdent(call.Args[0], "ctx") || !lineageSelector(call.Args[1], "cfg", "Subjects") ||
						!lineageSelector(call.Args[2], "cfg", "ConsumerGroup") || !lineageSelector(call.Args[3], "consumer", "SubscribeReady") ||
						!lineageSelector(call.Args[4], "harvester", "Handle") || !lineageIdent(call.Args[5], "readiness") {
						problems = append(problems, "production readiness helper is not bound to real Consumer/Harvester")
					}
				}
			}
			return true
		})
	}
	if consumerCount != 1 || boundSubscriptions != 1 {
		problems = append(problems, "production harvest does not return the single owned readiness/join seam")
	}
	resourceCount := 0
	ast.Inspect(gov, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || !lineageSelector(lit.Type, "auth", "Resource") {
			return true
		}
		resourceCount++
		bound := false
		for _, elt := range lit.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok && lineageIdent(kv.Key, "Tenant") {
				bound = lineageStoredTenant(kv.Value)
			}
		}
		if !bound {
			problems = append(problems, "resource tenant is not the stored owner")
		}
		return true
	})
	if resourceCount != 1 {
		problems = append(problems, "expected one owned authorization resource")
	}
	access := lineageFunction(gov, "CheckAccess")
	ownerFirst := false
	if access != nil && len(access.Body.List) > 0 {
		if gate, ok := access.Body.List[0].(*ast.IfStmt); ok {
			if a, ok := gate.Init.(*ast.AssignStmt); ok && len(a.Rhs) == 1 && lineageCall(a.Rhs[0], "g", "CheckOwner", 2) {
				call := a.Rhs[0].(*ast.CallExpr)
				cond, not := gate.Cond.(*ast.UnaryExpr)
				ownerFirst = lineageIdent(call.Args[0], "p") && lineageIdent(call.Args[1], "info") && not && cond.Op == token.NOT && lineageSelector(cond.X, "d", "Allow")
			}
		}
	}
	if !ownerFirst {
		problems = append(problems, "ownership is not the first unconditional access gate")
	}

	observationCount := 0
	ast.Inspect(harvest, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "DatasetOf" {
				problems = append(problems, "separate parent lookup outside atomic Observe")
			}
		}
		lit, ok := n.(*ast.CompositeLit)
		if !ok || !lineageSelector(lit.Type, "graph", "Observation") {
			return true
		}
		observationCount++
		bound := false
		for _, elt := range lit.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok && lineageIdent(kv.Key, "Owner") {
				bound = lineageEnvelopeOwner(kv.Value)
			}
		}
		if !bound {
			problems = append(problems, "observation owner is not the validated envelope tenant")
		}
		return true
	})
	if observationCount != 1 {
		problems = append(problems, "expected one owned observation")
	}
	handle := lineageFunction(harvest, "Handle")
	validated := false
	if handle != nil && len(handle.Body.List) > 0 {
		if gate, ok := handle.Body.List[0].(*ast.IfStmt); ok && lineageErrorCondition(gate.Cond) {
			if a, ok := gate.Init.(*ast.AssignStmt); ok && len(a.Rhs) == 1 && lineageCall(a.Rhs[0], "bus", "Validate", 1) {
				call := a.Rhs[0].(*ast.CallExpr)
				if lineageIdent(call.Args[0], "env") && len(gate.Body.List) == 1 {
					if r, ok := gate.Body.List[0].(*ast.ReturnStmt); ok && len(r.Results) == 1 && lineageCall(r.Results[0], "bus", "Terminal", 1) {
						validated = true
					}
				}
			}
		}
	}
	if !validated {
		problems = append(problems, "direct harvest does not refuse canonical validation failure")
	}
	return problems
}

func TestEventIndexIsWiredBoundedAndNotDeclaredComplete(t *testing.T) {
	if problems := lineageRecipeProblems(lineageSources(t)); len(problems) > 0 {
		t.Fatalf("lineage recipe refused: %v", problems)
	}
}

// Mutate exact AST spans of the actual sources; formatting changes do not turn
// a supposed counterexample into an unchanged copy. This is source binding,
// independently paired with the real behavior tests, not runtime reproduction.
func lineageMutant(t *testing.T, src lineageRecipe, kind string) lineageRecipe {
	t.Helper()
	target := &src.main
	if kind == "resource-tenant" || kind == "owner-gate" {
		target = &src.governor
	}
	if kind == "envelope-owner" || kind == "validation" || kind == "parent-lookup" {
		target = &src.harvest
	}
	original := *target
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "actual", original, 0)
	if err != nil {
		t.Fatal(err)
	}
	var node ast.Node
	var replacement string
	if kind == "owner-gate" {
		fn := lineageFunction(f, "CheckAccess")
		node = fn.Body.List[0].(*ast.IfStmt).Cond
		replacement = "false"
	} else if kind == "parent-lookup" {
		fn := lineageFunction(f, "Handle")
		offset := fset.Position(fn.Body.Lbrace).Offset + 1
		line := "\n _, _ = h.graph.DatasetOf(graph.Owner(env.GetTenantId()), env.GetCausationId())\n"
		*target = append(append(append([]byte(nil), original[:offset]...), []byte(line)...), original[offset:]...)
		return src
	} else {
		ast.Inspect(f, func(n ast.Node) bool {
			if node != nil {
				return false
			}
			switch x := n.(type) {
			case *ast.CallExpr:
				if kind == "readiness-binding" && lineageIdent(x.Fun, "runHarvestSubscriptions") {
					node = x.Args[3]
					replacement = "harvestSubscribeReady(nil)"
				}
				if kind == "handler-binding" && lineageIdent(x.Fun, "runHarvestSubscriptions") {
					node = x.Args[4]
					replacement = "bus.EventHandler(nil)"
				}
				if lineageSelector(x.Fun, "graph", "NewMemory") {
					if kind == "constructor" {
						node = x.Args[0]
						replacement = "graph.Limits{}"
					}
					if kind == "complete" {
						node = x
						replacement = "graph.NewMemory(cfg.GraphLimits, graph.WithCompleteHistory(\"acme\"))"
					}
				}
				if kind == "validation" && lineageSelector(x.Fun, "bus", "Validate") {
					node = x
					replacement = "func(*envelopepb.Envelope) error { return nil }(env)"
				}
			case *ast.CompositeLit:
				for _, elt := range x.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if kind == "resource-tenant" && lineageSelector(x.Type, "auth", "Resource") && lineageIdent(kv.Key, "Tenant") {
						node = kv.Value
						replacement = "p.Tenant"
					}
					if kind == "envelope-owner" && lineageSelector(x.Type, "graph", "Observation") && lineageIdent(kv.Key, "Owner") {
						node = kv.Value
						replacement = "graph.Owner(bus.SystemTenant)"
					}
				}
			case *ast.IfStmt:
				if kind == "constructor-error" {
					owned := false
					ast.Inspect(x.Body, func(n ast.Node) bool {
						if v, ok := n.(*ast.BasicLit); ok && v.Value == "\"graph configuration refused\"" {
							owned = true
						}
						return true
					})
					if owned {
						node = x.Cond
						replacement = "false"
					}
				}
			}
			return true
		})
	}
	if node == nil {
		t.Fatalf("mutation target %s did not resolve", kind)
	}
	start, end := fset.Position(node.Pos()).Offset, fset.Position(node.End()).Offset
	*target = append(append(append([]byte(nil), original[:start]...), []byte(replacement)...), original[end:]...)
	return src
}
func TestLineageOwnershipRecipeRejectsActualSourceMutations(t *testing.T) {
	src := lineageSources(t)
	for _, kind := range []string{"constructor", "constructor-error", "complete", "resource-tenant", "owner-gate", "envelope-owner", "validation", "parent-lookup", "readiness-binding", "handler-binding"} {
		t.Run(kind, func(t *testing.T) {
			if problems := lineageRecipeProblems(lineageMutant(t, src, kind)); len(problems) == 0 {
				t.Fatal("actual-source mutant escaped the owning recipe")
			}
		})
	}
}

type harvestRegistration struct {
	subject string
	ready   func()
}

func waitHarvestRegistration(t *testing.T, entered <-chan harvestRegistration) harvestRegistration {
	t.Helper()
	select {
	case item := <-entered:
		return item
	case <-time.After(3 * time.Second):
		t.Fatal("owned subscription did not enter")
		return harvestRegistration{}
	}
}
func waitHarvestResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("owned subscriptions did not join")
		return nil
	}
}
func waitHarvestFalse(t *testing.T, r *server.Readiness) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for r.Ready() {
		select {
		case <-deadline.C:
			t.Fatal("closed gate remained ready")
		case <-time.After(time.Millisecond):
		}
	}
}
func TestHarvestReadinessWaitsForAllCallbacksAndJoins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	readiness := &server.Readiness{}
	readiness.Set(true)
	entered := make(chan harvestRegistration, 2)
	cancelSeen := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWorkers := func() { releaseOnce.Do(func() { close(release) }) }
	var finished atomic.Int32
	subscribe := func(ctx context.Context, subject, group string, h bus.EventHandler, ready func()) error {
		defer finished.Add(1)
		entered <- harvestRegistration{subject: subject, ready: ready}
		<-ctx.Done()
		cancelSeen <- struct{}{}
		<-release
		return ctx.Err()
	}
	result := make(chan error, 1)
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		releaseWorkers()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("owned readiness fixture did not join")
		}
	})
	go func() {
		defer close(done)
		result <- runHarvestSubscriptions(ctx, []string{"one", "two"}, "owned", subscribe, func(context.Context, *envelopepb.Envelope, []byte) error { return nil }, readiness)
	}()
	first, second := waitHarvestRegistration(t, entered), waitHarvestRegistration(t, entered)
	if readiness.Ready() {
		t.Fatal("goroutine launch was credited as registration")
	}
	first.ready()
	first.ready()
	if readiness.Ready() {
		t.Fatal("one/duplicate callback credited every subject")
	}
	second.ready()
	if !readiness.Ready() {
		t.Fatal("all actual readiness callbacks never became ready")
	}
	cancel()
	for i := 0; i < 2; i++ {
		select {
		case <-cancelSeen:
		case <-time.After(3 * time.Second):
			t.Fatal("owned worker did not observe cancellation")
		}
	}
	waitHarvestFalse(t, readiness)
	first.ready()
	second.ready()
	if readiness.Ready() {
		t.Fatal("late callbacks resurrected cancellation")
	}
	select {
	case <-result:
		t.Fatal("returned before held workers joined")
	default:
	}
	releaseWorkers()
	if err := waitHarvestResult(t, result); err != nil {
		t.Fatalf("normal parent cancellation became fatal: %v", err)
	}
	if finished.Load() != 2 {
		t.Fatal("helper returned without all owned worker completions")
	}
	first.ready()
	if readiness.Ready() {
		t.Fatal("post-return callback reopened closed gate")
	}
}
func TestHarvestFailureClosesBeforeJoinAndPreservesFirstError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	readiness := &server.Readiness{}
	entered := make(chan harvestRegistration, 2)
	fail := make(chan struct{})
	held := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWorkers := func() { releaseOnce.Do(func() { close(release) }) }
	firstErr := errors.New("owned first subscription failure")
	subscribe := func(ctx context.Context, subject, group string, h bus.EventHandler, ready func()) error {
		entered <- harvestRegistration{subject: subject, ready: ready}
		if subject == "one" {
			select {
			case <-fail:
				return firstErr
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		<-ctx.Done()
		close(held)
		<-release
		return errors.New("later cleanup failure")
	}
	result := make(chan error, 1)
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		releaseWorkers()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("owned failure fixture did not join")
		}
	})
	go func() {
		defer close(done)
		result <- runHarvestSubscriptions(ctx, []string{"one", "two"}, "owned", subscribe, func(context.Context, *envelopepb.Envelope, []byte) error { return nil }, readiness)
	}()
	first, second := waitHarvestRegistration(t, entered), waitHarvestRegistration(t, entered)
	first.ready()
	second.ready()
	if !readiness.Ready() {
		t.Fatal("positive registration control never became ready")
	}
	close(fail)
	select {
	case <-held:
	case <-time.After(3 * time.Second):
		t.Fatal("peer worker was not cancelled")
	}
	if readiness.Ready() {
		t.Fatal("failure remained ready while joining")
	}
	first.ready()
	second.ready()
	if readiness.Ready() {
		t.Fatal("failure gate reopened")
	}
	select {
	case <-result:
		t.Fatal("returned before peer joined")
	default:
	}
	releaseWorkers()
	if err := waitHarvestResult(t, result); !errors.Is(err, firstErr) {
		t.Fatalf("primary subscription failure lost: %v", err)
	}
}
func TestHarvestAlreadyCancelledStartsNoWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &server.Readiness{}
	r.Set(true)
	var calls atomic.Int32
	err := runHarvestSubscriptions(ctx, []string{"one"}, "owned", func(context.Context, string, string, bus.EventHandler, func()) error { calls.Add(1); return nil }, func(context.Context, *envelopepb.Envelope, []byte) error { return nil }, r)
	if err != nil || calls.Load() != 0 || r.Ready() {
		t.Fatal("cancelled parent started or qualified workers")
	}
}
func TestHarvestInvalidScopesRefuseBeforeWorkers(t *testing.T) {
	cases := []struct {
		name     string
		subjects []string
		group    string
	}{
		{"none", nil, "owned"}, {"empty", []string{""}, "owned"}, {"duplicate", []string{"one", "one"}, "owned"},
		{"whitespace", []string{" one"}, "owned"}, {"no-group", []string{"one"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			r := &server.Readiness{}
			err := runHarvestSubscriptions(context.Background(), tc.subjects, tc.group, func(context.Context, string, string, bus.EventHandler, func()) error { calls.Add(1); return nil }, func(context.Context, *envelopepb.Envelope, []byte) error { return nil }, r)
			if err == nil || calls.Load() != 0 || r.Ready() {
				t.Fatal("invalid scopes looked healthy or launched workers")
			}
		})
	}
}
func TestHarvestUnexpectedCompletionNeverQualifies(t *testing.T) {
	for _, returned := range []error{nil, context.Canceled} {
		r := &server.Readiness{}
		err := runHarvestSubscriptions(context.Background(), []string{"one"}, "owned", func(context.Context, string, string, bus.EventHandler, func()) error { return returned }, func(context.Context, *envelopepb.Envelope, []byte) error { return nil }, r)
		if err == nil || r.Ready() {
			t.Fatal("unexpected subscription completion became healthy")
		}
	}
}

func lineageCanonicalBootstrap(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "infra", "nats", "bootstrap-job.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(raw, []byte("  bootstrap.sh: |\n")) != 1 {
		t.Fatal("canonical bootstrap literal did not resolve uniquely")
	}
	inside := false
	var script strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "  bootstrap.sh: |" {
			inside = true
			continue
		}
		if !inside {
			continue
		}
		if line != "" && !strings.HasPrefix(line, "    ") {
			break
		}
		script.WriteString(strings.TrimPrefix(line, "    "))
		script.WriteByte('\n')
	}
	if script.Len() == 0 {
		t.Fatal("empty canonical bootstrap")
	}
	return script.String()
}
func lineageTenantExtractor(t *testing.T) (string, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "test", "mtls", "up.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	startToken := "TENANT_EXECUTION_AWK='\n"
	endToken := "\n'\nif ! awk \"$TENANT_EXECUTION_AWK\" bootstrap.sh > tenant-execution-bootstrap.sh; then"
	if strings.Count(source, startToken) != 1 || strings.Count(source, endToken) != 1 {
		t.Fatal("one fixed tenant extractor did not resolve")
	}
	start := strings.Index(source, startToken) + len(startToken)
	end := strings.Index(source[start:], endToken)
	if end < 0 {
		t.Fatal("tenant extractor terminator missing")
	}
	return source, source[start : start+end]
}

// This named buffer exposes only Write: promoted ReaderFrom must not bypass the
// output cap. The child is the already-owned awk parser, processing source data;
// these fixtures do not execute the extracted shell/bootstrap or Docker.
type lineageBoundedOutput struct {
	data  bytes.Buffer
	limit int
}

func (b *lineageBoundedOutput) Write(p []byte) (int, error) {
	left := b.limit - b.data.Len()
	if len(p) > left {
		if left > 0 {
			n, _ := b.data.Write(p[:left])
			return n, io.ErrShortBuffer
		}
		return 0, io.ErrShortBuffer
	}
	return b.data.Write(p)
}
func lineageRunExtractor(t *testing.T, program, input string) ([]byte, error) {
	t.Helper()
	if len(input) > 65536 {
		t.Fatal("source fixture exceeded its input bound")
	}
	awk, err := exec.LookPath("awk")
	if err != nil {
		t.Fatal("existing canonical awk prerequisite unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, awk, program)
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin"}
	cmd.Dir = t.TempDir()
	cmd.Stdin = strings.NewReader(input)
	out := &lineageBoundedOutput{limit: 65536}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatal("fixed source extractor exceeded its bounded context")
	}
	return append([]byte(nil), out.data.Bytes()...), err
}
func TestTenantExecutionExtractorUsesOnlyVerbatimCanonicalRecipe(t *testing.T) {
	_, program := lineageTenantExtractor(t)
	canonical := lineageCanonicalBootstrap(t)
	fnStart := strings.Index(canonical, "ensure_stream() {\n")
	if fnStart < 0 {
		t.Fatal("canonical ensure_stream definition missing")
	}
	fnEnd := strings.Index(canonical[fnStart:], "\n}\n")
	if fnEnd < 0 {
		t.Fatal("canonical ensure_stream closing line missing")
	}
	head := canonical[:fnStart+fnEnd+3]
	var execution string
	for _, line := range strings.Split(canonical, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "ensure_stream" && fields[1] == "EXECUTION" {
			if execution != "" {
				t.Fatal("canonical EXECUTION is not unique")
			}
			execution = line
		}
	}
	if execution == "" {
		t.Fatal("canonical EXECUTION missing")
	}
	expected := head + execution + "\n"
	got, err := lineageRunExtractor(t, program, canonical)
	if err != nil || !bytes.Equal(got, []byte(expected)) {
		t.Fatal("actual extractor did not preserve the selected canonical bytes")
	}
	fixtures := map[string]string{
		"missing-function":       strings.Replace(canonical, "ensure_stream() {", "absent_stream() {", 1),
		"duplicate-function":     canonical + canonical[fnStart:fnStart+fnEnd+3],
		"missing-execution":      strings.Replace(canonical, execution+"\n", "", 1),
		"duplicate-execution":    canonical + execution + "\n",
		"wrong-subjects":         strings.Replace(canonical, execution, strings.Replace(execution, "order.>", "order.bad.>", 1), 1),
		"extra-flags":            strings.Replace(canonical, execution, execution+" --replicas=2", 1),
		"wrong-replica-default":  strings.Replace(canonical, "NATS_REPLICAS:-3", "NATS_REPLICAS:-2", 1),
		"wrong-url-default":      strings.Replace(canonical, "nats://nats:4222", "nats://unexpected:4222", 1),
		"wrong-storage":          strings.Replace(canonical, "--storage=file --retention=limits", "--storage=memory --retention=limits", 1),
		"wrong-duplicate-window": strings.Replace(canonical, "--dupe-window=2m", "--dupe-window=1m", 1),
		"wrong-discard":          strings.Replace(canonical, "discard=--discard=old", "discard=--discard=new", 1),
		"unterminated-function":  canonical[:fnStart+fnEnd] + canonical[fnStart+fnEnd+2:],
	}
	for name, input := range fixtures {
		t.Run(name, func(t *testing.T) {
			if input == canonical {
				t.Fatal("source mutation did not change its premise")
			}
			if _, err := lineageRunExtractor(t, program, input); err == nil {
				t.Fatal("source extractor accepted missing/duplicate/drifted canonical recipe")
			}
		})
	}
}
func lineageTenantInvocationBound(source string) bool {
	start := strings.Index(source, "# --- 4a. Provision ONLY the canonical tenant EXECUTION stream")
	end := strings.Index(source, "# THE EXPECTED STREAMS ARE DERIVED FROM THE SCRIPT")
	if start < 0 || end <= start {
		return false
	}
	block := source[start:end]
	for _, required := range []string{
		"docker run --rm --network \"$NETWORK\"",
		"-e NATS_REPLICAS=1",
		"$OUT/tenantoms.pem", "$OUT/tenantoms.key", "$OUT/bundle.pem",
		"\"$NATS_BOX_IMAGE\" sh /scripts/tenant-execution-bootstrap.sh",
		"if ! awk \"$TENANT_EXECUTION_AWK\" bootstrap.sh > tenant-execution-bootstrap.sh; then",
		"test -s tenant-execution-bootstrap.sh",
	} {
		if strings.Count(block, required) != 1 {
			return false
		}
	}
	return !strings.Contains(block, "$OUT/bootstrap.pem") && !strings.Contains(block, "sh /scripts/bootstrap.sh")
}
func TestTenantExecutionFixtureKeepsExistingIdentityAndPinnedTooling(t *testing.T) {
	source, _ := lineageTenantExtractor(t)
	if !lineageTenantInvocationBound(source) {
		t.Fatal("tenant prerequisite changed fixture identity/tooling/source scope")
	}
	for _, tc := range []struct{ name, old, new string }{
		{"system-certificate", "$OUT/tenantoms.pem", "$OUT/bootstrap.pem"},
		{"system-key", "$OUT/tenantoms.key", "$OUT/bootstrap.key"},
		{"full-bootstrap", "\"$NATS_BOX_IMAGE\" sh /scripts/tenant-execution-bootstrap.sh", "\"$NATS_BOX_IMAGE\" sh /scripts/bootstrap.sh"},
		{"other-image", "\"$NATS_BOX_IMAGE\" sh /scripts/tenant-execution-bootstrap.sh", "natsio/nats-box:latest sh /scripts/tenant-execution-bootstrap.sh"},
		{"replicas", "-e NATS_REPLICAS=1", "-e NATS_REPLICAS=3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := strings.Index(source, "# --- 4a.")
			mutant := source[:start] + strings.Replace(source[start:], tc.old, tc.new, 1)
			if mutant == source || lineageTenantInvocationBound(mutant) {
				t.Fatal("fixture identity/tool/source mutation escaped")
			}
		})
	}
}
