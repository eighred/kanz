package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A ROW LOCKED `FOR UPDATE` IS LOCKED TO BE CHECKED (#816).
//
// # What went wrong without it
//
// PostgresExceptions.Override ran
//
//	SELECT status, instrument_id FROM exceptions WHERE exception_id = $1 FOR UPDATE
//
// scanned both columns, used instrument_id to build the FACT — and never
// referenced status again. Every statement after the lock therefore ran
// unconditionally. A second override on an already-OVERRIDDEN exception appended
// a second exception_overrides row, re-flipped the status, and enqueued a second
// FACT with its own event id, which audit's dedup cannot collapse. One operator
// decision, two authorisations in an append-only trail whose entire purpose is
// to make a priced exception attributable to a named human.
//
// THE LOCK WAS CORRECT. That is what makes this shape worth a guard rather than
// a review note. FOR UPDATE serialised the two writers exactly as intended, so
// every concurrency test passed, and nothing in the language complained: `status`
// WAS used — its address was passed to Scan, which is a use as far as the
// compiler and `go vet` are concerned. The defect was that the value the lock
// was taken for never reached a branch.
//
// # The rule
//
// In a `SELECT ... FOR UPDATE`, every plain local whose address is passed to the
// resulting Scan must be READ somewhere else in the enclosing function. Reading
// it means anything other than the Scan call itself: a branch, a comparison, a
// call argument, a return.
//
// The rule is not "check the status" — that is one query's business. It is that
// taking a row lock and then discarding what the row said is a lock taken for a
// decision nobody made, and it is invisible to every other signal.
//
// # Scope and limits, so nobody reads more into a green run than it carries
//
//   - Syntactic, and it recognises the CHAINED form: a query call whose result
//     has .Scan(...) called on it directly, which is how both sites in this
//     module are written. The `row := tx.QueryRow(...)` / `row.Scan(...)` form
//     would not be seen — so the guard also counts every FOR UPDATE query
//     literal in the tree and FAILS if any of them has no analysed Scan beside
//     it. Introducing the other form does not slip past; it fails here and asks
//     for the analyser to be extended.
//   - Comments are detached (parser mode 0). Three guards in this tree have
//     passed while asserting nothing because a scan over raw source matched
//     their own prose, and "FOR UPDATE" appears in comments in
//     internal/outbox/outbox.go and pkg/bus/tuning.go — neither is a query.
//   - Non-test files only. The rule is about the estate's query sites; a test
//     that spells FOR UPDATE to reproduce a defect is not one.
//   - It does not check that the branch is CORRECT, only that the value reaches
//     one. A guard cannot know what the right refusal is; it can know that the
//     row was locked and then ignored.

// lockedScan is one `SELECT ... FOR UPDATE` whose result is scanned, and what
// became of the values.
type lockedScan struct {
	site    string   // file:line of the Scan call
	fn      string   // enclosing function
	scanned []string // plain locals scanned into
	unread  []string // ...of which these are never read again
}

// forUpdateSources is every non-test .go file in the module, parsed with
// comments detached.
func forUpdateSources(t *testing.T, root string) []struct {
	rel  string
	fset *token.FileSet
	file *ast.File
} {
	t.Helper()
	var out []struct {
		rel  string
		fset *token.FileSet
		file *ast.File
	}
	// .claude is the agent-worktree root (#848): without it this walk reads another
	// checkout's files as the estate's, which is a red suite with no defect in it.
	skipDir := map[string]bool{".git": true, ".claude": true, ".gotmp": true, "gen": true, "node_modules": true, "vendor": true}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		out = append(out, struct {
			rel  string
			fset *token.FileSet
			file *ast.File
		}{filepath.ToSlash(rel), fset, f})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out
}

// mentionsForUpdate reports whether e contains a string literal holding a
// FOR UPDATE clause. BasicLit only — a comment is not an ast.BasicLit, and with
// comments detached it is not in the tree at all.
func mentionsForUpdate(e ast.Node) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if found {
			return false
		}
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if isForUpdateSQL(lit.Value) {
			found = true
			return false
		}
		return true
	})
	return found
}

