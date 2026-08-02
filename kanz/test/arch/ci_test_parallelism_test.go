package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// EVERY `go test` A WORKFLOW RUNS MUST CARRY -p 1.
//
// Every Postgres-gated package in this tree shares ONE database — CI provisions a
// single `kanzapp` and hands the same TEST_POSTGRES_URL to each step — and Go's
// default -p runs packages in parallel. Two suites replaying their migrations into
// the same schema at the same time fight over each other's tables, and the failure
// names a table the failing service has nothing to do with:
//
//	relation "ledger_snapshots" does not exist
//
// reported by `alternatives`, because accounting's test dropped it mid-migration.
// The symptom is cross-service AND intermittent — it depends on which packages
// happen to overlap — which is the worst possible shape for a signal people are
// meant to trust.
//
// WHY THIS GUARD EXISTS RATHER THAN A COMMENT. kanz-ci.yml already carried a long
// note ending "Do not remove -p 1 to make this step faster." It sat above the main
// test step, and roughly sixty lines below it the `-tags redis` step ran three
// package trees against that same database with no -p 1 at all (#239). Prose next
// to one invocation does not travel to the next one somebody adds; this does.
//
// SCOPE IS EVERY WORKFLOW, NOT JUST kanz-ci. A guard that only reads kanz-ci.yml
// would not cover the next workflow that grows a database step — and that workflow
// is exactly the one nobody would think to check. -p 1 is inert where a step names
// a single package (parallelism is BETWEEN packages), so there is no invocation for
// which the flag is a real cost, and therefore no exemption list here: an exemption
// would only ever be somebody buying speed with determinism.
func TestEveryWorkflowGoTestIsSerialised(t *testing.T) {
	repoRoot := filepath.Dir(moduleRoot(t))

	// -p=1 and -p 1 are both valid; the \b stops "-p 10" from satisfying it.
	serial := regexp.MustCompile(`-p[= ]+1\b`)

	type invocation struct{ where, line string }
	var all, offenders []invocation

	for _, path := range workflowFiles(t, repoRoot) {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		rel, _ := filepath.Rel(repoRoot, path)
		for i, raw := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
			line := strings.TrimSpace(raw)
			// Comments discuss `go test` constantly — kanz-ci.yml's own -p 1 note
			// does, and so does this guard's rationale. Matching prose would flag a
			// workflow for explaining itself.
			if strings.HasPrefix(line, "#") || !strings.Contains(line, "go test") {
				continue
			}
			inv := invocation{where: filepath.ToSlash(rel) + ":" + strconv.Itoa(i+1), line: line}
			all = append(all, inv)
			if !serial.MatchString(line) {
				offenders = append(offenders, inv)
			}
		}
	}

	// NON-VACUITY. If the scan finds no `go test` at all, the workflows moved or the
	// matcher broke, and this guard would pass no matter how CI runs its suite.
	if len(all) < 2 {
		t.Fatalf("found only %d `go test` invocation(s) in .github/workflows — the scanner is "+
			"broken, not the pipeline. kanz-ci.yml alone runs several; zero or one means this "+
			"guard is asserting nothing about how the suite is executed", len(all))
	}

	if len(offenders) > 0 {
		var lines []string
		for _, o := range offenders {
			lines = append(lines, o.where+"\n      "+o.line)
		}
		sort.Strings(lines)
		t.Fatalf("these workflow `go test` invocations do not pass -p 1:\n\n  %s\n\n"+
			"Every Postgres-gated package shares ONE database, so packages running in parallel "+
			"replay their migrations over each other. The resulting failure is INTERMITTENT and "+
			"names a table belonging to a different service, which is why it costs hours to "+
			"diagnose (#239, and the note above the main test step in kanz-ci.yml).\n\n"+
			"Add -p 1. It is free where a step names one package, and it is the only thing "+
			"keeping the shared-database steps deterministic.",
			strings.Join(lines, "\n\n  "))
	}

	t.Logf("%d workflow `go test` invocation(s), all serialised with -p 1", len(all))
}
