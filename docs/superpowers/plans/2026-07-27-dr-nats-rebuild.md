# DR spine reconstruction (`nats-rebuild`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the disaster-recovery spine rebuild executable and correct — publish the `nats-rebuild` image, restore the right topics (including compacted state), and make both properties impossible to break again.

**Architecture:** No new source of truth. The archived-topic set is *derived* at test time from `infra/kafka/topics-job.yaml`'s provisioned table intersected with the archiver's `DefaultSubjects`; one guard holds both downstream consumers (`LAKE_SINK_TOPICS`, `NATS_REBUILD_TOPICS`) to it. A second guard forbids mutable image tags in production manifests. The manifests keep explicit, operator-readable lists — the guards prevent drift.

**Tech Stack:** Go 1.26.5, `go/ast` + `regexp` parsing in `test/arch`, Docker distroless, GitHub Actions matrices, `segmentio/kafka-go` via `tools/replay`.

**Spec:** `docs/superpowers/specs/2026-07-27-dr-nats-rebuild-design.md`

## Global Constraints

- Working directory for all Go commands is `kanz/`. Docker build context is the **repo root**.
- Go toolchain is pinned: `go 1.26.1` + `toolchain go1.26.5` in `kanz/go.mod`. Do not edit either line — `TestAllDockerfilesPinTheSameGolangVersion` enforces the Dockerfile matches.
- **Never run `go` with `GOFLAGS=-mod=mod`** in this plan. It silently rewrites `go.mod`/`go.sum` (board: "Verification-mode hazard"). Default `-mod=readonly` only.
- Full-suite runs need `-p 1` when `TEST_POSTGRES_URL` is set: Postgres-backed packages `DROP`/`CREATE` the same tables in parallel otherwise.
- The canonical image org is `ghcr.io/eighred` — `TestImageOrgIsCanonical` fails the build on `kanz-eng`. The Go module path stays `github.com/kanz-eng/kanz`; that is deliberate and unrelated.
- **Guard rule (board, `67`):** never record a guard as mutation-proven unless **every arm** was individually tripped and observed to fail. Each guard task below enumerates its arms.
- **CI is halted today** (repo private, billing). Nothing in this plan may be marked complete on the strength of a CI run. Task 7 records what stays open.
- Branch: `feat/dr-nats-rebuild` (already created off `main`, spec committed at `8066bf7`).

---

## File Structure

| File | Responsibility |
|---|---|
| `kanz/tools/natsrebuild/window.go` *(new)* | Pure function: given a topic, the state-topic set, `since` and `now`, produce the `replay.Range` to read it with. No I/O. |
| `kanz/tools/natsrebuild/window_test.go` *(new)* | Unit tests for the above. |
| `kanz/tools/natsrebuild/cmd/nats-rebuild/main.go` *(modify)* | Read `NATS_REBUILD_STATE_TOPICS`; call `WindowFor` per topic instead of building one shared `Range`. |
| `kanz/tools/natsrebuild/cmd/nats-rebuild/Dockerfile` *(new)* | Distroless static image, modelled on `kanz/cmd/kanz-halt/Dockerfile`. |
| `kanz/infra/dr/nats/rebuild-job.yaml` *(modify)* | Corrected `NATS_REBUILD_TOPICS`, new `NATS_REBUILD_STATE_TOPICS`. |
| `kanz/test/arch/archived_topics_test.go` *(new)* | `TestArchivedTopicConsumersMatchTheArchiverProducedSet` + the derivation helpers. |
| `kanz/test/arch/supplychain_test.go` *(modify)* | `TestProductionManifestsPinImagesByDigest` + its exemption map. |
| `.github/workflows/build.yml` *(modify)* | Matrix entry, 25 → 26. |
| `.github/workflows/release.yml` *(modify)* | Matrix entry, 25 → 26. |
| `KANZ_TASKS.md` *(modify)* | Correct row 58's false guard claim; record the open CI/runtime items. |

Task order is deliberate: the guard (Task 2) is written **before** the manifest fix (Task 3) so the wrong manifest is what makes it red. That red run is the evidence the guard works.

---

### Task 1: Per-topic read window for compacted state topics

A uniform `now-24h` window cannot restore a compacted topic: compaction retains the latest record per key regardless of age, so a mandate armed three days ago falls outside the window and is never read. `compliance.mandate` and `risk.position` are `compact` with retention `-1`.

**Files:**
- Create: `kanz/tools/natsrebuild/window.go`
- Create: `kanz/tools/natsrebuild/window_test.go`
- Modify: `kanz/tools/natsrebuild/cmd/nats-rebuild/main.go:32-92`

**Interfaces:**
- Consumes: `replay.Range{StartOffset *int64, StartTime *time.Time, EndOffset *int64, EndTime *time.Time}` from `kanz/tools/replay/reader.go:31`. `StartOffset`/`StartTime` are mutually exclusive; so are `EndOffset`/`EndTime`; an end bound is required.
- Produces: `natsrebuild.WindowFor(topic string, state map[string]bool, since time.Duration, now time.Time) replay.Range` and `natsrebuild.ValidateTopicClasses(topics, stateTopics []string) error`.

- [ ] **Step 1: Write the failing tests**

Create `kanz/tools/natsrebuild/window_test.go`:

