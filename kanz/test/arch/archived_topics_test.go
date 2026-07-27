package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// topicRowPolicy is topicRow (kafka_topology_test.go:76) with the cleanup
// column captured. Kept separate rather than widening the original: that regex
// is load-bearing for TestEverySubjectHasAKafkaTopic and this test needs one
// more field, not a different contract.
var topicRowPolicy = regexp.MustCompile(
	`(?m)^\s{4}([a-z][a-z0-9_]*(?:\.[a-z0-9_]+)+)\s+\d+\s+(delete|compact)\s`)

// coveredBySubject reports whether an archiver subject drains this topic.
//
// It walks EVERY dot boundary, not just the first and last. topic.For
// (services/archiver/internal/topic/topic.go:55-59) appends ".snapshot" for
// STATE_SNAPSHOT events, so a real topic can be {domain}.{entity}.snapshot
// while the subject that drains it is {domain}.{entity}.> — a rule checking
// only the first and last segments drops it silently, narrowing a DR restore
// set with no error at all.
func coveredBySubject(name string, subjects []string) bool {
	for i := len(name); i > 0; i = strings.LastIndex(name[:i], ".") {
		if slices.Contains(subjects, name[:i]+".>") {
			return true
		}
	}
	return false
}

// TestCoveredBySubject pins the dot-boundary walk directly, without going
// through any manifest. The first case is the regression this function
// exists to fix: under the old first-segment/whole-name-only rule it would
// have been false, silently dropping a compacted state topic from the DR
// restore set with no error at all.
func TestCoveredBySubject(t *testing.T) {
	tests := []struct {
		name     string
		topic    string
		subjects []string
		want     bool
	}{
		{
			// topic.For appends ".snapshot" for STATE_SNAPSHOT events
			// (topic.go:55-59), so this three-segment topic is real, but
			// DefaultSubjects has no bare "risk.>" — only "risk.position.>"
			// and its siblings (config.go:19-35). Only a middle-prefix
			// check catches this; first-segment and whole-name both miss.
			name:     "middle prefix covers a three-segment snapshot topic",
			topic:    "risk.position.snapshot",
			subjects: []string{"risk.position.>", "risk.exposure.>"},
			want:     true,
		},
		{
			// The old rule's first-segment case: a bare domain wildcard
			// covering a two-segment topic.
			name:     "first-segment prefix covers a two-segment topic",
			topic:    "order.order",
			subjects: []string{"order.>"},
			want:     true,
		},
		{
			// The old rule's whole-name case: the subject names the topic
			// exactly, with ".>" appended.
			name:     "whole-name prefix covers a two-segment topic",
			topic:    "compliance.mandate",
			subjects: []string{"compliance.mandate.>"},
			want:     true,
		},
		{
			// Proves the walk does not match everything: neither "market.>"
			// nor "market.book.>" is present, so this must stay false.
			name:     "no covering subject present",
			topic:    "market.book",
			subjects: []string{"risk.position.>", "order.>"},
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := coveredBySubject(tt.topic, tt.subjects); got != tt.want {
				t.Errorf("coveredBySubject(%q, %v) = %v, want %v", tt.topic, tt.subjects, got, tt.want)
			}
		})
	}
}

// derivedArchivedTopics computes the archiver's PRODUCED set — the only set
// that can be rebuilt from Kafka, because it is the only set that is in Kafka.
//
// It is the provisioned table intersected with what the archiver consumes.
// Neither input is new: topics-job.yaml is already the single source of truth
// for what topics exist, and DefaultSubjects for what the archiver drains.
// Deriving means there is no fourth copy of the topic table to drift — which
// is exactly how the DR Job ended up naming eight topics that do not exist.
//
// Returns topic name -> cleanup policy ("delete" or "compact").
func derivedArchivedTopics(t *testing.T, root string) map[string]string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, "infra", "kafka", "topics-job.yaml"))
	if err != nil {
		t.Fatalf("read topics-job.yaml: %v", err)
	}
	rows := topicRowPolicy.FindAllStringSubmatch(string(body), -1)
	if len(rows) == 0 {
		t.Fatal("no topic rows parsed from topics-job.yaml — has the table format changed? " +
			"(this test would otherwise pass vacuously)")
	}

	subjects := archiverDefaultSubjects(t, root)
	if !slices.Contains(subjects, "order.>") {
		t.Fatalf("DefaultSubjects parse looks wrong — expected order.> among %v", subjects)
	}

	out := map[string]string{}
	for _, row := range rows {
		name, policy := row[1], row[2]
		// dlq.* is a reserved leading segment, not a {domain}.{entity} pair.
		// Republishing a poison message onto the live spine would re-inject the
		// event that already failed, so it is never rebuildable.
		if strings.HasPrefix(name, "dlq.") {
			continue
		}
		if !strings.Contains(name, ".") {
			continue
		}
		if coveredBySubject(name, subjects) {
			out[name] = policy
		}
	}
	if len(out) == 0 {
		t.Fatal("derived archived set is empty — the intersection logic is broken")
	}
	return out
}

