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
				"go test -race -p 1 -json -count=1 ./services/lake-sink/internal/cdc -run '^TestRawKafkaCustodyAndOffsets$' | tee \"$RUNNER_TEMP/lake-raw-proof.jsonl\"",
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
