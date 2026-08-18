package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A PARTIAL INDEX AND THE QUERY THAT NEEDS IT MUST SPELL THE SAME PREDICATE (#548).
//
// # Why a comment was not enough, demonstrated
//
// 0009 created order_proposals_pending_idx and wrote the rule down:
//
//	Postgres uses a partial index only where it can prove the predicate holds,
//	so ProposalStore.Pending spells out `approver = ''`; that clause is
//	load-bearing and removing it as redundant turns every pending-list read
//	into a sequential scan.
//
// The comment was right and it still did not hold the property. What actually
// went wrong was one level up: the PREDICATE ITSELF was wrong, because
// `approver = ”` never stops being true for a proposal nobody signs. The index
// grew for the life of the deployment while the comment kept describing a
// bounded one. A prose invariant cannot notice that its own premise expired.
//
// # What this checks
//
// The two clauses of order_proposals_open_idx appear in BOTH queries that must
// use it — ProposalStore.Pending and ProposalStore.ExpiredUnannounced. Drop
// either clause from either query and Postgres cannot prove the partial
// predicate, silently falls back to a sequential scan over every order ever
// held, and nothing fails: the answer is still correct, just slower every day.
// That is the shape a guard has to catch, because a test never will.
//
// # What it deliberately does not check
//
// That the planner actually chooses the index. That needs a real engine and
// lives in the Postgres-gated suite, which asserts the plan names
// order_proposals_open_idx rather than a Seq Scan. This guard is the half that
// runs everywhere, on every commit, without a database.
func TestThePendingIndexPredicateIsSpelledInTheQueriesThatNeedIt(t *testing.T) {
	root := moduleRoot(t)

	migrations := readAllMigrations(t, filepath.Join(root, "services", "oms", "migrations"))
	// NON-VACUITY: the migration set must be the one we think it is, or the
	// index-shape assertions below check nothing.
	if !strings.Contains(migrations, "order_proposals") {
		t.Fatal("no migration under services/oms/migrations mentions order_proposals — the path " +
			"moved and this guard is reading an empty tree")
	}

	// THE OLD INDEX MUST BE GONE. Leaving it beside the new one would restore
	// exactly the cost this repair removed: a second index on the same three
	// columns, maintained on every write, unbounded in the same way.
	if strings.Contains(migrations, "CREATE INDEX order_proposals_pending_idx") &&
		!strings.Contains(migrations, "DROP INDEX order_proposals_pending_idx") {
		t.Error("order_proposals_pending_idx is created and never dropped — it is the unbounded " +
			"index #548 is about: approver stays '' for any proposal nobody signs, so no row ever " +
			"leaves it")
	}

	src := readGuardFile(t, filepath.Join(root, "services", "oms", "internal", "order", "proposals.go"))

	// Both clauses of the partial predicate, as they must appear in a query for
	// the planner to prove it.
	const (
		undecided    = "approver = ''"
		notAnnounced = "expiry_announced_at IS NULL"
	)

	for _, q := range []struct {
		fn   string
		body string
	}{
		{"Pending", queryBodyOf(t, src, "func (p *PostgresProposals) Pending(")},
		{"ExpiredUnannounced", queryBodyOf(t, src, "func (p *PostgresProposals) ExpiredUnannounced(")},
	} {
		if !strings.Contains(q.body, undecided) {
			t.Errorf("%s's query does not spell %s — Postgres cannot prove the partial predicate "+
				"of order_proposals_open_idx and falls back to a sequential scan over every order "+
				"ever held. The read stays correct and gets slower every day, which is why nothing "+
				"else would catch this", q.fn, undecided)
		}
		if !strings.Contains(q.body, notAnnounced) {
			t.Errorf("%s's query does not spell %s.\n\n"+
				"It is NOT implied by the expiry comparison. 0010's CHECK makes the two equivalent "+
				"in practice — an announcement cannot predate the expiry it announces — but the "+
				"planner does not reason across a CHECK constraint, so the clause has to be in the "+
				"query. Without it this read is a sequential scan.", q.fn, notAnnounced)
		}
	}

	// AND THE MEMORY SEAM MUST AGREE. The shared contract exists so the double
	// cannot accept what Postgres refuses; a memory Pending that ignored the
	// announced flag would pass every contract case except the one that matters.
	// SCOPED TO THE FUNCTION, NOT THE FILE. Checking the whole file passed while
	// the clause had been deleted from Pending, because AnnounceExpiry consults the
	// same field two functions away — found by mutation, and it is the same
	// false-negative shape this guard exists to prevent one layer down.
	if !strings.Contains(funcBodyOf(t, src, "func (m *MemoryProposals) Pending("), "ExpiryAnnouncedAt.IsZero()") {
		t.Error("MemoryProposals.Pending does not consult ExpiryAnnouncedAt — the two stores now " +
			"disagree about whether an announced proposal is work, and fakeBus already taught this " +
			"repository what a permissive double costs")
	}
}

// readAllMigrations concatenates every .sql file in dir, comments stripped, so a
// predicate quoted in prose cannot be mistaken for one that is executed.
func readAllMigrations(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var b strings.Builder
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		for _, line := range strings.Split(readGuardFile(t, filepath.Join(dir, e.Name())), "\n") {
			if i := strings.Index(line, "--"); i >= 0 {
				line = line[:i]
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	return b.String()
}

func readGuardFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v — the guard cannot check what it cannot read", path, err)
	}
	return string(b)
}

// funcBodyOf returns the source of the function beginning at marker, up to the
// next top-level func.
func funcBodyOf(t *testing.T, src, marker string) string {
	t.Helper()
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatalf("could not find %q in proposals.go — it was renamed and this guard is checking "+
			"nothing", marker)
	}
	rest := src[i+len(marker):]
	if end := strings.Index(rest, "\nfunc "); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

var backtickQuery = regexp.MustCompile("(?s)`([^`]*)`")

// queryBodyOf returns the first backticked SQL literal inside the function
// beginning at marker.
func queryBodyOf(t *testing.T, src, marker string) string {
	t.Helper()
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatalf("could not find %q in proposals.go — the function was renamed and this guard is "+
			"checking nothing", marker)
	}
	rest := src[i:]
	m := backtickQuery.FindStringSubmatch(rest)
	if m == nil {
		t.Fatalf("no SQL literal found in %q — the query moved out of a backtick string and this "+
			"guard can no longer read it", marker)
	}
	return m[1]
}
