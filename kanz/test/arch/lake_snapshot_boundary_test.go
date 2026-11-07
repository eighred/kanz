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

// This is bounded source wiring, not proof of runtime authorization or process
// confinement. Real gateway/PG/NATS/worker fixtures remain independently required.
func snapshotBoundaryWiring(files map[string]string) error {
	required := map[string][]string{
		"services/lake-sink/internal/config/config.go": {`if cfg.Snapshot != nil {`, `lifecycleTopic := topic.Qualify(cfg.Snapshot.Tenant, "lake.snapshot")`, `if !slices.Contains(cfg.Topics, lifecycleTopic) {`, `cfg.Topics = append(cfg.Topics, lifecycleTopic)`},
		"services/lake-sink/cmd/lake-sink/main.go": {
			"if cfg.Snapshot != nil {\n\t\tfileOptions = append(fileOptions, sink.EnableActiveRunCapture)\n\t}",
			`sink.NewFileSink(cfg.OutputDir, "", fileOptions...)`,
			"snapshot.Run(ctx, *cfg.Snapshot, mesh.Source, landing, cfg.OutputDir, logger, busMetrics, readiness.SetSnapshot)",
			"readiness.RequireSnapshot()",
		},
		"services/lake-sink/internal/snapshot/runtime.go": {
			"pg.NewTenantPool(startup, cfg.DatabaseURL, cfg.Tenant)",
			"DisableReconnect: true", "transport.AuthorizeServices(brokerID)",
			"client.RequireAccountIdentity(startup, cfg.NATSAccount, svid.ID.String())",
			"client.RequireDurableRetention(startup, snapshotprofile.Stream())",
			`client.RequireRegisteredDefaultDurable(startup, snapshotprofile.Name, "lake.snapshot.>", "archiver")`,
			"client.RequireCanonicalParking(startup, SubjectRequested, int64(snapshotprofile.Stream().MaxMsgSize))",
			"queue.PendingKeys(startup, 17)", "len(keys) > 16", "queue.PendingKeys(startup, 1)",
			"bus.WithRetainedRetries(SubjectRequested)", "worker.slot <- struct{}{}",
		},
		"services/lake-sink/internal/snapshot/grpc.go": {
			"transport.AuthorizeServices(gateway)", "grpc.UnaryInterceptor(handler.delegate)",
			"s.store.authorize(ctx, caller, action, id)", "context.AfterFunc(s.lifetime, cancel)", "grpc.WaitForHandlers(true)",
		},
		"services/api-gateway/internal/gateway/lake.go": {
			"len(p.Portfolios) != 0", "p.Tenant != h.lakeTenant", "Subject: p.Subject, Tenant: p.Tenant",
			"new(lakepb.CreateSnapshotInput)", "DiscardUnknown: false",
			`mux.Handle(authz.LakeSnapshotCreate, "POST /v1/lake/snapshots", h.createLakeSnapshot)`, `mux.Handle(authz.LakeSnapshotRead, "GET /v1/lake/snapshots/{id}", h.getLakeSnapshot)`, `mux.Handle(authz.LakeSnapshotRead, "POST /v1/lake/snapshots/{id}/query", h.queryLakeSnapshot)`,
		},
		"infra/nats/snapshot_candidate.go": {
			"//go:embed tenancy.yaml", "hex.EncodeToString(sum[:]) != baselineSHA256",
			"uniqueUsers(accounts)", "encoder.SetEscapeHTML(false)", "reflect.DeepEqual(document, roundtrip)",
		},
		"pkg/bus/nats.go": {"if cfg.DisableReconnect {\n\t\topts = append(opts, nats.NoReconnect())\n\t}"},
	}
	for path, markers := range required {
		for _, marker := range markers {
			if !strings.Contains(files[path], marker) {
				return fmt.Errorf("snapshot boundary missing in %s", path)
			}
		}
		if _, err := parser.ParseFile(token.NewFileSet(), path, files[path], 0); err != nil {
			return err
		}
	}
	// The current composition owns exactly one constructor and one conditional
	// options append. A second assignment cannot silently enable capture by default.
	f, _ := parser.ParseFile(token.NewFileSet(), "main.go", files["services/lake-sink/cmd/lake-sink/main.go"], 0)
	assignments := 0
	ast.Inspect(f, func(n ast.Node) bool {
		if a, ok := n.(*ast.AssignStmt); ok {
			for _, left := range a.Lhs {
				if id, ok := left.(*ast.Ident); ok && id.Name == "fileOptions" {
					assignments++
				}
			}
		}
		return true
	})
	if assignments != 1 {
		return fmt.Errorf("snapshot options ownership changed")
	}
	runtimeFile, _ := parser.ParseFile(token.NewFileSet(), "runtime.go", files["services/lake-sink/internal/snapshot/runtime.go"], 0)
	serveCalls, gatedServeCalls := 0, 0
	isServe := func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Serve" {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == "grpcServer"
	}
	ast.Inspect(runtimeFile, func(n ast.Node) bool {
		if isServe(n) {
			serveCalls++
		}
		clause, ok := n.(*ast.CommClause)
		if !ok {
			return true
		}
		expression, ok := clause.Comm.(*ast.ExprStmt)
		if !ok {
			return true
		}
		receive, ok := expression.X.(*ast.UnaryExpr)
		if !ok || receive.Op != token.ARROW {
			return true
		}
		channel, ok := receive.X.(*ast.Ident)
		if !ok || channel.Name != "initialReady" {
			return true
		}
		for _, statement := range clause.Body {
			guard, ok := statement.(*ast.IfStmt)
			if !ok || lakeNodeText(guard.Cond) != "!failed.Load() && ctx.Err() == nil" {
				continue
			}
			ast.Inspect(guard.Body, func(n ast.Node) bool {
				if isServe(n) {
					gatedServeCalls++
				}
				return true
			})
		}
		return true
	})
	if serveCalls != 1 || gatedServeCalls != 1 {
		return fmt.Errorf("snapshot RPC admission must follow initial replay under the live failure gate")
	}
	latched := false
	ast.Inspect(runtimeFile, func(n ast.Node) bool {
		a, ok := n.(*ast.AssignStmt)
		if !ok || len(a.Lhs) != 1 || len(a.Rhs) != 1 {
			return true
		}
		name, ok := a.Lhs[0].(*ast.Ident)
		if !ok || name.Name != "latch" {
			return true
		}
		fn, ok := a.Rhs[0].(*ast.FuncLit)
		if !ok || len(fn.Body.List) == 0 {
			return true
		}
		statement, ok := fn.Body.List[len(fn.Body.List)-1].(*ast.ExprStmt)
		if !ok {
			return true
		}
		call, ok := statement.X.(*ast.CallExpr)
		if !ok || len(call.Args) != 0 {
			return true
		}
		callee, ok := call.Fun.(*ast.Ident)
		latched = ok && callee.Name == "cancel"
		return true
	})
	if !latched {
		return fmt.Errorf("snapshot failure must cancel owner admission, not only readiness")
	}
	return nil
}