```go
package natsrebuild

import (
	"strings"
	"testing"
	"time"
)

func TestWindowForStateTopicReadsFromTheEarliestRetainedOffset(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	state := map[string]bool{"compliance.mandate": true}

	r := WindowFor("compliance.mandate", state, 24*time.Hour, now)

	// A compacted topic retains the latest record per key REGARDLESS of age.
	// A time lower bound would skip a mandate armed before the window and the
	// control would come back DISARMED — so the lower bound must be offset 0.
	if r.StartOffset == nil || *r.StartOffset != 0 {
		t.Fatalf("StartOffset = %v, want 0", r.StartOffset)
	}
	if r.StartTime != nil {
		t.Fatalf("StartTime = %v, want nil (mutually exclusive with StartOffset)", r.StartTime)
	}
	if r.EndTime == nil || !r.EndTime.Equal(now) {
		t.Fatalf("EndTime = %v, want %v", r.EndTime, now)
	}
}

func TestWindowForEventTopicReadsTheBoundedRecentWindow(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	state := map[string]bool{"compliance.mandate": true}

	r := WindowFor("order.order", state, 24*time.Hour, now)

	want := now.Add(-24 * time.Hour)
	if r.StartTime == nil || !r.StartTime.Equal(want) {
		t.Fatalf("StartTime = %v, want %v", r.StartTime, want)
	}
	if r.StartOffset != nil {
		t.Fatalf("StartOffset = %v, want nil (mutually exclusive with StartTime)", r.StartOffset)
	}
	if r.EndTime == nil || !r.EndTime.Equal(now) {
		t.Fatalf("EndTime = %v, want %v", r.EndTime, now)
	}
}

func TestValidateTopicClassesRefusesAStateTopicThatIsNotBeingRebuilt(t *testing.T) {
	// Fail closed: a state topic nobody reads is a config typo, and silently
	// ignoring it means the operator believes state is being restored when it
	// is not — the exact failure this whole change exists to remove.
	err := ValidateTopicClasses(
		[]string{"order.order"},
		[]string{"compliance.mandate"},
	)
	if err == nil {
		t.Fatal("want an error naming compliance.mandate, got nil")
	}
	if got := err.Error(); !strings.Contains(got, "compliance.mandate") {
		t.Fatalf("error %q does not name the offending topic", got)
	}
}

func TestValidateTopicClassesAcceptsAProperSubset(t *testing.T) {
	// Non-vacuity: a validator that rejected everything would pass the test
	// above. This proves the accept path still accepts.
	if err := ValidateTopicClasses(
		[]string{"order.order", "compliance.mandate"},
		[]string{"compliance.mandate"},
	); err != nil {
		t.Fatalf("valid subset rejected: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd kanz && go test ./tools/natsrebuild/ -run 'TestWindowFor|TestValidateTopicClasses' -v
```

Expected: FAIL — `undefined: WindowFor`, `undefined: ValidateTopicClasses`.

- [ ] **Step 3: Write the implementation**

Create `kanz/tools/natsrebuild/window.go`:

```go
package natsrebuild

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kanz-eng/kanz/tools/replay"
)

// WindowFor returns the read range for one topic.
//
// EVENT topics (Kafka cleanup.policy=delete) are rebuilt from a bounded recent
// window: the live tier is short-retention by design and a DR spine needs the
// recent history, not all of it.
//
// STATE topics (cleanup.policy=compact, retention -1) must be read IN FULL.
// Compaction retains the latest record per key regardless of age, so a time
// lower bound skips any key not written inside the window — a mandate armed
// last week would not come back, and a compliance control that returns
// DISARMED is the failure infra/kafka/topics-job.yaml warns about in writing.
//
// The end bound stays wall-clock for both: it means "everything up to now",
// and replay.Reader already stops at min(bound, high-water-mark) so a range
// past the log's end cannot block (DATA-M6).
func WindowFor(topic string, state map[string]bool, since time.Duration, now time.Time) replay.Range {
	end := now
	if state[topic] {
		var earliest int64 // 0 — the earliest retained offset, not "no bound"
		return replay.Range{StartOffset: &earliest, EndTime: &end}
	}
	start := now.Add(-since)
	return replay.Range{StartTime: &start, EndTime: &end}
}

// ValidateTopicClasses refuses a configuration whose state-topic list is not a
// subset of the topics being rebuilt. Failing closed matters here: a state
// topic nobody reads restores nothing, and the run still exits 0, so the
// operator is told the spine is rebuilt when its controls are missing.
func ValidateTopicClasses(topics, stateTopics []string) error {
	var orphans []string
	for _, s := range stateTopics {
		if !slices.Contains(topics, s) {
			orphans = append(orphans, s)
		}
	}
	if len(orphans) > 0 {
		return fmt.Errorf(
			"natsrebuild: NATS_REBUILD_STATE_TOPICS names %s, which NATS_REBUILD_TOPICS does not rebuild — "+
				"a state topic that is not read restores nothing while the run still succeeds",
			strings.Join(orphans, ", "))
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
cd kanz && go test ./tools/natsrebuild/ -v
```

Expected: PASS, including the pre-existing `TestRebuildPublishesToLiveSubjectUnflagged`.

- [ ] **Step 5: Wire it into `main.go`**

In `kanz/tools/natsrebuild/cmd/nats-rebuild/main.go`, after the existing `topics := splitList(...)` line (currently line 34), add:

```go
	stateTopics := splitList(os.Getenv("NATS_REBUILD_STATE_TOPICS"))
```

After the existing `if len(topics) == 0 { ... }` block (currently lines 41-44), add:

```go
	if err := natsrebuild.ValidateTopicClasses(topics, stateTopics); err != nil {
		logger.Error("invalid topic classification", "err", err)
		os.Exit(2)
	}
	stateSet := make(map[string]bool, len(stateTopics))
	for _, s := range stateTopics {
		stateSet[s] = true
	}
```

Replace the range construction in the read loop. The current lines 70-78 are:

```go
	end := time.Now()
	startTime := end.Add(-since)
	var total uint64
	for _, topic := range topics {
		reader, err := replay.NewReader(replay.Config{
			Brokers: brokers,
			Topic:   topic,
			Range:   replay.Range{StartTime: &startTime, EndTime: &end},
		})
```

Replace with:

```go
	now := time.Now()
	var total uint64
	for _, topic := range topics {
		reader, err := replay.NewReader(replay.Config{
			Brokers: brokers,
			Topic:   topic,
			Range:   natsrebuild.WindowFor(topic, stateSet, since, now),
		})
```

Then change the per-topic success log (currently line 90) so an operator can see which window each topic used — a rebuild that silently used the wrong window is the defect this task fixes, and the log is how it stays visible:

```go
		logger.Info("rebuilt topic", "topic", topic, "state", stateSet[topic],
			"published", stats.Published, "malformed", stats.Malformed)
```

- [ ] **Step 6: Build and vet**

```bash
cd kanz && go build ./... && go vet ./tools/natsrebuild/...
```

Expected: exit 0, no output.

- [ ] **Step 7: Commit**

```bash
git add kanz/tools/natsrebuild/
git commit -m "fix(dr): read compacted state topics in full, not through a time window

A uniform now-24h window cannot restore a compacted topic. compliance.mandate
and risk.position are cleanup.policy=compact with retention -1: compaction
keeps the latest record per key REGARDLESS of age, so a mandate armed three
days ago has a three-day-old timestamp, falls outside the window, and is never
read. The DR run would exit 0 having skipped precisely the state compaction
exists to preserve — a compliance control that comes back DISARMED.

WindowFor selects the bound per topic: StartOffset 0 (earliest retained) for
state topics, StartTime now-since for event topics. replay.Range already
supports both and makes them mutually exclusive, so this is a selection, not a
new bound. ValidateTopicClasses fails closed on a state topic that is not
being rebuilt, because that config restores nothing while still exiting 0."
```

---

### Task 2: The topic-set guard, written against the wrong manifest

**Files:**
- Create: `kanz/test/arch/archived_topics_test.go`

**Interfaces:**
- Consumes: `moduleRoot(t)` (`test/arch/risk_boundary_test.go:121`), `archiverDefaultSubjects(t, root) []string` (`test/arch/archiver_topology_test.go:89`). Both are package-level helpers in `package arch` — call them directly, do not re-implement.
- Produces: nothing consumed by later tasks. `derivedArchivedTopics` and `manifestEnvList` stay unexported to this file.

- [ ] **Step 1: Write the guard**

Create `kanz/test/arch/archived_topics_test.go`:

```go
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
		domain, _, ok := strings.Cut(name, ".")
		if !ok {
			continue
		}
		if slices.Contains(subjects, domain+".>") || slices.Contains(subjects, name+".>") {
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
		name    string
		path    string
		envVar  string
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
```

- [ ] **Step 2: Run it and watch it fail against the real manifests**

```bash
cd kanz && go test ./test/arch/ -run TestArchivedTopicConsumersMatchTheArchiverProducedSet -v
```

Expected: FAIL. `NATS_REBUILD_STATE_TOPICS` does not exist yet, so `manifestEnvList` fatals on it. That is the correct first failure — the guard refuses to read a manifest it cannot parse rather than passing vacuously.

To see the full drift before the state var exists, temporarily comment out the `stateGot`/`stateWant` block and re-run. Expected: FAIL listing ~8 topics `nats-rebuild` names that the archiver does not write, and ~10 it omits — including `order.order`, `accounting.balance`, `settlement.instruction`, `compliance.mandate`, `risk.position`. `lake-sink` should report **zero** problems. Uncomment the block afterwards.

- [ ] **Step 3: Commit the guard, red**

```bash
git add kanz/test/arch/archived_topics_test.go
git commit -m "test(arch): derive the archived topic set and hold both consumers to it

Committed RED. NATS_REBUILD_TOPICS is the pre-36a9ba1 dead table: eight of its
twelve topics do not exist, and it names no money-path topic at all. This guard
is what proves the next commit fixes it.

The set is DERIVED — topics-job.yaml's provisioned table intersected with the
archiver's DefaultSubjects — so no fourth copy of the topic table is
introduced. Both parse helpers already existed in test/arch and are reused.

Also closes an unguarded surface nobody had noticed: LAKE_SINK_TOPICS is
recorded on the board as 'guarded by two arch tests' and grep finds the string
only in lake-sink's config reader. It is guarded now."
```

---

### Task 3: Declare the unarchived topics, then correct the manifest

**Files:**
- Modify: `kanz/test/arch/archived_topics_test.go` (exemption map + completeness arm)
- Modify: `kanz/infra/dr/nats/rebuild-job.yaml:52-56`

**Scope note (owner decision, 2026-07-27):** four provisioned topics are excluded from the derived set because no archiver subject covers them. Two of those — `wealth.household` (compacted, retention −1) and `alternatives.commitment` — are *actively published*, so their exclusion is a real DR scope decision, not an artifact. The owner chose **not** to widen the archiver, and instead to make every exclusion explicit, reasoned and guarded. Absence must never be the mechanism.

- [ ] **Step 0a: Split the provisioned parse out of the derivation**

In `archived_topics_test.go`, extract the table parse so both the derivation and the new completeness arm read one source. Replace the body of `derivedArchivedTopics` (currently lines 105-142) with two functions:

```go
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
```

- [ ] **Step 0b: Add the exemption map and the completeness arm — written BEFORE the map, so it goes red**

Add to `archived_topics_test.go`:

