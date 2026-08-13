package ledger

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// EVERY COLUMN THE JOURNAL WRITES MUST BE ONE THE JOURNAL READS BACK (#415).
//
// Append INSERTs venue_account_id — the exchange account whose collateral an
// entry moved, and the whole point of migration 0003's write-guard. journal's
// SELECT did not list it. So every *Event read back from Postgres carried an
// EMPTY account, including fills, which have set it since the column existed.
//
// The column was populated and invisible. Replay, MaterializeCurrent, every
// snapshot rebuilt from the journal, and any per-account projection all saw "".
// And "" is not neutral here: migration 0003 makes it the POSITIVE DECLARATION
// that an entry touched no exchange account, so the read path did not merely
// omit a field, it reported a claim the row did not make.
//
// WHY THE EXISTING TEST DID NOT CATCH IT.
// TestAppend_RecordsTheAccountTheFillSettledAgainst reads the column with its
// own hand-written SQL:
//
//	SELECT venue_account_id FROM ledger_entries WHERE entry_id = 'fill:1'
//
// which proves the WRITE and says nothing about the read path the rest of the
// service uses. A test that queries around the code under test cannot fail when
// that code stops carrying a field. This one compares the two statements
// directly, so the gap between them is the thing being asserted.
//
// IT NEEDS NO DATABASE, deliberately. The defect is a mismatch between two SQL
// literals in one file, and the Postgres-gated round-trip test beside it
// (TestJournal_ReadsBackTheVenueAccount) cannot run on a machine without a
// broker. This one runs everywhere, including on the box where the bug was
// written.

var (
	insertColsRe = regexp.MustCompile(`(?s)INSERT INTO ledger_entries\s*\((.*?)\)\s*VALUES`)
	// ANCHORED ON entry_id, not on SELECT alone. postgres.go holds two other
	// statements — a set_config and StalePortfolios' aggregate over
	// `ledger_entries e` — and a bare non-greedy SELECT..FROM match spans from the
	// first of those to the second, yielding a "column list" that is neither.
	selectColsRe = regexp.MustCompile(`(?s)SELECT\s+(entry_id.*?)\s+FROM ledger_entries`)
)

// journalReadExempt lists columns the INSERT writes that the journal read
// legitimately need not select, with the reason.
//
// ONE ENTRY, AND IT IS NOT A FIELD OF Event. tenant_id is supplied by the
// database from current_setting('app.tenant_id') and enforced by RLS; reading it
// back would hand Go a value it must never act on, because the scope is the
// connection's, not the row's.
var journalReadExempt = map[string]string{
	"tenant_id": "RLS scope, set by the engine from app.tenant_id — not a field of Event, and Go must not act on it",
}

func TestJournalReadsBackEveryColumnAppendWrites(t *testing.T) {
	src, err := os.ReadFile("postgres.go")
	if err != nil {
		t.Fatalf("read postgres.go: %v", err)
	}
	body := string(src)

	written := sqlColumnList(t, insertColsRe, body, "INSERT INTO ledger_entries")
	read := sqlColumnList(t, selectColsRe, body, "SELECT ... FROM ledger_entries")

	// NON-VACUITY, BOTH SIDES. A rewritten query or a renamed table would
	// otherwise make this pass by comparing two empty lists.
	if len(written) < 10 {
		t.Fatalf("found %d INSERT columns (%v) — expected at least 10. The statement moved and "+
			"this guard is asserting nothing", len(written), written)
	}
	if len(read) < 10 {
		t.Fatalf("found %d SELECT columns (%v) — expected at least 10. The statement moved and "+
			"this guard is asserting nothing", len(read), read)
	}

	have := map[string]bool{}
	for _, c := range read {
		have[c] = true
	}
	var missing []string
	for _, c := range written {
		if have[c] {
			continue
		}
		if reason, ok := journalReadExempt[c]; ok {
			t.Logf("%s: exempt — %s", c, reason)
			continue
		}
		missing = append(missing, c)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("Append writes %d column(s) the journal read never selects: %v.\n"+
			"Every *Event read back from Postgres carries the zero value for those, so Replay, "+
			"MaterializeCurrent and every snapshot rebuilt from the journal silently lose them. "+
			"For venue_account_id that is worse than a loss: migration 0003 makes the empty "+
			"string the positive claim that the entry touched no exchange account.", len(missing), missing)
	}

	// DEAD-ENTRY ARM: an exemption for a column that is no longer written has
	// outlived its repair and would wave through a future column of that name.
	writes := map[string]bool{}
	for _, c := range written {
		writes[c] = true
	}
	for c, reason := range journalReadExempt {
		if !writes[c] {
			t.Errorf("exemption for %q (%s) matches no INSERT column — delete it", c, reason)
		}
	}
}

// sqlColumnList pulls a comma-separated column list out of the first match of re
// and normalises it. Bare identifiers only: a projection with an expression or an
// alias is not a column list this guard can compare, and silently ignoring one
// would make the comparison weaker than it looks.
func sqlColumnList(t *testing.T, re *regexp.Regexp, body, what string) []string {
	t.Helper()
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("could not locate %q in postgres.go — the statement was rewritten and this "+
			"guard can no longer see it", what)
	}
	var out []string
	for _, raw := range strings.Split(m[1], ",") {
		c := strings.TrimSpace(strings.ReplaceAll(raw, "\n", " "))
		c = strings.TrimSpace(strings.Join(strings.Fields(c), " "))
		if c == "" {
			continue
		}
		if strings.ContainsAny(c, "()* ") {
			t.Fatalf("%s contains a non-identifier term %q — this guard compares bare column "+
				"lists, and treating an expression as a column name would make it assert less "+
				"than it appears to", what, c)
		}
		out = append(out, c)
	}
	return out
}
