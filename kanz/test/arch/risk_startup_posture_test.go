package arch

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// This guard binds the current risk composition's registration and observer
// identities, not arbitrary control flow or all conditionally registered metrics.
// Behavior tests own registry values, finite labels and unknown backlog absence.
func checkRiskStartupPosture(main, posture, publisher, engine []byte) error {
	parse := func(source []byte) (map[string]*ast.FuncDecl, error) {
		file, err := parser.ParseFile(token.NewFileSet(), "owner.go", source, 0)
		if err != nil {
			return nil, err
		}
		functions := map[string]*ast.FuncDecl{}
		for _, declaration := range file.Decls {
			if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Recv == nil {
				functions[fn.Name.Name] = fn
			}
		}
		return functions, nil
	}
	m, err := parse(main)
	if err != nil {
		return err
	}
	p, err := parse(posture)
	if err != nil {
		return err
	}
	b, err := parse(publisher)
	if err != nil {
		return err
	}
	e, err := parse(engine)
	if err != nil {
		return err
	}
	for name, fn := range map[string]*ast.FuncDecl{"run": m["run"], "runEngine": m["runEngine"], "newStartupPostures": p["newStartupPostures"], "stateMeasurePostures": p["stateMeasurePostures"], "newMeasurePublisher": b["newMeasurePublisher"], "NewMetrics": e["NewMetrics"]} {
		if fn == nil || fn.Body == nil {
			return fmt.Errorf("missing owned function %s", name)
		}
	}
	registration := -1
	riskRegistration := -1
	greekRegistration := -1
	branch := -1
	httpConstruction := -1
	httpServing := -1
	for i, statement := range m["run"].Body.List {
		if riskStartupText(statement) == `postures := newStartupPostures(obs.Registry, cfg.Source, cfg.NATSURL != "")` {
			registration = i
		}
		if riskStartupText(statement) == "riskMetrics := engine.NewMetrics(obs.Registry)" {
			riskRegistration = i
		}
		if riskStartupText(statement) == "greekModels := app.NewGreekModelPosture(obs.Registry)" {
			greekRegistration = i
		}
		if assignment, ok := statement.(*ast.AssignStmt); ok && len(assignment.Lhs) == 1 && len(assignment.Rhs) == 1 && riskStartupText(assignment.Lhs[0]) == "httpSrv" {
			if call, ok := assignment.Rhs[0].(*ast.CallExpr); ok && riskStartupText(call.Fun) == "httpserver.New" && httpConstruction < 0 {
				httpConstruction = i
			}
		}
		if launch, ok := statement.(*ast.GoStmt); ok && httpServing < 0 {
			ast.Inspect(launch.Call, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok && riskStartupText(call.Fun) == "httpSrv.ListenAndServe" {
					httpServing = i
				}
				return true
			})
		}
		if conditional, ok := statement.(*ast.IfStmt); ok && riskStartupText(conditional.Cond) == `cfg.NATSURL != ""` {
			branch = i
		}
	}
	if registration < 0 || branch < 0 || registration >= branch {
		return fmt.Errorf("startup posture must precede the broker branch")
	}
	if httpConstruction < 0 || httpServing < 0 || httpConstruction >= httpServing {
		return fmt.Errorf("missing current HTTP construction and serving boundary")
	}
	for _, index := range []int{registration, riskRegistration, greekRegistration} {
		if index < 0 || index >= httpConstruction || index >= httpServing {
			return fmt.Errorf("startup collectors must precede HTTP construction and serving")
		}
	}
	if !riskStartupHasCall(m["run"], "runEngine", func(args []ast.Expr) bool { return len(args) > 0 && riskStartupText(args[len(args)-1]) == "postures" }) {
		return fmt.Errorf("engine must receive the startup instance")
	}
	if !riskStartupTopStatement(m["runEngine"], "busMetrics := postures.bus") || riskStartupHasCall(m["runEngine"], "bus.NewBusMetrics", nil) {
		return fmt.Errorf("engine must reuse startup bus collectors")
	}
	if !riskStartupHasCall(m["runEngine"], "newMeasurePublisher", func(args []ast.Expr) bool { return riskStartupArgs(args) == "producer,postures.methods" }) {
		return fmt.Errorf("publisher lost startup observer")
	}
	if !riskStartupTopStatement(m["runEngine"], "stateMeasurePostures(logger, registry, postures, greekModels)") {
		return fmt.Errorf("posture must state the final runtime registry")
	}
	if !riskStartupTopStatement(p["newStartupPostures"], "postures.bus.ConfigureClient(source, brokerConfigured)") || !riskStartupTopStatement(p["newStartupPostures"], "postures.methods.Unconfigured()") {
		return fmt.Errorf("configuration and finite startup state must be unconditional")
	}
	for _, constructor := range []string{"bus.NewBusMetrics", "app.NewMeasureRegistryPosture", "app.NewMeasureMethodPosture"} {
		if !riskStartupHasCall(p["newStartupPostures"], constructor, func(args []ast.Expr) bool { return riskStartupArgs(args) == "reg" }) {
			return fmt.Errorf("startup lost %s", constructor)
		}
	}
	for _, statement := range []string{"postures.measures.State(logger, registry)", "postures.methods.Seed(registry)"} {
		if !riskStartupTopStatement(p["stateMeasurePostures"], statement) {
			return fmt.Errorf("final registry state lost %s", statement)
		}
	}
	if riskStartupHasCall(b["newMeasurePublisher"], "app.NewMeasureMethodPosture", nil) || !riskStartupHasCall(b["newMeasurePublisher"], "publish.WithMeasureMethodObserver", func(args []ast.Expr) bool { return riskStartupArgs(args) == "methods.Observe" }) {
		return fmt.Errorf("publisher must attach the supplied observer")
	}
	finite := false
	for _, statement := range e["NewMetrics"].Body.List {
		loop, ok := statement.(*ast.RangeStmt)
		if !ok || loop.Value == nil || riskStartupText(loop.Value) != "status" || riskStartupText(loop.X) != `[]string{"ok", "error"}` {
			continue
		}
		for _, body := range loop.Body.List {
			finite = finite || riskStartupText(body) == "m.recomputeTotal.WithLabelValues(status)"
		}
	}
	if !finite {
		return fmt.Errorf("finite recompute outcomes must exist before the first event")
	}
	return nil
}

