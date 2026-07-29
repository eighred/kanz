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
// (internal/topic/topic.go) appends ".snapshot" for
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

// provisionedNonDLQTopics returns every {domain}.{entity} row in the
// provisioned table, mapped to its cleanup policy ("delete" or "compact").
//
// dlq.* is a reserved leading segment, not a {domain}.{entity} pair.
// Republishing a poison message onto the live spine would re-inject the event
// that already failed, so a DLQ topic is never rebuildable and never archived.
func provisionedNonDLQTopics(t *testing.T, root string) map[string]string {
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
	out := map[string]string{}
	for _, row := range rows {
		name, policy := row[1], row[2]
		if strings.HasPrefix(name, "dlq.") || !strings.Contains(name, ".") {
			continue
		}
		out[name] = policy
	}
	if len(out) == 0 {
		t.Fatal("no non-DLQ topics parsed — the filter is broken")
	}
	return out
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
	provisioned := provisionedNonDLQTopics(t, root)
	subjects := archiverDefaultSubjects(t, root)
	if !slices.Contains(subjects, "order.>") {
		t.Fatalf("DefaultSubjects parse looks wrong — expected order.> among %v", subjects)
	}
	out := map[string]string{}
	for name, policy := range provisioned {
		if coveredBySubject(name, subjects) {
			out[name] = policy
		}
	}
	if len(out) == 0 {
		t.Fatal("derived archived set is empty — the intersection logic is broken")
	}
	return out
}

// notArchivedByDesign declares every provisioned topic the archiver
// deliberately does not drain, with the reason and the owner of that decision.
//
// A topic here is absent from Kafka, therefore absent from the lakehouse, and
// therefore ABSENT FROM DISASTER RECOVERY: after a region failover nothing
// replays it onto the live spine. That is a scope decision about durability,
// not a detail — so it is declared, reasoned, and guarded here rather than
// being implied by a subject nobody added.
//
// The "reason and owner" requirement is mechanically enforced, not just
// requested: TestEveryProvisionedTopicIsArchivedOrDeclaredUnarchived rejects
// any entry whose reason is shorter than minReasonLength (a placeholder like
// "TODO" cannot clear it, every real entry does with headroom to spare) or
// that contains neither an ISO date (YYYY-MM-DD) nor an uppercase decision ID
// (e.g. DATA-M1) per reasonProvenance — so a reason cannot merely be prose,
// it has to say who or when decided.
//
// TestEveryProvisionedTopicIsArchivedOrDeclaredUnarchived fails the build on a
// provisioned topic that is in neither the derived set nor this map, so a NEW
// topic cannot quietly inherit "not in DR". It also fails on an entry that has
// since become archived, or that names a topic no longer provisioned, so an
// exemption cannot outlive its reason.

// minReasonLength is a floor a placeholder cannot clear but every real
// exemption reason clears with headroom — the shortest current entry
// ("market.crypto"'s) is well over twice this.
const minReasonLength = 60

// reasonProvenance requires an ISO date (YYYY-MM-DD) or an uppercase decision
// ID (e.g. DATA-M1, COMP-M2, OPS-M4a) somewhere in the reason, so "who or
// when decided" is checked mechanically instead of merely asked for in a
// doc comment.
var reasonProvenance = regexp.MustCompile(`\d{4}-\d{2}-\d{2}|[A-Z]{3,}-M?\d+[a-z]?`)