func TestLakeSnapshotBoundaryWiring(t *testing.T) {
	paths := []string{"services/lake-sink/cmd/lake-sink/main.go", "services/lake-sink/internal/snapshot/runtime.go", "services/lake-sink/internal/snapshot/grpc.go", "services/api-gateway/internal/gateway/lake.go", "infra/nats/snapshot_candidate.go", "pkg/bus/nats.go", "services/lake-sink/internal/config/config.go"}
	files := map[string]string{}
	for _, path := range paths {
		files[path] = string(mustReadArchFile(t, filepath.Join(moduleRoot(t), path)))
	}
	if err := snapshotBoundaryWiring(files); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []struct{ path, old, replacement string }{
		{paths[6], `if cfg.Snapshot != nil {`, `if true {`},
		{paths[6], `topic.Qualify(cfg.Snapshot.Tenant, "lake.snapshot")`, `"lake.snapshot"`},
		{paths[6], `topic.Qualify(cfg.Snapshot.Tenant, "lake.snapshot")`, `topic.Qualify("__system__", "lake.snapshot")`},
		{paths[6], `if !slices.Contains(cfg.Topics, lifecycleTopic) {`, `if true {`},
		{paths[6], `cfg.Topics = append(cfg.Topics, lifecycleTopic)`, `cfg.Topics = cfg.Topics`},
		{paths[0], "if cfg.Snapshot != nil {\n\t\tfileOptions", "if true {\n\t\tfileOptions"},
		{paths[0], "readiness.RequireSnapshot()", "readiness.SetSnapshot(true)"},
		{paths[1], "DisableReconnect: true", "DisableReconnect: false"},
		{paths[1], "initialReady := make(chan struct{})", "run(func() error { return grpcServer.Serve(listener) }); initialReady := make(chan struct{})"},
		{paths[1], "case <-initialReady:", "case <-ctx.Done():"},
		{paths[1], "!failed.Load() && ctx.Err() == nil", "true"},
		{paths[1], "client.RequireAccountIdentity(startup, cfg.NATSAccount, svid.ID.String())", "error(nil)"},
		{paths[1], "client.RequireDurableRetention(startup, snapshotprofile.Stream())", "error(nil)"},
		{paths[1], "queue.PendingKeys(startup, 1)", "queue.PendingKeys(startup, 0)"},
		{paths[1], "\t\tcancel()\n\t}", "\t\treadiness(false)\n\t}"},
		{paths[2], "grpc.WaitForHandlers(true)", "grpc.WaitForHandlers(false)"},
		{paths[1], `client.RequireRegisteredDefaultDurable(startup, snapshotprofile.Name, "lake.snapshot.>", "archiver")`, "error(nil)"},
		{paths[1], "client.RequireCanonicalParking(startup, SubjectRequested, int64(snapshotprofile.Stream().MaxMsgSize))", "error(nil)"},
		{paths[2], "grpc.UnaryInterceptor(handler.delegate)", "grpc.MaxRecvMsgSize(MaxControlBytes)"},
		{paths[2], "transport.AuthorizeServices(gateway)", "transport.AuthorizeMesh()"},
		{paths[3], "len(p.Portfolios) != 0", "false"},
		{paths[3], "mux.Handle(authz.LakeSnapshotRead", "mux.Handle(authz.Read"},
		{paths[4], "hex.EncodeToString(sum[:]) != baselineSHA256", "false"},
		{paths[5], "opts = append(opts, nats.NoReconnect())", "opts = append(opts, nats.MaxReconnects(-1))"},
	} {
		copyFiles := map[string]string{}
		for path, source := range files {
			copyFiles[path] = source
		}
		copyFiles[mutation.path] = strings.Replace(copyFiles[mutation.path], mutation.old, mutation.replacement, 1)
		if copyFiles[mutation.path] == files[mutation.path] || snapshotBoundaryWiring(copyFiles) == nil {
			t.Fatalf("snapshot mutation escaped: %s", mutation.old)
		}
	}
}