```go
// notArchivedByDesign declares every provisioned topic the archiver
// deliberately does not drain, with the reason and the owner of that decision.
//
// A topic here is absent from Kafka, therefore absent from the lakehouse, and
// therefore ABSENT FROM DISASTER RECOVERY: after a region failover nothing
// replays it onto the live spine. That is a scope decision about durability,
// not a detail — so it is declared, reasoned, and guarded here rather than
// being implied by a subject nobody added.
//
// TestEveryProvisionedTopicIsArchivedOrDeclaredUnarchived fails the build on a
// provisioned topic that is in neither the derived set nor this map, so a NEW
// topic cannot quietly inherit "not in DR". It also fails on an entry that has
// since become archived, or that names a topic no longer provisioned, so an
// exemption cannot outlive its reason.
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
	// Non-vacuity: an empty reason is a topic-name list, which is what this map
	// exists NOT to be.
	for name, reason := range notArchivedByDesign {
		if strings.TrimSpace(reason) == "" {
			problems = append(problems, name+": declared with an empty reason — "+
				"an exemption without a stated reason and owner is just an omission with extra steps")
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
```

**Evidence required for this arm.** Write the test with `notArchivedByDesign` declared but **empty** (`var notArchivedByDesign = map[string]string{}`), run it, and capture the failure — it must name all four undrained topics (`market.book`, `market.crypto`, `wealth.household`, `alternatives.commitment`) as provisioned-but-undeclared. Then fill in the four entries and capture it passing. That RED is what proves the arm detects an undeclared topic rather than merely tolerating the ones already there.

- [ ] **Step 1: Replace the topic env block**

Replace lines 52-56 (the `NATS_REBUILD_TOPICS` comment and entry, through `NATS_REBUILD_SINCE`) with:

```yaml
            # THE ARCHIVER'S PRODUCED SET — what is in Kafka, and therefore the
            # only thing that can be rebuilt from it. Held to
            # infra/kafka/topics-job.yaml x the archiver's DefaultSubjects by
            # TestArchivedTopicConsumersMatchTheArchiverProducedSet; the same
            # guard holds lake-sink's LAKE_SINK_TOPICS to the same set.
            #
            # This list USED to be the pre-36a9ba1 dead table (market.equity,
            # execution.order, platform.model, data.market_stream, ...): eight
            # topics that do not exist and NOT ONE of the money path. A DR
            # rebuild would have restored no orders, no fills, no accounting,
            # no positions and no mandates, and exited 0.
            #
            # Not restored, each deliberately: market.book/market.crypto are
            # unarchived by design (highest volume, re-fetchable from the
            # venue), dlq.archiver holds poison messages that must never go
            # back on the live spine, and wealth/alternatives have no archiver
            # subscription so nothing was ever written to them.
            - { name: NATS_REBUILD_TOPICS, value: "order.order,strategy.signal,accounting.balance,settlement.instruction,compliance.breach,compliance.mandate,risk.portfolio,risk.position,platform.authz,platform.compliance,platform.mode,inference.feature,inference.prediction,data.feature" }
            # COMPACTED STATE — read IN FULL, never through the time window.
            # Compaction keeps the latest record per key regardless of age, so
            # a mandate armed last week is older than any recent window and
            # would simply not come back. A mandate that does not come back is
            # a compliance control that returns DISARMED (topics-job.yaml says
            # so in writing). Must equal exactly the `compact` rows of the
            # derived set — the guard asserts it.
            - { name: NATS_REBUILD_STATE_TOPICS, value: "compliance.mandate,risk.position" }
            # The recent window for EVENT topics only. 24h matches the longest
            # live-tier max-age (infra/nats/README §Streams).
            - { name: NATS_REBUILD_SINCE, value: "24h" }
```

- [ ] **Step 2: Run the guard and verify it passes**

```bash
cd kanz && go test ./test/arch/ -run TestArchivedTopicConsumersMatchTheArchiverProducedSet -v
```

Expected: PASS.

- [ ] **Step 3: Mutation-test every arm — all eight**

Run each mutation, confirm the stated failure, then revert it before the next one. Arms 6-8 cover the new exemption machinery; skipping them would repeat the exact error the board records at row 67, where a guard was certified "mutation-proven" after only its already-working arms were tripped.

| # | Mutation | Expected failure |
|---|---|---|
| 1 | Delete `order.order` from `NATS_REBUILD_TOPICS` | `nats-rebuild: NATS_REBUILD_TOPICS omits "order.order"` |
| 2 | Add `market.equity` to `NATS_REBUILD_TOPICS` | `nats-rebuild: ... names "market.equity", which the archiver does not write` |
| 3 | Delete `risk.position` from `LAKE_SINK_TOPICS` (`infra/deploy/lake-sink-deploy.yaml:138`) | `lake-sink: LAKE_SINK_TOPICS omits "risk.position"` |
| 4 | Delete `risk.position` from `NATS_REBUILD_STATE_TOPICS` | `NATS_REBUILD_STATE_TOPICS = [compliance.mandate], want exactly the compacted topics [...]` |
| 5 | In `topics-job.yaml`, change `risk.position`'s cleanup column from `compact` to `delete` | Same state-topic failure, from the other direction — proving the state list is checked against the table, not hardcoded |
| 6 | Delete the `wealth.household` entry from `notArchivedByDesign` | `wealth.household: provisioned, but no archiver subject drains it and it is not declared ... DISASTER RECOVERY WILL NOT RESTORE IT` |
| 7 | Add `"order.order": "test"` to `notArchivedByDesign` | `order.order: the archiver DOES drain this topic, but it is still declared ... stale exemption, remove it` |
| 8 | Add `"no.suchtopic": "test"` to `notArchivedByDesign` | `no.suchtopic: declared in notArchivedByDesign but not provisioned in topics-job.yaml — dead exemption, remove it` |

Arm 9 (optional, cheap): set one entry's reason to `""` and confirm the empty-reason arm fires. The map exists to carry reasoning; a bare topic list would defeat it.

```bash
cd kanz && go test ./test/arch/ -run TestArchivedTopicConsumersMatchTheArchiverProducedSet
```

`git diff` must be empty before Step 4.

- [ ] **Step 4: Commit**

