package arch

// THE OUTBOX'S PENDING PROJECTION MUST SCAN EVERY COLUMN IT SELECTS (#817).
//
// # Why this is a static guard and not a unit test
//
// internal/outbox.Postgres.Pending builds its SELECT from one shared const,
// pendingColumns, and reads the result with one rows.Scan. If the two disagree
// — a column added to the projection with no destination, or a destination with
// no column — pgx fails at RUNTIME with "number of field descriptions must equal
// number of destinations".
//
// That failure is reachable ONLY on a machine with TEST_POSTGRES_URL set. Every
// Postgres-gated test in this repository SKIPS SILENTLY without it (CLAUDE.md
// says so, and dozens of files report `ok` while asserting nothing), so a
// developer box, `go build`, `go vet` and golangci-lint are all green on a
// projection that cannot execute. The first thing to find out would be the OMS
// relay in a cluster, where the symptom is not an error anybody is watching:
// Pending returns an error, DrainOnce returns it, and the outbox stops draining
// — order FACTs committed and never announced, which is the exact failure
// internal/outbox exists to make impossible.
//
// So the arity is asserted from the source, on every box, with no database.
//
// # There is no exemption map here, deliberately
//
// The other guards in this package classify a population — consumers, methods,
// config files — and an exemption is how a known debt is carried with an issue
// number until it is repaired. This one asserts an EQUALITY between two
// expressions in one function. There is no legitimate state in which they
// differ: a mismatch is not a debt, it is a query that cannot run. An exemption
// map would be a licence to merge exactly that, so the guard does not offer one.
//
// # What it can and cannot see
//
// It counts. It does not type-check the destinations and it cannot see a
// REORDERING — swapping two same-typed columns in the const without swapping
// their Scan targets produces matching counts and a silently wrong record. That
// is a real blind spot and it is stated rather than left to be discovered;
// internal/outbox's Postgres tests are what cover the values, and they need the
// database this guard exists to not need. What it does catch is the failure that
// actually happens when a column is added, which is the one #817 added one.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// outboxPostgresFile is the single subject. Named as a constant so a move
// renames one line and the guard fails loudly rather than silently finding
// nothing — see the os.ReadFile failure below, which is fatal on purpose.
const outboxPostgresFile = "internal/outbox/postgres.go"

// projection is what the two halves of a SELECT/Scan pair measure to.
type projection struct {
	columns  []string
	scanArgs int
}

// readProjection derives BOTH halves from the source rather than from anything
// written here: the column list is the const's own value, split as SQL would
// split it, and the destination count is the Scan call's own argument count. A
// hand-written expectation would have to be updated by the same edit that breaks
// the code, so it would never fail.
//
// The parse mode is 0 — COMMENTS ARE NOT ATTACHED. The doc paragraphs above and
// in postgres.go both name columns and both mention Scan; a guard that could be
// satisfied or defeated by its own prose checks nothing.
func readProjection(src []byte, constName, method string) (projection, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "src.go", src, 0)
	if err != nil {
		return projection{}, fmt.Errorf("parse: %w", err)
	}

	var raw string
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || spec.Names[0].Name != constName || len(spec.Values) != 1 {
			return true
		}
		lit, isLit := spec.Values[0].(*ast.BasicLit)
		if !isLit || lit.Kind != token.STRING {
			return true
		}
		if unquoted, uerr := strconv.Unquote(lit.Value); uerr == nil {
			raw, found = unquoted, true
		}
		return false
	})
	if !found {
		return projection{}, fmt.Errorf("no string const %q — the guard is looking at the wrong "+
			"source, so it would pass on anything", constName)
	}

	var columns []string
	for _, c := range strings.Split(raw, ",") {
		c = strings.TrimSpace(c)
		if c != "" {
			columns = append(columns, c)
		}
	}

	scans := 0
	args := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != method || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if !isCall {
				return true
			}
			sel, isSel := call.Fun.(*ast.SelectorExpr)
			if !isSel || sel.Sel.Name != "Scan" {
				return true
			}
			scans++
			args = len(call.Args)
			return true
		})
	}
	switch scans {
	case 0:
		return projection{}, fmt.Errorf("found no Scan call in %s — the guard measured nothing", method)
	case 1:
	default:
		return projection{}, fmt.Errorf("found %d Scan calls in %s — the guard cannot tell which one "+
			"reads the projection", scans, method)
	}
	return projection{columns: columns, scanArgs: args}, nil
}