// isForUpdateSQL reports whether a string literal's text carries a FOR UPDATE
// clause, tolerant of the whitespace a raw multi-line query is written with.
func isForUpdateSQL(lit string) bool {
	return strings.Contains(strings.Join(strings.Fields(strings.ToUpper(lit)), " "), "FOR UPDATE")
}

// declaredIn returns the identifiers a function body DECLARES, so a name's
// declaration is not mistaken for a read of it.
func declaredIn(body *ast.BlockStmt) map[*ast.Ident]bool {
	out := map[*ast.Ident]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.ValueSpec:
			for _, nm := range v.Names {
				out[nm] = true
			}
		case *ast.AssignStmt:
			if v.Tok != token.DEFINE {
				return true
			}
			for _, l := range v.Lhs {
				if id, ok := l.(*ast.Ident); ok {
					out[id] = true
				}
			}
		}
		return true
	})
	return out
}

// readElsewhere reports whether name is mentioned in body outside [from, to) and
// outside its own declaration — which is what "the value reached a decision"
// means syntactically.
func readElsewhere(body *ast.BlockStmt, decls map[*ast.Ident]bool, name string, from, to token.Pos) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		id, ok := n.(*ast.Ident)
		if !ok || id.Name != name || decls[id] {
			return true
		}
		if id.Pos() >= from && id.Pos() < to {
			return true
		}
		found = true
		return false
	})
	return found
}

// analyseLockedScans finds every chained `SELECT ... FOR UPDATE` + Scan in one
// file and reports which scanned locals are never read again.
func analyseLockedScans(fset *token.FileSet, rel string, f *ast.File) []lockedScan {
	var out []lockedScan
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		decls := declaredIn(fd.Body)
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Scan" || !mentionsForUpdate(sel.X) {
				return true
			}
			ls := lockedScan{
				site: fmt.Sprintf("%s:%d", rel, fset.Position(call.Pos()).Line),
				fn:   fd.Name.Name,
			}
			for _, arg := range call.Args {
				un, ok := arg.(*ast.UnaryExpr)
				if !ok || un.Op != token.AND {
					continue
				}
				// PLAIN LOCALS ONLY. `&rec.Field` writes into a struct whose own
				// use is the thing to judge, and this analysis cannot follow it;
				// counting it would produce noise, not findings.
				id, ok := un.X.(*ast.Ident)
				if !ok || id.Name == "_" {
					continue
				}
				ls.scanned = append(ls.scanned, id.Name)
				if !readElsewhere(fd.Body, decls, id.Name, call.Pos(), call.End()) {
					ls.unread = append(ls.unread, id.Name)
				}
			}
			out = append(out, ls)
			return true
		})
	}
	return out
}

// countForUpdateLiterals counts the FOR UPDATE query literals in a file. It is
// the cross-check that keeps the chained-Scan limitation honest: a query written
// in a form the analyser does not recognise still shows up here, and the count
// mismatch fails the guard.
func countForUpdateLiterals(f *ast.File) int {
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		lit, ok := node.(*ast.BasicLit)
		if ok && lit.Kind == token.STRING && isForUpdateSQL(lit.Value) {
			n++
		}
		return true
	})
	return n
}

// analyseLockedScansText runs the analysis over a source literal, so the
// pre-#816 shape can be pinned forever without leaving a broken store in the
// tree for the guard to find.
func analyseLockedScansText(t *testing.T, name, src string) []lockedScan {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	got := analyseLockedScans(fset, name, f)
	if len(got) == 0 {
		t.Fatalf("fixture %s produced no FOR UPDATE scan site — the fixture, not the tree, is broken", name)
	}
	return got
}

func lockFixture(method string) string {
	return `package fixture

import "context"

type Tx interface {
	QueryRow(ctx context.Context, sql string, args ...any) Row
	Exec(ctx context.Context, sql string, args ...any) (any, error)
}

type Row interface{ Scan(dest ...any) error }

type lot struct{}

func lotFrom(a, b, c string) (*lot, error) { return &lot{}, nil }

` + method
}

