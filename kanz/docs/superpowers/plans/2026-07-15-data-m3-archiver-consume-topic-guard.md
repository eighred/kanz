# DATA-M3 — Archiver Consume↔Topic Guard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A build-time arch guard that the DATA-M1 archiver's `DefaultSubjects` consume-set never drifts ahead of the provisioned Kafka topic topology, with the three currently-unbacked subjects made a documented, deliberate exemption.

**Architecture:** One new test file in package `arch`, reusing the existing `moduleRoot`/`provisionedTopics` helpers and `go/ast` machinery. No runtime change; `DefaultSubjects` is left unchanged (fail-loud, not fail-remove).

**Tech Stack:** Go 1.26 stdlib (`go/ast`, `go/parser`, `go/token`, `slices`, `strings`, `sort`, `strconv`).

## Global Constraints

- **`DefaultSubjects` is NOT modified** — the three unbacked subjects stay subscribed (fail-loud NACK-loop beats silent-loss; the archiver's never-silent rule). The fix is a guard + allowlist, not a subject removal.
- **Only two-segment `{domain}.{entity}.>` subjects are checked** (they map to topic `{domain}.{entity}`); one-segment domain wildcards (`order.>`) are skipped (covered by the publish-side guard).
- **The guard must PASS on the current tree** (the 3 unbacked subjects are allowlisted with reasons; the 4 backed ones resolve to topics) and must be proven non-vacuous by a mutation check.
- Reuse the existing package-`arch` helpers `moduleRoot(t)` and `provisionedTopics(t, path)`; do not duplicate them.
- Ends green: `GOFLAGS=-mod=mod go test ./test/arch/ && go build ./... && go vet ./...`. `GOFLAGS=-mod=mod` required.

## File Structure

- `test/arch/archiver_topology_test.go` (new) — `TestArchiverConsumeSetHasBackingTopics`, the `unbackedByDesign` allowlist, and the `archiverDefaultSubjects` AST helper.

---

### Task 1: The archiver consume↔topic guard

**Files:**
- Create: `test/arch/archiver_topology_test.go`

**Interfaces:**
- Consumes (existing, package `arch`): `moduleRoot(t *testing.T) string`, `provisionedTopics(t *testing.T, path string) map[string]bool`.
- Produces: `TestArchiverConsumeSetHasBackingTopics`, package var `unbackedByDesign map[string]string`, helper `archiverDefaultSubjects(t *testing.T, root string) []string`.

- [ ] **Step 1: Write the guard**

Create `test/arch/archiver_topology_test.go`:

```go
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
```

- [ ] **Step 2: Run the guard — it must PASS on the current tree**

Run: `cd /c/Users/root/Desktop/eighred-kanz/kanz && GOFLAGS=-mod=mod go test ./test/arch/ -run TestArchiverConsumeSetHasBackingTopics -v`
Expected: PASS — the four backed two-segment subjects (`compliance.breach`, `compliance.mandate`, `risk.portfolio`, `risk.position`) resolve to topics, the three unbacked (`risk.exposure`, `risk.signal`, `risk.command`) are allowlisted, and domain wildcards are skipped.

- [ ] **Step 3: Mutation-check — prove the guard bites (temporary, revert after)**

Temporarily delete the `"risk.exposure": ...` line from `unbackedByDesign`, then run the guard.

Run: `cd /c/Users/root/Desktop/eighred-kanz/kanz && GOFLAGS=-mod=mod go test ./test/arch/ -run TestArchiverConsumeSetHasBackingTopics -v`
Expected: FAIL, naming `risk.exposure` (`archiver consumes risk.exposure.> but NO Kafka topic backs it, and it is not in unbackedByDesign`).

Then **restore the deleted line** and re-run: PASS again. (This proves the guard is not vacuous. Do NOT commit the mutation.)

- [ ] **Step 4: Full verification**

Run:
```bash
cd /c/Users/root/Desktop/eighred-kanz/kanz
GOFLAGS=-mod=mod go test ./test/arch/ && \
GOFLAGS=-mod=mod go build ./... && \
GOFLAGS=-mod=mod go vet ./test/arch/... && \
GOFLAGS=-mod=mod gofmt -l test/arch/
```
Expected: `test/arch` all green (the new guard + every existing arch test), build clean, vet clean, `gofmt -l` lists nothing.

- [ ] **Step 5: Commit**

```bash
cd /c/Users/root/Desktop/eighred-kanz
git add kanz/test/arch/archiver_topology_test.go
git commit -m "test(arch): guard archiver consume-set has backing Kafka topics (DATA-M3)"
```

---

## Post-implementation

After the task: update `KANZ_TASKS.md` (DATA-M3 → DONE; the deferred archiver-subjects note is now closed by the guard). No `KANZ_BRAIN.md` entry — this enforces an existing decision (the archiver's never-silent rule), it does not make a new one. Board hygiene, done in the wrap-up.