```bash
git add kanz/infra/dr/nats/rebuild-job.yaml
git commit -m "fix(dr): rebuild the topics the archiver actually writes

Turns the guard from the previous commit green. NATS_REBUILD_TOPICS is now the
archiver's produced set (14 topics), and NATS_REBUILD_STATE_TOPICS names the
two compacted ones so they are read in full rather than through the 24h window.

Before: 12 topics, 8 of which do not exist, and no money path. After: every
topic the archiver writes, and nothing that is not in Kafka to begin with.

All five guard arms mutation-tested individually and observed to fail."
```

---

### Task 4: The Dockerfile

**Files:**
- Create: `kanz/tools/natsrebuild/cmd/nats-rebuild/Dockerfile`

- [ ] **Step 1: Write the Dockerfile**

```dockerfile
# syntax=docker/dockerfile:1
#
# nats-rebuild container (DR-01c). Distroless + static binary.
#
# Build context is the repo ROOT, not kanz/ — same as every other image here:
# the module's go.mod replaces github.com/kanz-eng/kanz-schemas-go =>
# ../kanz-schemas/gen/go, which is generated-not-committed (EVT-15a). Build with:
#   docker build -f kanz/tools/natsrebuild/cmd/nats-rebuild/Dockerfile .
#
# # Why the DR tool needs an image
#
# It never had one, and that was invisible because nothing had run a failover.
# infra/dr/nats/rebuild-job.yaml has referenced ghcr.io/eighred/nats-rebuild
# since it was written; the image was in neither build.yml's nor release.yml's
# matrix, so it has never been published. infra/dr/failover.sh waits 600s on a
# Job that ImagePullBackOffs — the DR cutover could not complete.
#
# The Job also needs an IDENTITY, not just a binary: it publishes to a
# verify:true NATS cluster over mTLS, and SPIRE issues an SVID to a POD from
# its ServiceAccount. Same reason kanz-halt needs an image (SEC-M3c).
FROM golang:1.26.5 AS build
WORKDIR /src
ENV GOFLAGS=-mod=mod

COPY kanz-schemas/gen/go/ ./kanz-schemas/gen/go/
COPY kanz/go.mod kanz/go.sum ./kanz/
WORKDIR /src/kanz
RUN go mod download

COPY kanz/ ./
# CGO off ⇒ a fully static binary, which is what distroless/static requires.
# -trimpath strips local paths; -s -w drop the symbol/debug tables.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
      -o /out/nats-rebuild ./tools/natsrebuild/cmd/nats-rebuild

# Distroless static: no shell, no libc, no package manager. :nonroot runs as uid
# 65532.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/nats-rebuild /nats-rebuild
USER nonroot:nonroot
ENTRYPOINT ["/nats-rebuild"]
```

> `ENV GOFLAGS=-mod=mod` inside the Dockerfile is correct and is what every other image here does — the generated SDK is not committed, so the build must be allowed to resolve it. The Global Constraint against `-mod=mod` applies to commands **you** run on the host, not to the container build.

- [ ] **Step 2: Verify the Go toolchain guard still passes**

`TestAllDockerfilesPinTheSameGolangVersion` scans every Dockerfile.

```bash
cd kanz && go test ./test/arch/ -run TestAllDockerfilesPinTheSameGolangVersion -v
```

Expected: PASS. If it fails, the `FROM golang:` tag disagrees with `kanz/go.mod`'s `toolchain` line — match the Dockerfile to `go.mod`, never the reverse.

- [ ] **Step 3: Build the image locally**

From the **repo root**:

```bash
docker build -f kanz/tools/natsrebuild/cmd/nats-rebuild/Dockerfile -t nats-rebuild:local .
```

Expected: succeeds. If `kanz-schemas/gen/go/` is absent, generate it first per the repo's buf instructions — the COPY will otherwise fail.

- [ ] **Step 4: Prove the binary fails closed**

```bash
docker run --rm nats-rebuild:local; echo "exit=$?"
```

Expected: a JSON log line `"NATS_REBUILD_TOPICS is required (the log-of-record topics to rebuild from)"` and `exit=2`. A DR tool that starts with no topics and exits 0 would be the worst possible failure mode; this proves it does not.

- [ ] **Step 5: Commit**

```bash
git add kanz/tools/natsrebuild/cmd/nats-rebuild/Dockerfile
git commit -m "build(dr): distroless image for nats-rebuild

The DR spine-rebuild Job has referenced ghcr.io/eighred/nats-rebuild since it
was written and nothing ever built it — no Dockerfile existed, and the service
was in neither CI matrix. failover.sh waits 600s on a Job that ImagePullBackOffs.

Modelled on cmd/kanz-halt/Dockerfile: repo-root build context (the schemas SDK
is generated-not-committed), CGO off for a static binary, distroless/static
nonroot. Verified locally: the image builds and exits 2 with the required-topics
error when run with no configuration."
```

---

### Task 5: Both CI matrices, together

`TestReleaseMatrixCoversEveryBuiltService` fails the build if the two matrices disagree, so a one-sided edit is caught — but do both in one commit anyway, so the tree is never knowingly broken.

**Files:**
- Modify: `.github/workflows/build.yml` (after the `kanz-provisioner` entry)
- Modify: `.github/workflows/release.yml` (after the `kanz-provisioner` entry)

- [ ] **Step 1: Add the entry to `build.yml`**

Append to the `matrix.include` list, after the `kanz-provisioner` entry:

```yaml
          # DR-01c. The spine-rebuild Job has referenced this image since it was
          # written and nothing built it, so infra/dr/failover.sh waited 600s on
          # an ImagePullBackOff. A disaster-recovery path that cannot pull its
          # own image is not a disaster-recovery path.
          - service: nats-rebuild
            dockerfile: kanz/tools/natsrebuild/cmd/nats-rebuild/Dockerfile
```

- [ ] **Step 2: Add the identical entry to `release.yml`**