// pre816Override is PostgresExceptions.Override's read as it stood before #816:
// status scanned under the lock, instrument_id used, status never referenced.
const pre816Override = `func Override(ctx context.Context, tx Tx, id string) error {
	var status, instrumentID string
	if err := tx.QueryRow(ctx,
		` + "`SELECT status, instrument_id FROM exceptions WHERE exception_id = $1 FOR UPDATE`" + `,
		id).Scan(&status, &instrumentID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, ` + "`INSERT INTO exception_overrides (exception_id) VALUES ($1)`" + `, id); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, ` + "`UPDATE exceptions SET status = 'OVERRIDDEN' WHERE exception_id = $1`" + `, instrumentID)
	return err
}`

// fixed816Override is the shape #816 landed: the status reaches a branch.
const fixed816Override = `func Override(ctx context.Context, tx Tx, id string) error {
	var status, instrumentID string
	if err := tx.QueryRow(ctx,
		` + "`SELECT status, instrument_id FROM exceptions WHERE exception_id = $1 FOR UPDATE`" + `,
		id).Scan(&status, &instrumentID); err != nil {
		return err
	}
	if status == "OVERRIDDEN" {
		return errAlreadyOverridden
	}
	if _, err := tx.Exec(ctx, ` + "`INSERT INTO exception_overrides (exception_id) VALUES ($1)`" + `, id); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, ` + "`UPDATE exceptions SET status = 'OVERRIDDEN' WHERE exception_id = $1`" + `, instrumentID)
	return err
}

var errAlreadyOverridden = context.Canceled`

// loadLotShaped is the OMS's position.loadLot: three columns scanned and all
// three handed on. It must stay clean, or the rule degenerates into "never scan
// more than you branch on".
const loadLotShaped = `func loadLot(ctx context.Context, tx Tx, portfolioID, venue, instrument string) (*lot, error) {
	var qty, avg, realized string
	if err := tx.QueryRow(ctx, ` + "`" + `
		SELECT quantity, average_price, realized_pnl
		FROM positions
		WHERE portfolio_id = $1 AND venue = $2 AND instrument_id = $3
		FOR UPDATE` + "`" + `, portfolioID, venue, instrument).Scan(&qty, &avg, &realized); err != nil {
		return nil, err
	}
	return lotFrom(qty, avg, realized)
}`

// lockedRowExemptions is the default-deny allow-list: a scanned-and-never-read
// column under a row lock that has been reviewed and argued for. Keyed on
// "file:line of the Scan" plus the column's local name, so moving the code
// un-certifies the entry.
//
// EMPTY, and the bar for adding one is high: an argument that the value is
// genuinely not the reason the row was locked. "It is only informational" is not
// that argument — informational is what `status` looked like too.
var lockedRowExemptions = map[string]string{}