func TestTheOutboxPendingProjectionScansEveryColumnItSelects(t *testing.T) {
	path := filepath.Join(moduleRoot(t), filepath.FromSlash(outboxPostgresFile))
	src, err := os.ReadFile(path)
	if err != nil {
		// FATAL, not skip. A guard that quietly passes when its subject moved is
		// worse than no guard: it reports coverage it no longer has.
		t.Fatalf("read %s: %v — this guard's subject moved; re-point outboxPostgresFile", path, err)
	}

	got, err := readProjection(src, "pendingColumns", "Pending")
	if err != nil {
		t.Fatalf("%s: %v", outboxPostgresFile, err)
	}

	// NON-VACUITY. A scanner that returned an empty projection and a zero arg
	// count would satisfy the equality below on any source at all.
	if len(got.columns) < 10 {
		t.Fatalf("derived only %d columns from pendingColumns (%v) — the extractor is broken, "+
			"not the projection", len(got.columns), got.columns)
	}

	if len(got.columns) != got.scanArgs {
		t.Errorf("pendingColumns selects %d columns and Pending scans into %d destinations:\n  %s\n\n"+
			"pgx fails this at runtime with \"number of field descriptions must equal number of "+
			"destinations\", and ONLY on a machine with TEST_POSTGRES_URL set — every Postgres-gated "+
			"test skips silently without it, so build, vet, lint and the local suite are all green. "+
			"In a cluster the symptom is the outbox never draining: order FACTs committed and never "+
			"announced.",
			len(got.columns), got.scanArgs, strings.Join(got.columns, ", "))
	}

	// #817's REPAIR, HELD. The cause of a stall was written on every failed
	// attempt and selected by nothing, so the only route to it was psql against
	// production during the outage. Dropping it from the projection again would
	// leave every reader — the relay's stall log today, a read plane later —
	// unable to say why the head of a key will not publish, and nothing else
	// would fail.
	if !containsString(got.columns, "last_error") {
		t.Errorf("pendingColumns no longer selects last_error (%v) — the field that says WHY a record "+
			"is stuck at the head of its key is write-only again, reachable only by hand-written SQL "+
			"against a production database mid-incident (#817)", got.columns)
	}
}

// THE GUARD MUST FAIL ON A COLUMN IT HAS NEVER SEEN.
//
// A passing arity check proves what it RESOLVED, not what its name claims. These
// cases are synthetic sources rather than a mutation of the real file, so the
// proof is executable in CI and on every box, and so it exercises the extractor
// on shapes the real file does not currently contain — a third column with no
// destination, a missing const, and a function with no Scan at all.
func TestTheProjectionGuardCatchesAnUnscannedColumn(t *testing.T) {
	const matched = `package p

const cols = ` + "`a, b`" + `

func (x *T) Read() {
	_ = rows.Scan(&r.A, &r.B)
}
`
	const unscanned = `package p

const cols = ` + "`a, b, c`" + `

func (x *T) Read() {
	_ = rows.Scan(&r.A, &r.B)
}
`
	const noScan = `package p

const cols = ` + "`a, b`" + `

func (x *T) Read() {
	_ = rows.Err()
}
`

	base, err := readProjection([]byte(matched), "cols", "Read")
	if err != nil {
		t.Fatalf("the extractor could not read a well-formed pair: %v", err)
	}
	if len(base.columns) != 2 || base.scanArgs != 2 {
		t.Fatalf("a matched pair read as %d columns / %d destinations, want 2/2 — the extractor is "+
			"not measuring what the guard compares", len(base.columns), base.scanArgs)
	}

	bad, err := readProjection([]byte(unscanned), "cols", "Read")
	if err != nil {
		t.Fatalf("extract the mismatched pair: %v", err)
	}
	if len(bad.columns) == bad.scanArgs {
		t.Errorf("a projection with a third column and no third destination read as %d/%d — "+
			"the guard would pass on the defect it exists to catch",
			len(bad.columns), bad.scanArgs)
	}

	if _, err := readProjection([]byte(noScan), "cols", "Read"); err == nil {
		t.Error("a function with no Scan produced no error — the guard would report 0 destinations " +
			"as a finding rather than as a broken measurement")
	}
	if _, err := readProjection([]byte(matched), "notTheConst", "Read"); err == nil {
		t.Error("a missing const produced no error — the guard would measure an empty projection " +
			"and pass")
	}
}
