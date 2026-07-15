package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// unbackedByDesign lists the archiver DefaultSubjects {domain}.{entity} subjects
// that deliberately have NO Kafka topic yet: no publisher exists (only test
// fixtures emit them). They stay SUBSCRIBED on purpose — the archiver's
// never-silent rule (KANZ_BRAIN.md) means a premature publisher must hit a loud
// NACK-loop, not be silently unarchived. Provision a topic AND remove the line
// here when a real publisher is designed (DATA-M3).
var unbackedByDesign = map[string]string{
	"risk.exposure": "no publisher yet; provision a topic + remove this line when one is designed (DATA-M3)",
	"risk.signal":   "no publisher yet; provision a topic + remove this line when one is designed (DATA-M3)",
	"risk.command":  "no publisher yet; provision a topic + remove this line when one is designed (DATA-M3)",
}

// TestArchiverConsumeSetHasBackingTopics is the CONSUME-side twin of
// TestEverySubjectHasAKafkaTopic (the publish side). The archiver drains its
// DefaultSubjects into Kafka; auto-create is disabled, so a {domain}.{entity}
// subject with no provisioned topic fails CLOSED the moment anything publishes on
// it. This asserts every two-segment ({domain}.{entity}.>) archiver subject has a
// backing topic OR a written reason in unbackedByDesign — so the consume-set can
// never silently drift ahead of the topology, and a NEW unbacked subject fails
// the build (topic-first).
func TestArchiverConsumeSetHasBackingTopics(t *testing.T) {
	root := moduleRoot(t)
	topics := provisionedTopics(t, filepath.Join(root, "infra", "kafka", "topics-job.yaml"))
	if len(topics) == 0 {
		t.Fatal("no topics found in topics-job.yaml — has the table format changed?")
	}
	subjects := archiverDefaultSubjects(t, root)
	if len(subjects) == 0 {
		t.Fatal("no DefaultSubjects parsed — this test would pass vacuously (parse regression?)")
	}
	// Non-vacuous guard: a known subject must be present, or the parse is broken.
	if !slices.Contains(subjects, "order.>") {
		t.Fatalf("DefaultSubjects parse looks wrong — expected order.> among %v", subjects)
	}

	var problems []string
	seen := map[string]bool{}
	for _, s := range subjects {
		name := strings.TrimSuffix(s, ".>")
		if strings.Count(name, ".") != 1 {
			continue // one-segment domain wildcard (order.>) — no single topic; publish-side guard covers it
		}
		seen[name] = true
		if topics[name] {
			if _, ok := unbackedByDesign[name]; ok {
				problems = append(problems, name+": now has a Kafka topic — remove it from unbackedByDesign (stale exemption)")
			}
			continue
		}
		if _, ok := unbackedByDesign[name]; ok {
			continue // deliberately unbacked, with a written reason
		}
		problems = append(problems, name+": archiver consumes "+name+".> but NO Kafka topic backs it, and it is not in unbackedByDesign")
	}
	// An allowlist entry for a subject the archiver no longer consumes is dead.
	for name := range unbackedByDesign {
		if !seen[name] {
			problems = append(problems, name+": in unbackedByDesign but not a two-segment archiver subject — dead exemption, remove it")
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("archiver consume↔topic contract violations:\n\n  %s\n\n"+
			"The archiver (services/archiver DefaultSubjects) fails closed on a {domain}.{entity} event whose Kafka "+
			"topic is not provisioned in infra/kafka/topics-job.yaml. Provision the topic, or add the subject to "+
			"unbackedByDesign with a written reason (keeping it SUBSCRIBED so a premature publisher NACK-loops loudly "+
			"rather than being silently unarchived).", strings.Join(problems, "\n  "))
	}
}

// archiverDefaultSubjects parses the archiver config source and returns the
// string elements of its DefaultSubjects var. test/arch cannot import the
// archiver's internal/config package (Go internal rule), so it reads the source —
// consistent with how subject_topology_test.go AST-walks the code.
func archiverDefaultSubjects(t *testing.T, root string) []string {
	t.Helper()
	path := filepath.Join(root, "services", "archiver", "internal", "config", "config.go")
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var subjects []string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range vs.Names {
			if name.Name != "DefaultSubjects" || i >= len(vs.Values) {
				continue
			}
			lit, ok := vs.Values[i].(*ast.CompositeLit)
			if !ok {
				continue
			}
			for _, el := range lit.Elts {
				bl, ok := el.(*ast.BasicLit)
				if !ok || bl.Kind != token.STRING {
					continue
				}
				str, uerr := strconv.Unquote(bl.Value)
				if uerr != nil {
					t.Fatalf("unquote %q: %v", bl.Value, uerr)
				}
				subjects = append(subjects, str)
			}
		}
		return true
	})
	return subjects
}