func riskStartupText(node ast.Node) string {
	var out bytes.Buffer
	if err := format.Node(&out, token.NewFileSet(), node); err != nil {
		return ""
	}
	return strings.Join(strings.Fields(out.String()), " ")
}

func riskStartupTopStatement(fn *ast.FuncDecl, expected string) bool {
	for _, statement := range fn.Body.List {
		if riskStartupText(statement) == expected {
			return true
		}
	}
	return false
}

func riskStartupArgs(args []ast.Expr) string {
	values := make([]string, len(args))
	for i, value := range args {
		values[i] = riskStartupText(value)
	}
	return strings.Join(values, ",")
}

func riskStartupHasCall(fn *ast.FuncDecl, name string, arguments func([]ast.Expr) bool) bool {
	found := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && riskStartupText(call.Fun) == name && (arguments == nil || arguments(call.Args)) {
			found = true
		}
		return true
	})
	return found
}

func TestRiskStartupPostureOwnsBothBranches(t *testing.T) {
	root := moduleRoot(t)
	read := func(path string) []byte { return mustReadArchFile(t, filepath.Join(root, filepath.FromSlash(path))) }
	main := read("services/risk_engine/cmd/risk_engine/main.go")
	posture := read("services/risk_engine/cmd/risk_engine/measureposture.go")
	publisher := read("services/risk_engine/cmd/risk_engine/measurepublish.go")
	engine := read("internal/risk/engine/metrics.go")
	if err := checkRiskStartupPosture(main, posture, publisher, engine); err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name          string
		input         int
		before, after string
	}{
		{"registration behind branch", 0, `postures := newStartupPostures(obs.Registry, cfg.Source, cfg.NATSURL != "")`, `var postures *startupPostures; if cfg.NATSURL != "" { postures = newStartupPostures(obs.Registry, cfg.Source, true) }`},
		{"different publisher observer", 0, "newMeasurePublisher(producer, postures.methods)", "newMeasurePublisher(producer, app.NewMeasureMethodPosture(obs.Registry))"},
		{"different registry", 0, "stateMeasurePostures(logger, registry, postures, greekModels)", "stateMeasurePostures(logger, compute.NewRegistry(), postures, greekModels)"},
		{"configuration omitted", 1, "postures.bus.ConfigureClient(source, brokerConfigured)", "_ = brokerConfigured"},
		{"seed behind branch", 1, "postures.methods.Unconfigured()", "if brokerConfigured { postures.methods.Unconfigured() }"},
		{"duplicate observer", 2, "methods.Observe", "app.NewMeasureMethodPosture(nil).Observe"},
		{"finite error outcome omitted", 3, `[]string{"ok", "error"}`, `[]string{"ok"}`},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			inputs := [][]byte{main, posture, publisher, engine}
			changed := strings.Replace(string(inputs[mutation.input]), mutation.before, mutation.after, 1)
			if changed == string(inputs[mutation.input]) {
				t.Fatal("mutation missed current owner")
			}
			inputs[mutation.input] = []byte(changed)
			if checkRiskStartupPosture(inputs[0], inputs[1], inputs[2], inputs[3]) == nil {
				t.Fatal("startup defect accepted")
			}
		})
	}
	t.Run("registration after HTTP serving", func(t *testing.T) {
		const registration = `postures := newStartupPostures(obs.Registry, cfg.Source, cfg.NATSURL != "")`
		changed := strings.Replace(string(main), "\t"+registration+"\n", "", 1)
		if changed == string(main) {
			t.Fatal("registration removal missed current owner")
		}
		moved := strings.Replace(changed, "\tvar runErr error", "\t"+registration+"\n\n\tvar runErr error", 1)
		if moved == changed {
			t.Fatal("post-HTTP insertion missed current owner")
		}
		if checkRiskStartupPosture([]byte(moved), posture, publisher, engine) == nil {
			t.Fatal("registration after HTTP serving accepted")
		}
	})
}