func TestALockedRowIsCheckedAndNotJustLocked(t *testing.T) {
	// FIXTURE ARM 1 (positive control). The analyser must flag the exact shape
	// #816 removed. Without this arm, a scanner that silently stopped matching
	// would report a clean estate forever.
	pre := analyseLockedScansText(t, "pre816.go", lockFixture(pre816Override))
	var preUnread []string
	for _, s := range pre {
		preUnread = append(preUnread, s.unread...)
	}
	if len(preUnread) != 1 || preUnread[0] != "status" {
		t.Fatalf("the analyser reported unread columns %v for the pre-#816 Override, want exactly "+
			"[status]. It scanned status under FOR UPDATE and never branched on it, which is the "+
			"whole defect — every arm below is vacuous until this fixture is flagged again.", preUnread)
	}

	// FIXTURE ARM 2 (negative control). The repair must be clean, or the guard
	// would have rejected the change that resolved #816.
	for _, s := range analyseLockedScansText(t, "fixed816.go", lockFixture(fixed816Override)) {
		if len(s.unread) != 0 {
			t.Fatalf("the analyser flagged the FIXED shape: %v. A guard that rejects the repair "+
				"teaches people to work around it.", s.unread)
		}
	}

	// FIXTURE ARM 3 (negative control). Scanning three columns and using all
	// three is the other real site in this module and must stay legitimate.
	for _, s := range analyseLockedScansText(t, "loadlot.go", lockFixture(loadLotShaped)) {
		if len(s.unread) != 0 {
			t.Fatalf("the analyser flagged position.loadLot's shape: %v — every column it scans is "+
				"handed to lotFrom.", s.unread)
		}
	}

	root := moduleRoot(t)
	var (
		scans    []lockedScan
		literals int
	)
	for _, src := range forUpdateSources(t, root) {
		literals += countForUpdateLiterals(src.file)
		scans = append(scans, analyseLockedScans(src.fset, src.rel, src.file)...)
	}

	// NON-VACUITY 1: the estate has FOR UPDATE query sites. Zero means the walk,
	// the parse or the literal match broke, and every assertion below is running
	// over an empty set — the failure a guard is least able to notice about
	// itself.
	if literals == 0 {
		t.Fatal("found no `SELECT ... FOR UPDATE` query literal anywhere in the module. " +
			"services/datamaster/internal/store/postgres.go and " +
			"services/oms/internal/position/postgres.go both hold one, so the walk or the " +
			"string-literal match has stopped working and this guard is checking nothing.")
	}

	// NON-VACUITY 2, and the answer to the chained-Scan limitation. Every row
	// lock must have produced an analysed Scan. A query written as
	// `row := tx.QueryRow(...)` followed by `row.Scan(...)`, or one read through
	// tx.Query and a rows loop, lands here rather than passing unexamined.
	if len(scans) < literals {
		var seen []string
		for _, s := range scans {
			seen = append(seen, s.site)
		}
		sort.Strings(seen)
		t.Fatalf("found %d `SELECT ... FOR UPDATE` query literal(s) but only %d scan site(s) the "+
			"analyser could read (%s).\n\nA row lock whose columns this guard cannot follow is a "+
			"row lock nobody is checking. Either the query does not scan (say so with a comment "+
			"and a case here), or it uses the `row := QueryRow(...)` / `row.Scan(...)` form the "+
			"analyser does not recognise — extend analyseLockedScans rather than leaving the site "+
			"unexamined.", literals, len(scans), strings.Join(seen, ", "))
	}

	// NON-VACUITY 3: the scans must actually bind column names. An analyser that
	// returned empty scanned-lists would have zero unread columns forever.
	bound := 0
	for _, s := range scans {
		bound += len(s.scanned)
	}
	if bound < 2 {
		t.Fatalf("the analyser bound %d scanned local(s) across %d FOR UPDATE scan site(s) — "+
			"datamaster scans two columns and the OMS scans three, so this should be at least "+
			"two. The &ident match has stopped working.", bound, len(scans))
	}

	// DEAD-ENTRY CHECK: an exemption naming a column that is now read reads as a
	// reviewed decision while protecting nothing.
	live := map[string]bool{}
	var offending []string
	for _, s := range scans {
		for _, col := range s.unread {
			key := s.site + " " + col
			live[key] = true
			if _, ok := lockedRowExemptions[key]; ok {
				continue
			}
			offending = append(offending, fmt.Sprintf("%s: %s scans %q under FOR UPDATE and never "+
				"reads it", s.site, s.fn, col))
		}
	}
	var dead []string
	for key := range lockedRowExemptions {
		if !live[key] {
			dead = append(dead, key)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Fatalf("lockedRowExemptions names %d column(s) that are no longer scanned-and-unread: %s\n\n"+
			"Either the code moved (update the key) or it was fixed (delete the entry). A stale "+
			"exemption cannot outlive the thing it excused.", len(dead), strings.Join(dead, ", "))
	}
	if len(offending) == 0 {
		return
	}
	sort.Strings(offending)
	t.Fatalf("%d column(s) are locked FOR UPDATE and then discarded:\n  %s\n\n"+
		"A row lock is taken so a decision can be made on what the row says. Scanning a column "+
		"and never branching on it means the lock serialises writers correctly while the check it "+
		"was taken for is absent — which is exactly what #816 was: PostgresExceptions.Override "+
		"read the exception's status under this lock, ignored it, and appended a SECOND audit row "+
		"and a SECOND FACT for one operator decision. Nothing else catches it. The compiler and "+
		"go vet both count &status as a use, the concurrency tests pass because the lock is "+
		"correct, and the damage is a duplicated row in an append-only trail whose whole purpose "+
		"is to answer 'who decided this, and when' with one name.\n\n"+
		"Either branch on the value or stop selecting it.",
		len(offending), strings.Join(offending, "\n  "))
}
