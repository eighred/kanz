package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// EVERY SUBJECT THE CODE PUBLISHES MUST HAVE A KAFKA TOPIC TO LAND IN.
//
// The exact analogue of TestEverySubjectIsCarriedByAStream, for the OTHER half of
// the backbone — and the reason that half rotted undetected. Kafka is the durable
// log of record; auto-create is DISABLED, so a subject with no provisioned topic
// is not a soft failure, it is an archiver that fails closed on that event forever.
//
// When this was first written, only 4 of the 16 {domain}.{entity} names the code
// actually publishes on had a topic. Nine PROVISIONED topics had no publisher at
// all: they were written from the taxonomy doc's EXAMPLES rather than from the code.
//
// CROSS-LANGUAGE ON PURPOSE. kanz-py publishes inference.prediction.scored and
// data.feature.drift_detected. A guard that walked only the Go AST would pronounce
// the topology safe while being structurally blind to every subject the Python side
// publishes — which is the identical failure mode AI-M1 found, where Go required
// tenant_id on the live path and the Python producer had no tenant concept at all,
// so nothing kanz-py published had ever been consumable by any Go service.
//
// The reverse direction is deliberately NOT asserted: a provisioned topic with no
// publisher is dead weight, not a fault, and failing on one would block
// provisioning a topic ahead of the code that fills it.
func TestEverySubjectHasAKafkaTopic(t *testing.T) {
	root := moduleRoot(t) // .../kanz
	repo := filepath.Dir(root)

	topics := provisionedTopics(t, filepath.Join(root, "infra", "kafka", "topics-job.yaml"))
	if len(topics) == 0 {
		t.Fatal("no topics found in topics-job.yaml — has the table format changed?")
	}

	subjects := declaredSubjects(t, root)                                             // Go
	subjects = append(subjects, pythonSubjects(t, filepath.Join(repo, "kanz-py"))...) // Python
	if len(subjects) == 0 {
		t.Fatal("no subjects found — this test would pass vacuously")
	}

	missing := map[string]bool{}
	for _, s := range subjects {
		parts := strings.Split(s, ".")
		if len(parts) != 3 {
			continue // not a {domain}.{entity}.{event_type} logical name
		}
		if parts[0] == "replay" || parts[0] == "dlq" {
			continue // reserved, never archived
		}
		name := parts[0] + "." + parts[1]
		if !topics[name] {
			missing[name] = true
		}
	}
	if len(missing) > 0 {
		var out []string
		for m := range missing {
			out = append(out, m)
		}
		sort.Strings(out)
		t.Fatalf("these {domain}.{entity} names are published by the code but have NO Kafka topic "+
			"in infra/kafka/topics-job.yaml:\n\n  %s\n\nKafka auto-create is disabled, so the archiver "+
			"fails closed on every one of these events. Provision them.", strings.Join(out, "\n  "))
	}
}

// provisionedTopics reads the topic names from the heredoc table in topics-job.yaml.
// Table rows are: `name  partitions  cleanup  retention_ms  dlq`.
var topicRow = regexp.MustCompile(`(?m)^\s{4}([a-z][a-z0-9_]*(?:\.[a-z0-9_]+)+)\s+\d+\s+(?:delete|compact)\s`)

func provisionedTopics(t *testing.T, path string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read topics-job.yaml: %v", err)
	}
	out := map[string]bool{}
	for _, m := range topicRow.FindAllStringSubmatch(string(b), -1) {
		out[m[1]] = true
	}
	return out
}

// pySubjectAssign matches a line-anchored assignment of a string literal to an
// identifier — the same shape as a Go `const Foo = "..."` — mirroring the Go side's
// own convention: every real subject in kanz-py is a module-level constant like
// `SUBJECT_DRIFT_DETECTED = "data.feature.drift_detected"`. Anchoring on the
// assignment statement (not a bare string-literal scan) means prose can never
// match: a docstring line such as `subject="market.equity.trade"` inside a call
// expression is not `IDENT = "..."` at the start of a line, so it is structurally
// invisible to this pattern — no regex ever has to special-case a comment or
// docstring to stay correct.
var pySubjectAssign = regexp.MustCompile(`(?m)^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*"([a-z][a-z0-9]*(?:\.[a-z0-9_]+){2})"`)

func pythonSubjects(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "__pycache__", ".venv", "tests":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".py") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, m := range pySubjectAssign.FindAllStringSubmatch(string(b), -1) {
			name, value := m[1], m[2]
			if !subjectish(name) {
				continue // not a *Subject*/*EventType* constant, same filter as the Go AST walker
			}
			if protoTypeRef.MatchString(value) {
				continue // proto type ref (e.g. "inference.v1.PredictionEnvelope"), not a subject
			}
			out = append(out, value)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk kanz-py: %v", err)
	}
	return out
}