var notArchivedByDesign = map[string]string{
	"market.book": "DATA-M1 scope: L2 depth is the highest-volume stream in the estate and is " +
		"re-fetchable from the venue, unlike a fill. Archiving it is its own capacity and cost " +
		"decision. After a failover the book is cold until the venue feeds refill it.",
	"market.crypto": "DATA-M1 scope, same as market.book: highest-volume, re-fetchable from the " +
		"venue, deliberately unarchived.",
	"wealth.household": "Owner decision 2026-07-27: wealth is durable in its SERVICE POSTGRES STORE, " +
		"not in Kafka. The archiver subscribes no wealth.> subject, so nothing is written to this " +
		"topic and nothing can be rebuilt from it. Accepted consequence: a household book is NOT " +
		"restored by nats-rebuild after a region failover and must be recovered from Postgres " +
		"(PITR/CNPG), like any other service-owned relational state. Revisit if wealth ever gains " +
		"an automated publisher — today its only producer is the kanz-household operator CLI.",
	"alternatives.commitment": "Owner decision 2026-07-27: same posture as wealth.household — the " +
		"alternatives journal's durable record is the service's Postgres store, not the 7d stream, " +
		"and no tooling replays that stream. The archiver subscribes no alternatives.> subject. " +
		"Accepted consequence: commitments and NAV marks are NOT restored by nats-rebuild and are " +
		"recovered from Postgres. Revisit if an automated administrator feed replaces the " +
		"kanz-altevent operator CLI.",
}

// TestEveryProvisionedTopicIsArchivedOrDeclaredUnarchived closes the hole that
// let this whole class of defect exist: nothing asserted that a provisioned
// topic was reachable by DR at all.
//
// archiver_topology_test.go asserts the CONSUME side (every archiver subject
// has a backing topic). This is the other direction — every backing topic is
// either drained by the archiver, or declared undrained on purpose. Without
// it, adding a Kafka topic silently adds a topic that disaster recovery will
// never restore, and no test anywhere would notice.
func TestEveryProvisionedTopicIsArchivedOrDeclaredUnarchived(t *testing.T) {
	root := moduleRoot(t)
	provisioned := provisionedNonDLQTopics(t, root)
	derived := derivedArchivedTopics(t, root)

	var problems []string
	for name := range provisioned {
		_, archived := derived[name]
		reason, declared := notArchivedByDesign[name]
		switch {
		case archived && declared:
			problems = append(problems, fmt.Sprintf(
				"%s: the archiver DOES drain this topic, but it is still declared in "+
					"notArchivedByDesign (%q) — stale exemption, remove it", name, reason))
		case !archived && !declared:
			problems = append(problems, fmt.Sprintf(
				"%s: provisioned, but no archiver subject drains it and it is not declared in "+
					"notArchivedByDesign. Nothing is written to this topic, so DISASTER RECOVERY "+
					"WILL NOT RESTORE IT. Either add a covering subject to the archiver's "+
					"DefaultSubjects, or declare it here with the reason and who decided", name))
		}
	}
	// Anti-rot: an exemption for a topic that is no longer provisioned at all.
	for name := range notArchivedByDesign {
		if _, ok := provisioned[name]; !ok {
			problems = append(problems, name+
				": declared in notArchivedByDesign but not provisioned in topics-job.yaml — "+
				"dead exemption, remove it")
		}
	}
	// Non-vacuity: a reason must be substantive (long enough that a
	// placeholder like "TODO" cannot pass) AND carry provenance (an ISO date
	// or a decision ID), not just non-empty. An exemption without a stated
	// reason and owner is just an omission with extra steps.
	for name, reason := range notArchivedByDesign {
		trimmed := strings.TrimSpace(reason)
		switch {
		case len(trimmed) < minReasonLength:
			problems = append(problems, fmt.Sprintf(
				"%s: declared with a %d-character reason, shorter than the %d-character floor — "+
					"too short to be a substantive account of the decision, not a placeholder",
				name, len(trimmed), minReasonLength))
		case !reasonProvenance.MatchString(reason):
			problems = append(problems, name+": declared with a reason that has no provenance — "+
				"no ISO date (YYYY-MM-DD) and no decision ID (e.g. DATA-M1) tying it to who or when decided")
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("provisioned-topic DR coverage:\n\n  %s\n\n"+
			"Every provisioned topic must be either drained by the archiver (and therefore "+
			"restorable after a failover) or explicitly declared as deliberately unarchived.",
			strings.Join(problems, "\n  "))
	}
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
