package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"gopkg.in/yaml.v3"
	"path/filepath"
	"strings"
	"testing"
)

func deliveryCall(call *ast.CallExpr, method string, args ...string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != method || len(call.Args) != len(args) {
		return false
	}
	for i, name := range args {
		id, ok := call.Args[i].(*ast.Ident)
		if !ok || id.Name != name {
			return false
		}
	}
	return true
}

// Bounded syntax check: the private delegate forwards every argument directly,
// and the sole shared pipeline forwards readiness to the actual transport.
func rawDeliveryDelegation(source string) error {
	f, err := parser.ParseFile(token.NewFileSet(), "consumer.go", source, 0)
	if err != nil {
		return err
	}
	funcs := map[string]*ast.FuncDecl{}
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv != nil {
			funcs[fn.Name.Name] = fn
		}
	}
	wrapper := funcs["subscribe"]
	if wrapper == nil || wrapper.Body == nil || len(wrapper.Body.List) != 1 {
		return fmt.Errorf("private delegate shape changed")
	}
	ret, ok := wrapper.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return fmt.Errorf("delegate must return shared outcome")
	}
	call, ok := ret.Results[0].(*ast.CallExpr)
	if !ok || !deliveryCall(call, "subscribeDelivery", "ctx", "subject", "group", "h", "ready", "nil") {
		return fmt.Errorf("delegate drops handler/readiness")
	}
	common := funcs["subscribeDelivery"]
	if common == nil || common.Body == nil {
		return fmt.Errorf("missing common pipeline")
	}
	ready, dispatch := 0, 0
	ast.Inspect(common.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if deliveryCall(call, "SubscribeReady", "ctx", "subject", "group", "handle", "ready") {
			ready++
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "dispatchEvent" && len(call.Args) == 4 {
			if h, ok := call.Args[1].(*ast.Ident); ok && h.Name == "attemptHandler" {
				dispatch++
			}
		}
		return true
	})
	if ready != 1 || dispatch != 1 {
		return fmt.Errorf("shared readiness/dispatch must remain wired")
	}
	typed := funcs["SubscribeDelivery"]
	if typed == nil || typed.Body == nil {
		return fmt.Errorf("missing delivery entrypoint")
	}
	forwarded := 0
	ast.Inspect(typed.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && deliveryCall(call, "subscribeDelivery", "ctx", "subject", "group", "nil", "nil", "h") {
			forwarded++
		}
		return true
	})
	if forwarded != 1 {
		return fmt.Errorf("delivery entrypoint drops callback")
	}
	return nil
}
func TestLakeRawDeliveryPreservesCommonReadinessPipeline(t *testing.T) {
	source := string(mustReadArchFile(t, filepath.Join(moduleRoot(t), "pkg/bus/consumer.go")))
	if err := rawDeliveryDelegation(source); err != nil {
		t.Fatal(err)
	}
	for _, change := range [][2]string{
		{"c.subscribeDelivery(ctx, subject, group, h, ready, nil)", "c.subscribeDelivery(ctx, subject, group, nil, ready, nil)"},
		{"c.subscribeDelivery(ctx, subject, group, h, ready, nil)", "c.subscribeDelivery(ctx, subject, group, h, nil, nil)"},
		{"c.subscribeDelivery(ctx, subject, group, nil, nil, h)", "c.subscribeDelivery(ctx, subject, group, nil, nil, nil)"},
		{"SubscribeReady(ctx, subject, group, handle, ready)", "SubscribeReady(ctx, subject, group, handle, nil)"},
		{"dispatchEvent(ctx, attemptHandler, env, payload)", "dispatchEvent(ctx, h, env, payload)"},
	} {
		mutated := strings.Replace(source, change[0], change[1], 1)
		if mutated == source {
			t.Fatal("mutation missed")
		}
		if rawDeliveryDelegation(mutated) == nil {
			t.Fatal("ignored delegate argument accepted")
		}
	}
}