// envFlow matches `- { name: X, value: "..." }`; envBlock matches the
// two-line form. Both appear in infra/, so both are supported rather than
// reformatting a manifest to suit a test.
func manifestEnvList(t *testing.T, path, name string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	q := regexp.QuoteMeta(name)
	patterns := []string{
		`(?m)^\s*-\s*\{\s*name:\s*` + q + `\s*,\s*value:\s*"([^"]*)"\s*\}`,
		`(?m)^\s*-\s*name:\s*` + q + `\s*$\r?\n\s*value:\s*"([^"]*)"`,
	}
	for _, p := range patterns {
		if m := regexp.MustCompile(p).FindStringSubmatch(string(body)); m != nil {
			var out []string
			for _, part := range strings.Split(m[1], ",") {
				if part = strings.TrimSpace(part); part != "" {
					out = append(out, part)
				}
			}
			if len(out) == 0 {
				t.Fatalf("%s: %s is present but empty", path, name)
			}
			return out
		}
	}
	t.Fatalf("%s: env var %s not found as a double-quoted scalar — "+
		"this test cannot silently pass on a manifest it failed to read", path, name)
	return nil
}

// TestArchivedTopicConsumersMatchTheArchiverProducedSet holds every downstream
// consumer of the archived Kafka set to that set.
//
// TWO consumers exist and they had drifted apart completely: lake-sink drains
// it to the permanent lakehouse, nats-rebuild drains it back onto the live
// spine after a failover. Before this guard, nats-rebuild's list was the
// pre-36a9ba1 dead table — eight topics that do not exist, and NOT ONE topic
// of the money path. A DR run would have restored no orders, no fills, no
// accounting, no positions and no mandates, then exited 0.
//
// The lists stay in the manifests, because an operator reading a DR runbook
// must be able to see what will be restored. They just cannot drift.
func TestArchivedTopicConsumersMatchTheArchiverProducedSet(t *testing.T) {
	root := moduleRoot(t)
	derived := derivedArchivedTopics(t, root)

	consumers := []struct {
		name   string
		path   string
		envVar string
	}{
		{"lake-sink", filepath.Join(root, "infra", "deploy", "lake-sink-deploy.yaml"), "LAKE_SINK_TOPICS"},
		{"nats-rebuild", filepath.Join(root, "infra", "dr", "nats", "rebuild-job.yaml"), "NATS_REBUILD_TOPICS"},
	}

	var problems []string
	for _, c := range consumers {
		got := manifestEnvList(t, c.path, c.envVar)
		gotSet := map[string]bool{}
		for _, topic := range got {
			gotSet[topic] = true
			if _, ok := derived[topic]; !ok {
				problems = append(problems, fmt.Sprintf(
					"%s: %s names %q, which the archiver does not write to Kafka — "+
						"nothing is there to consume", c.name, c.envVar, topic))
			}
		}
		for topic := range derived {
			if gotSet[topic] {
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"%s: %s omits %q, which the archiver DOES write — it would not be %s",
				c.name, c.envVar, topic,
				map[string]string{"lake-sink": "landed in the lakehouse", "nats-rebuild": "restored after a failover"}[c.name]))
		}
	}

	// The state list must be exactly the compacted rows of the derived set.
	// Not a hand-maintained second list: topics-job.yaml's cleanup column is
	// the declaration, and this asserts the manifest agrees with it.
	stateGot := manifestEnvList(t,
		filepath.Join(root, "infra", "dr", "nats", "rebuild-job.yaml"), "NATS_REBUILD_STATE_TOPICS")
	stateWant := []string{}
	for topic, policy := range derived {
		if policy == "compact" {
			stateWant = append(stateWant, topic)
		}
	}
	sort.Strings(stateGot)
	sort.Strings(stateWant)
	if !slices.Equal(stateGot, stateWant) {
		problems = append(problems, fmt.Sprintf(
			"nats-rebuild: NATS_REBUILD_STATE_TOPICS = %v, want exactly the compacted topics %v — "+
				"a compacted topic read through a time window loses every key not written inside it",
			stateGot, stateWant))
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("archived-topic consumer drift:\n\n  %s\n\n"+
			"The archived set is DERIVED from infra/kafka/topics-job.yaml intersected with the archiver's "+
			"DefaultSubjects. Do not fix this by editing the test: fix the manifest, or change what the "+
			"archiver consumes and let the derived set follow.", strings.Join(problems, "\n  "))
	}
}