Append to its `matrix.include` list, after the `kanz-provisioner` entry:

```yaml
          - service: nats-rebuild
            dockerfile: kanz/tools/natsrebuild/cmd/nats-rebuild/Dockerfile
```

- [ ] **Step 3: Verify the matrices agree**

```bash
cd kanz && go test ./test/arch/ -run 'TestReleaseMatrixCoversEveryBuiltService|TestImageOrgIsCanonical|TestWorkflowActionsArePinnedToSHA' -v
```

Expected: all PASS.

- [ ] **Step 4: Mutation-test the matrix guard**

Delete the `nats-rebuild` entry from `release.yml` only and re-run:

```bash
cd kanz && go test ./test/arch/ -run TestReleaseMatrixCoversEveryBuiltService
```

Expected: FAIL naming `nats-rebuild`. Restore the entry; confirm `git diff` shows both files present again.

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/build.yml .github/workflows/release.yml
git commit -m "ci(dr): build and release nats-rebuild (25 -> 26 images)

Both matrices, one commit. TestReleaseMatrixCoversEveryBuiltService holds them
equal and was mutation-tested by removing the release entry alone.

This is what puts a digest in ghcr for the DR Job to be pinned to — the
release.yml pin-digests job picks it up with no further change."
```

---

### Task 6: Forbid mutable image tags in production manifests

OPS-M4a's stated deliverable, never shipped: *"add an arch guard that fails the build on any mutable tag in a production manifest. The guard is the point."* The manifests were pinned by `pin-digests`; nothing held them pinned, and `nats-rebuild:latest` is the proof they drifted.

**Files:**
- Modify: `kanz/test/arch/supplychain_test.go` (append)

**Interfaces:**
- Consumes: `moduleRoot(t)`, and the package-level `privateImagePrefix` constant already used by `TestPrivateImagesHavePullSecrets`. Confirm its value is `ghcr.io/eighred/`; if it is not, use the literal and leave the existing constant alone.

- [ ] **Step 1: Append the guard**

```go
// mutableTagExempt records an image reference in infra/ that is deliberately
// NOT digest-pinned, with a written reason. Every entry must be retired; the
// dead-exemption arm below fails the build when one outlives its reason.
var mutableTagExempt = map[string]string{
	"infra/gitops/preview-applicationset.yaml": "per-PR preview environments build a fresh :pr-N image per pull " +
		"request; there is no release digest to pin to and the environment is ephemeral",

	// TEMPORARY — retire on the first release that publishes this image.
	// nats-rebuild joined the build/release matrices in the same branch as this
	// guard, so no digest exists for it yet. release.yml's pin-digests job
	// rewrites it on the first tagged run; delete this line then, and the dead-
	// exemption arm below will fail the build until it is deleted.
	"infra/dr/nats/rebuild-job.yaml": "image added to the CI matrices in this branch; no published digest exists " +
		"until the next release. Retire on the first pin-digests run",
}

// productionImageRef matches an image reference to our own registry in a
// manifest. Anchored on `image:` so a comment mentioning a tag cannot match —
// prose matching is how three earlier guards in this repo asserted nothing
// (board row 67).
var productionImageRef = regexp.MustCompile(`(?m)^\s*(?:-\s+)?image:\s*"?(ghcr\.io/eighred/[^"\s]+)"?`)