func lakeRawProofWorkflow(source string) error {
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name      string            `yaml:"name"`
				If        string            `yaml:"if"`
				Directory string            `yaml:"working-directory"`
				Continue  bool              `yaml:"continue-on-error"`
				Env       map[string]string `yaml:"env"`
				Run       string            `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(source), &workflow); err != nil {
		return err
	}
	found := 0
	for jobName, job := range workflow.Jobs {
		for _, step := range job.Steps {
			if step.Name != "Prove landed raw Kafka custody and crash offsets" {
				continue
			}
			found++
			if jobName != "go-race" {
				return fmt.Errorf("raw proof must belong to required Go matrix")
			}
			if step.If != "matrix.kind == 'quality'" || step.Directory != "kanz" || step.Continue || step.Env["TEST_KAFKA_BROKERS"] != "localhost:9092" {
				return fmt.Errorf("raw Kafka proof routing changed")
			}
			for _, command := range []string{
				"set -euo pipefail",
				"python3 -B -m unittest discover -s ../.github/scripts -p test_check_lake_raw_evidence.py",
				"go test -race -p 1 -json -count=1 ./services/lake-sink/internal/cdc -run '^(TestRawKafkaCustodyAndOffsets|TestRawRestartReadPhaseHandoff|TestRawCrashRecoveryPhaseHandoff)$' | tee \"$RUNNER_TEMP/lake-raw-proof.jsonl\"",
				"python3 ../.github/scripts/check_lake_raw_evidence.py \"$RUNNER_TEMP/lake-raw-proof.jsonl\"",
			} {
				if !strings.Contains(step.Run, command) {
					return fmt.Errorf("mandatory raw proof command missing")
				}
			}
		}
	}
	if found != 1 {
		return fmt.Errorf("raw proof step missing or duplicated")
	}
	return nil
}
func TestLakeRawCustodyRequiresActualKafkaEvidence(t *testing.T) {
	source := string(mustReadArchFile(t, filepath.Join(moduleRoot(t), "../.github/workflows/kanz-ci.yml")))
	if err := lakeRawProofWorkflow(source); err != nil {
		t.Fatal(err)
	}
	marker := "      - name: Prove landed raw Kafka custody and crash offsets"
	start := strings.Index(source, marker)
	end := strings.Index(source[start+len(marker):], "      - name:") + start + len(marker)
	section := source[start:end]
	for _, change := range [][2]string{
		{"matrix.kind == 'quality'", "false"}, {"working-directory: kanz", "working-directory: elsewhere"},
		{"TEST_KAFKA_BROKERS: localhost:9092", "TEST_KAFKA_BROKERS: ''"},
		{"set -euo pipefail", "true"}, {"go test -race -p 1 -json -count=1", "go test -p 1 -json -count=1"},
		{"^(TestRawKafkaCustodyAndOffsets|TestRawRestartReadPhaseHandoff|TestRawCrashRecoveryPhaseHandoff)$", "^TestRawKafkaCustodyAndOffsets$"},
		{"^(TestRawKafkaCustodyAndOffsets|TestRawRestartReadPhaseHandoff|TestRawCrashRecoveryPhaseHandoff)$", "^TestRawRestartReadPhaseHandoff$"},
		{"|TestRawCrashRecoveryPhaseHandoff", ""},
		{"python3 ../.github/scripts/check_lake_raw_evidence.py", "echo"},
		{"python3 -B -m unittest discover", "echo"},
		{"        run: |", "        continue-on-error: true\n        run: |"},
	} {
		mutated := strings.Replace(section, change[0], change[1], 1)
		if mutated == section {
			t.Fatal("mutation missed")
		}
		if lakeRawProofWorkflow(source[:start]+mutated+source[end:]) == nil {
			t.Fatal("proof weakening escaped")
		}
	}
	if lakeRawProofWorkflow(source[:start]+source[end:]) == nil {
		t.Fatal("missing step escaped")
	}
}

func lakeRawCrashPhases(source string) error {
	f, err := parser.ParseFile(token.NewFileSet(), "raw_integration_test.go", source, 0)
	if err != nil {
		return err
	}
	var problem error
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "TestRawKafkaCustodyAndOffsets" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if goStmt, ok := n.(*ast.GoStmt); ok {
				ast.Inspect(goStmt, func(child ast.Node) bool {
					if id, ok := child.(*ast.Ident); ok && (id.Name == "ctx" || id.Name == "workCtx") {
						problem = fmt.Errorf("goroutine captures mutable phase context")
					}
					return true
				})
			}
			if assign, ok := n.(*ast.AssignStmt); ok && assign.Tok != token.DEFINE {
				for _, lhs := range assign.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && id.Name == "ctx" {
						problem = fmt.Errorf("setup context reassigned")
					}
				}
			}
			return true
		})
	}
	if problem != nil {
		return problem
	}
	position := 0
	for _, marker := range []string{
		"ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)",
		"subctx, stop := context.WithCancel(ctx)",
		"fetched.Topics[topic][0].CommittedOffset == independent.Offset+1",
		`membership.GroupState != "Stable" || len(membership.Members) != 1`,
		"recovery, cancelRecovery, err := rawIndependentPhase(ctx, 45*time.Second)",
		"workCtx = recovery",
		"child.Process.Kill()",
		"rawKilledProcessJoined(workCtx, done)",
		"describeRawCrashGroup(t, workCtx, brokers, group, diagnostic)",
		`membership.GroupState == "Empty" && len(membership.Members) == 0`,
		"client.Publish(workCtx, bus.Message{Subject: topic, Body: nextMarker})",
		`rawRestartReadContext(workCtx, mode == "process-crash")`,
		"restart := kafka.NewReader(",
		"restart.FetchMessage(readctx)",
	} {
		i := strings.Index(source[position:], marker)
		if i < 0 {
			return fmt.Errorf("crash phase wiring missing or reordered")
		}
		position += i + len(marker)
	}
	return nil
}

func TestLakeRawCrashPhaseWiring(t *testing.T) {
	source := string(mustReadArchFile(t, filepath.Join(moduleRoot(t), "services/lake-sink/internal/cdc/raw_integration_test.go")))
	if err := lakeRawCrashPhases(source); err != nil {
		t.Fatal(err)
	}
	for _, change := range [][2]string{
		{"rawIndependentPhase(ctx, 45*time.Second)", "rawIndependentPhase(ctx, 60*time.Second)"},
		{"rawKilledProcessJoined(workCtx, done)", "rawKilledProcessJoined(context.Background(), done)"},
		{"workCtx = recovery", "workCtx = ctx"},
		{"workCtx = recovery", "ctx = recovery"},
		{"consumer.SubscribeDelivery(subctx,", "consumer.SubscribeDelivery(workCtx,"},
		{"client.Publish(workCtx,", "client.Publish(ctx,"},
		{`rawRestartReadContext(workCtx, mode == "process-crash")`, "rawRestartReadContext(workCtx, true)"},
	} {
		mutated := strings.Replace(source, change[0], change[1], 1)
		if mutated == source || lakeRawCrashPhases(mutated) == nil {
			t.Fatal("phase weakening mutation escaped")
		}
	}
	constructor := "restart := kafka.NewReader(kafka.ReaderConfig{Brokers: brokers, Topic: topic, GroupID: group, MinBytes: 1, MaxBytes: 1 << 20})"
	handoff := `readctx, readcancel, err := rawRestartReadContext(workCtx, mode == "process-crash")`
	mutated := strings.Replace(source, constructor, "", 1)
	mutated = strings.Replace(mutated, handoff, constructor+"\n"+handoff, 1)
	if mutated == source || lakeRawCrashPhases(mutated) == nil {
		t.Fatal("reader creation before active phase handoff escaped")
	}
}
