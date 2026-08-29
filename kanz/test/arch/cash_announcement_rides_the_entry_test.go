package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// THE CASH LEVEL IS COMMITTED WITH THE ENTRY THAT CHANGED IT, AND ORDERED (#804).
//
// # What this is protecting
//
// consume.Fold called store.Append and then announce, as two independent writes,
// and discarded the announce error on purpose — the ledger is the book of
// record, the announcement is DERIVED, and nacking the fold to retry a publish
// would turn a broker blip into a stalled ledger. All of that was right.
//
// What it left out was the RECOVERY, and there was none: the only thing that
// re-announced a portfolio was the next fold FOR THAT PORTFOLIO. One broker blip
// therefore refused every order for that portfolio under a buying-power mandate,
// indefinitely, until unrelated activity happened to arrive.
//
// # The two things that must both stay true
//
//  1. THE ENQUEUE. Append writes the announcement in its own transaction, so the
//     entry and the FACT land together and the relay retries the publish.
//
//  2. THE ADVISORY LOCK, and this is the half a test cannot notice going
//     missing. The relay publishes one partition key in id order, and id order
//     is COMMIT order only where a key's transactions cannot interleave. The OMS
//     gets that free — every enqueue there rides a compare-and-swap. This
//     journal is append-only with ON CONFLICT DO NOTHING and has no such write,
//     so the per-portfolio lock is the ONLY thing supplying it. Remove it and
//     two folds for one portfolio commit independently, the relay publishes the
//     OLDER balance last, and internal/cashview replaces its map entry with no
//     as_of guard — so the stale level is what every buying-power check reads
//     until the next fold.
//
//     Nothing fails when that happens. The suite is single-threaded, the
//     balances are all correct in the database, and the only symptom is orders
//     being admitted or refused against a number that is quietly behind. That is
//     #795's failure in another service, and it is why this is a guard.
//
// # Why it reads the AST with comments detached
//
// Three guards in this tree have already passed while asserting nothing, because
// a regex over raw source matched their own explanatory prose. The paragraph you
// are reading cannot satisfy anything below.

const ledgerPostgresFile = "services/accounting/internal/ledger/postgres.go"

func TestTheCashAnnouncementRidesTheLedgerTransaction(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, filepath.FromSlash(ledgerPostgresFile))

	fset := token.NewFileSet()
	// Mode 0: comments are not attached.
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", ledgerPostgresFile, err)
	}

	var appendBody *ast.BlockStmt
	names := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Recv == nil {
			continue
		}
		names[fn.Name.Name] = true
		if fn.Name.Name == "Append" {
			appendBody = fn.Body
		}
	}

	// NON-VACUITY 1: the method was found. A rename or a move would otherwise
	// leave both assertions checking an absent function.
	if appendBody == nil {
		t.Fatalf("no Append method in %s — the ledger store moved, and this guard is asserting "+
			"nothing about where the cash announcement is written", ledgerPostgresFile)
	}
	// NON-VACUITY 2: the file really is the durable store, not some other one
	// that happens to have an Append.
	for _, must := range []string{"Journal", "LoadSnapshot"} {
		if !names[must] {
			t.Fatalf("%s has no %s method — this is not the ledger store this guard means to read",
				ledgerPostgresFile, must)
		}
	}

	sql := strings.Join(proposalSQLIn(appendBody), "\n")
	calls := selectorNamesIn(appendBody)

	if !strings.Contains(sql, "pg_advisory_xact_lock") {
		t.Error("ledger.Postgres.Append no longer takes a per-portfolio advisory lock.\n\n" +
			"That lock is the ONLY thing making the outbox's \"one partition key publishes in id " +
			"order\" true for this table: the journal is append-only with ON CONFLICT DO NOTHING " +
			"and carries no compare-and-swap, so without it two folds for one portfolio commit " +
			"independently and the relay publishes the OLDER balance last. internal/cashview " +
			"replaces unconditionally with no as_of guard, so that stale level is what every " +
			"buying-power check then reads.\n\n" +
			"NO TEST WILL CATCH THIS. The suite is single-threaded and every balance in the " +
			"database stays correct; the only symptom is orders decided against a number that is " +
			"quietly behind (#795, in another service).")
	}

	if !calls["Enqueue"] {
		t.Error("ledger.Postgres.Append no longer enqueues the announcement in its own " +
			"transaction.\n\nThe entry and its cash FACT are supposed to commit together. Without " +
			"it the announcement goes back to being a second, independent write whose only " +
			"recovery is the next fold for the same portfolio — so one broker blip refuses every " +
			"order for that portfolio under a buying-power mandate until unrelated activity " +
			"happens to arrive (#804).")
	}
}

// THE FOLDER MUST NOT PUBLISH THE CASH FACT ITSELF (#804).
//
// The relay is the publisher now. A direct Publish from the fold path would be
// the two-independent-writes shape restored — and worse than before, because it
// would race the relay for the same portfolio's key and could land an older
// level after a newer one.
//
// The Folder is still allowed to FLUSH, which is the relay publishing on its
// behalf and preserves the promptness the direct publish had.
func TestTheFolderDoesNotPublishTheCashFactItself(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, filepath.FromSlash("services/accounting/internal/consume"))

	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	var offenders []string
	sawFolder := false
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", p, perr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv == nil {
				continue
			}
			if proposalReceiverName(fn.Recv.List[0].Type) != "Folder" {
				continue
			}
			sawFolder = true
			if selectorNamesIn(fn.Body)["Publish"] {
				offenders = append(offenders, "Folder."+fn.Name.Name)
			}
		}
	}

	// NON-VACUITY: the Folder was found at all.
	if !sawFolder {
		t.Fatal("no methods on consume.Folder were parsed — the type moved, and this guard is " +
			"asserting nothing about who publishes the cash level")
	}
	if len(offenders) > 0 {
		t.Errorf("these Folder methods publish directly: %v.\n\n"+
			"The cash level is committed with the journal entry and published by the outbox relay "+
			"(#804). A direct publish here is the two-independent-writes shape restored, and it "+
			"would also RACE the relay for this portfolio's key — landing an older level after a "+
			"newer one, which cashview replaces with unconditionally. Flush through the relay "+
			"instead; that is what keeps the announcement as prompt as it used to be.", offenders)
	}
}