// TestProductionManifestsPinImagesByDigest is OPS-M4a's other half.
//
// release.yml already pins everything AFTER the build to the immutable digest —
// trivy, cosign and the SBOM all attest one specific image — and pin-digests
// rewrites the manifests to match. But nothing stopped a manifest drifting back
// to a tag, and one already had: the DR Job ran ghcr.io/eighred/nats-rebuild:latest.
//
// A mutable tag breaks the supply chain in two distinct ways, and both are live:
// :latest defaults imagePullPolicy to Always, so replicas rescheduled at
// different moments can run DIFFERENT CODE under one Deployment; and there is no
// previous digest to roll back TO, which is the primitive the TUI's rollback is
// supposed to wrap.
func TestProductionManifestsPinImagesByDigest(t *testing.T) {
	root := moduleRoot(t)
	infra := filepath.Join(root, "infra")

	var problems []string
	seenFiles := map[string]bool{}
	refCount := 0

	err := filepath.WalkDir(infra, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || (!strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml")) {
			return nil
		}
		body, rErr := os.ReadFile(path)
		if rErr != nil {
			return rErr
		}
		rel, _ := filepath.Rel(root, path)
		relSlash := filepath.ToSlash(rel)

		for _, m := range productionImageRef.FindAllStringSubmatch(string(body), -1) {
			ref := m[1]
			refCount++
			if strings.Contains(ref, "@sha256:") {
				if _, exempt := mutableTagExempt[relSlash]; exempt {
					problems = append(problems, relSlash+
						": every image here is digest-pinned but the file is still in mutableTagExempt — stale exemption, remove it")
				}
				continue
			}
			seenFiles[relSlash] = true
			if _, exempt := mutableTagExempt[relSlash]; exempt {
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"%s: %s is a MUTABLE tag. Production manifests must pin @sha256: — "+
					"release.yml's pin-digests job writes them. Add a named exemption with a "+
					"written reason only if the image genuinely has no release digest.", relSlash, ref))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk infra: %v", err)
	}

	// Non-vacuity: if the regex stops matching, this test would pass having
	// checked nothing.
	if refCount == 0 {
		t.Fatal("no ghcr.io/eighred/ image references found under infra/ — the parse is broken, " +
			"and this test would otherwise pass vacuously")
	}

	// Anti-rot, direction two: an exemption for a file with no mutable tag left.
	for file := range mutableTagExempt {
		if !seenFiles[file] {
			problems = append(problems, file+
				": in mutableTagExempt but has no mutable tag — dead exemption, remove it")
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("mutable image tags in production manifests:\n\n  %s", strings.Join(problems, "\n  "))
	}
}
```

If `supplychain_test.go` does not already import `io/fs`, `fmt`, `os`, `path/filepath`, `regexp`, `sort` or `strings`, add the missing ones.

- [ ] **Step 2: Run it**

```bash
cd kanz && go test ./test/arch/ -run TestProductionManifestsPinImagesByDigest -v
```

Expected: PASS. Both known mutable refs are exempted with reasons; all 41 others are `@sha256:`.

- [ ] **Step 3: Mutation-test every arm — all four**

Revert each mutation before the next.

| # | Mutation | Expected failure |
|---|---|---|
| 1 | Change any digest-pinned image in `infra/deploy/oms-deploy.yaml` to `:latest` | `infra/deploy/oms-deploy.yaml: ghcr.io/eighred/oms:latest is a MUTABLE tag` |
| 2 | Add `"infra/deploy/oms-deploy.yaml": "no reason"` to `mutableTagExempt` with the file unmodified | `...: in mutableTagExempt but has no mutable tag — dead exemption` |
| 3 | Pin `rebuild-job.yaml` to a fake digest while leaving its exemption in place | `...: every image here is digest-pinned but the file is still in mutableTagExempt — stale exemption` |
| 4 | Break `productionImageRef` (e.g. change `ghcr` to `gcrX`) | `no ghcr.io/eighred/ image references found under infra/ — the parse is broken` |

- [ ] **Step 4: Run the whole arch suite**

```bash
cd kanz && go test ./test/arch/ -v 2>&1 | tail -30
```

Expected: all PASS. `git diff` must show only the intended additions.

- [ ] **Step 5: Commit**

```bash
git add kanz/test/arch/supplychain_test.go
git commit -m "test(arch): fail the build on a mutable image tag in a production manifest

OPS-M4a's stated deliverable — 'the guard is the point' — and it was never
shipped. pin-digests rewrote the manifests; nothing held them pinned, and
infra/dr/nats/rebuild-job.yaml had already drifted back to :latest.

Two exemptions, both with written reasons and both retireable: preview
environments build a fresh :pr-N per pull request and have no release digest,
and nats-rebuild has no published digest until the release that this branch
first adds it to. The dead-exemption arm fails the build when either outlives
its reason, so the temporary one cannot be forgotten.

All four arms mutation-tested individually, including the vacuity arm."
```

---

### Task 7: Record what is true, including what is not done

**Files:**
- Modify: `KANZ_TASKS.md` (the PRODUCTION READINESS BOARD table, and OPS-M4a in TODO)

- [ ] **Step 1: Correct the false claim in row 58**

Row 58 currently reads, in part: *"`LAKE_SINK_TOPICS`: the deployed 14-topic list matches the archiver's produced set exactly … and guarded by two arch tests."* Replace that clause with:

```
**`LAKE_SINK_TOPICS`:** the deployed 14-topic list matches the archiver's
produced set exactly, set-difference empty both directions. **CORRECTED
2026-07-27: the claim "guarded by two arch tests" was FALSE.** `grep -rn
LAKE_SINK_TOPICS kanz --include=*.go` returned exactly one hit —
`lake-sink/internal/config/config.go:48`, reading the env var. No arch test
referenced it; the neighbouring `TestArchiverConsumeSetHasBackingTopics`
checks archiver subjects against provisioned topics, not lake-sink's list. The
list was correct and unguarded, which is the combination that decays. It is
guarded now by `TestArchivedTopicConsumersMatchTheArchiverProducedSet`. **The
lesson is the one this board already has nine entries for: a row asserting a
control exists is not evidence the control operates — and this is the first
one asserted as REFUTED.**
```

- [ ] **Step 2: Add the DR row**

Add to the readiness table:

```
| **The DR spine rebuild could not run, and would have restored almost nothing if it had** | **IMPLEMENTED — LOCALLY VERIFIED; IMAGE UNPUBLISHED AND FAILOVER UNRUN** | Three defects in one path. (1) **No image, ever**: `kanz/tools/natsrebuild/` had no Dockerfile and `nats-rebuild` was in neither CI matrix, so `ghcr.io/eighred/nats-rebuild` has never been published — `infra/dr/failover.sh:59` waits 600s on a Job that `ImagePullBackOff`s. (2) **The topic list was the pre-`36a9ba1` dead table**: 8 of its 12 topics do not exist and it named NO money-path topic — a DR run would have restored no orders, no fills, no accounting, no positions, no mandates, and exited 0. This was a **FOURTH copy** of the topic table; the `36a9ba1` sweep found three and missed this one because it lives in an env var. (3) **`NATS_REBUILD_SINCE=24h` was applied to compacted topics**, which retain the latest record per key regardless of age — a mandate armed last week falls outside any recent window, so the control returns DISARMED. Fixed: the set is now DERIVED (`topics-job.yaml` ∩ archiver `DefaultSubjects`, 14 topics, set-identical to `LAKE_SINK_TOPICS` — independent corroboration), state topics read from offset 0, and both consumers are held to the derived set by one guard, five arms mutation-proven. | controller | **NOT COMPLETE.** Open: a green `kanz-build`/`release.yml` publishing the image; `pin-digests` rewriting the Job to `@sha256:` and the temporary exemption retiring; and the **full `failover.sh` run on a real cluster, asserted on restored CONTENT — specifically a `compliance.mandate` written >24h before the run — not on exit code.** An exit-0 assertion would have passed against every one of the three defects above. |
```

- [ ] **Step 3: Update OPS-M4a**

Append to the OPS-M4a row in TODO:

```
**The guard shipped 2026-07-27** — `TestProductionManifestsPinImagesByDigest`,
four arms mutation-proven. It found one live regression on its first run:
`infra/dr/nats/rebuild-job.yaml` was `ghcr.io/eighred/nats-rebuild:latest`, the
last mutable tag in `infra/`. Two named exemptions remain, both with written
reasons and both retireable: per-PR preview images, and `nats-rebuild` until
its first published digest.
```

- [ ] **Step 3b: Record the tenant-DR follow-up as its own row**

Owner decision 2026-07-27: this is **not** in P0's scope and must not expand it. Record it so it is tracked rather than rediscovered.

```
| **`nats-rebuild` has no tenant dimension — tenant-prefixed topics are outside DR** | **OPEN WORK — scope boundary is documented, the capability is not built** | `topic.go:61-64` prefixes every topic with `{tenant}.` for any non-`__system__` tenant, and `infra/kafka/tenancy.yaml` provisions the same 19-row table under that prefix. `NATS_REBUILD_TOPICS` is a **flat, un-prefixed list with no tenant dimension**, so onboarding a tenant creates ~19 archived topics `nats-rebuild` will never read — **and it still exits 0**, which is the same silent-success shape the 2026-07-27 DR work was done to remove. **No guard can see it:** `TestEveryProvisionedTopicIsArchivedOrDeclaredUnarchived` iterates `topics-job.yaml` rows only, and `TestTenantTopicTableMatchesTheSystemTable` compares the two *provisioning* tables to each other, never to a DR consumer. **Latent today** — the sole tenant is `__system__` — but tenant onboarding is **shipped tooling** (`infra/onboarding/provision-tenant.sh` → `infra/tenancy/tenantctl.sh`, both present). The boundary is now stated in `rebuild-job.yaml` so an operator cannot read it as estate-wide coverage. | lead | build per-tenant rebuild (enumerate tenants, drain each prefix) **or** decide tenant state is recovered from Postgres only and guard that decision. Trigger: the first non-`__system__` tenant. |
```

- [ ] **Step 4: Run the board validator**

```bash
cd kanz && go test ./test/arch/ -run TestBoard -v 2>&1 | tail -5
```

If no board-validator test exists under that name, find it (`grep -rn "MALFORMED" kanz/tools/ kanz/test/`) and run it. Board row 62 records that this validator once **printed MALFORMED and exited 0** — so read its output, do not trust its exit code.

- [ ] **Step 5: Final full-suite run**

```bash
cd kanz && go build ./... && go vet ./... && go test -p 1 ./... 2>&1 | tail -20
```

Expected: build and vet exit 0; all packages `ok` or `no test files`.

- [ ] **Step 6: Commit**

```bash
git add KANZ_TASKS.md
git commit -m "docs(board): record the DR rebuild defects and correct row 58

Row 58 asserted LAKE_SINK_TOPICS was 'guarded by two arch tests'. It was not —
grep found the string only in lake-sink's config reader. Correct, unguarded,
and recorded as refuted: the first entry in this board's guards-that-do-not-
guard catalogue that was asserted as REFUTED rather than merely believed.

The new DR row states plainly what is NOT done: the image is unpublished, the
Job is not digest-pinned, and failover.sh has never run. It also states the
assertion that run must make — restored content including a compacted record
older than the event window — because an exit-0 assertion would have passed
against all three defects this change fixes."
```

---

## What this plan does NOT complete

CI is halted (repository private, billing). None of the following may be checked off on the strength of anything in this plan; they close on the end-of-day public-visibility push, or later:

- [ ] Green `kanz-build` on a push to `main` with the 26-image matrix
- [ ] `nats-rebuild` published to `ghcr.io/eighred/nats-rebuild`
- [ ] Green `release.yml` for this image — trivy, cosign, SBOM
- [ ] `pin-digests` PR rewriting `rebuild-job.yaml` to `@sha256:`, and the temporary exemption removed
- [ ] `cosign verify` against that digest **from a clean machine**
- [ ] **`failover.sh` end to end on a real cluster**, asserted on restored content

Per the spec's §8, **P0 is not complete until all six hold.**

---

## Self-Review

**Spec coverage.** §1 functional scope → Task 3's manifest comments and Task 7's board row. §2.1 no image → Tasks 4, 5. §2.2 wrong topics → Tasks 2, 3. §2.3 compacted window → Task 1. §2.4 unguarded `LAKE_SINK_TOPICS` → Task 2 (guarded) + Task 7 (corrected). §2.5 mutable tag → Task 6. §3 derivation → Task 2's `derivedArchivedTopics`. §4 lifecycle stages 1-2-3 → Tasks 4, 5; stage 4 pinning is existing automation; stage 6 → Task 6; the sequencing constraint → Task 6's temporary exemption plus the dead-exemption arm. §5 all seven changes → Tasks 1-7. §6 testing → each task's mutation table. §7-§8 → Task 7 and the section above. No gaps.

**Placeholders.** None. Every code step carries complete code; every command carries its expected output; every mutation carries its expected failure string.

**Type consistency.** `WindowFor(topic string, state map[string]bool, since time.Duration, now time.Time) replay.Range` — defined in Task 1 Step 3, called in Task 1 Step 5 with `stateSet`, matching. `ValidateTopicClasses(topics, stateTopics []string) error` — same. `derivedArchivedTopics` returns `map[string]string` (name → policy) and is consumed as such in the same file. `manifestEnvList` returns `[]string` and every call site treats it as one. `moduleRoot(t)` and `archiverDefaultSubjects(t, root)` match their definitions at `risk_boundary_test.go:121` and `archiver_topology_test.go:89`.

**One flagged risk.** Task 6 Step 1 assumes `privateImagePrefix` is `ghcr.io/eighred/`; the guard uses a literal in its regex regardless, so a mismatch cannot make it pass wrongly — but confirm the constant's value before reusing it elsewhere.
