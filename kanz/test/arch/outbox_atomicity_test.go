package arch

// THE OUTBOX ATOMICITY GUARD, ESTATE-WIDE (#841).
//
// oms_outbox_test.go asserts that the OMS's order package commits a FACT in the
// same transaction as the state change it announces. That guard is scoped to one
// package by a constant:
//
//	const omsOrderPkg = "services/oms/internal/order"
//
// and none of its reasoning is specific to the OMS. Three more packages write
// durable financial state and enqueue outbox records — accounting's ledger (the
// BOOK OF RECORD), datamaster's store, and the OMS's position book — and until
// this file none of them was covered. Accounting's Postgres.Append was read in
// full on 2026-08-30 and found correct: one Begin, the per-portfolio advisory
// lock, the venue-account declaration, the INSERT, announce(p.withTx(tx)),
// outbox.Enqueue(ctx, tx, …), Commit. So this is REGRESSION PREVENTION, not a
// repair — and the fact that the property had to be re-established by reading is
// exactly what the guard exists to stop anyone having to do again.
//
// # What the failure looks like when it comes back
//
// A dual write: a ledger entry committed with its BALANCE announcement lost, or
// a BALANCE announced for an entry that rolled back. internal/cashview replaces
// its map entry unconditionally with no as_of guard, so a lost or stale BALANCE
// is not self-correcting — it is what every subsequent buying-power check reads
// until unrelated activity for that portfolio happens to arrive.
//
// # THE SCOPE IS DERIVED, NOT LISTED, AND THAT IS THE POINT
//
// There is no package constant here and no hand-written list of writers. The
// walk is the whole module: every non-test file that imports internal/outbox is
// parsed, and every outbox.Enqueue call it contains is analysed. A fifth writer
// added next year is covered on the commit that adds it, by nobody remembering.
// A hand list is the artifact that has broken repeatedly in this repository.
// walk_skip_test.go's own header is the worked example: four guards each kept a
// hand-written directory-skip list, the four had already drifted apart, and the
// entry every one of them was missing is the one that later failed a guard on a
// clean main.
//
// # WHAT THIS CAN AND CANNOT SEE
//
// It is a syntactic, single-function analysis over the identifier handed to
// outbox.Enqueue. It asserts four things about that handle, all of them local:
// it is opened by a Begin here, it is committed here, the enqueue happens before
// that commit, and the SAME handle carries at least one other piece of work —
// the state write. The last is what separates atomicity from theatre: a
// transaction opened solely to hold the outbox record, while the row is written
// on the pool, is a dual write wearing a Begin.
//
// It CANNOT see across functions. An enqueue whose tx arrives as a PARAMETER is
// therefore a violation by default rather than a pass — not because the shape is
// wrong (a helper taking a tx can be perfectly atomic) but because the pair that
// makes it atomic is somewhere this analysis does not look, and a guard that
// silently passes on the case it cannot check is worse than no guard. There are
// zero such sites today; if one is genuinely wanted, it goes in the exemption
// map below with the issue that retires it.
//
// Comments are not parsed into the AST (mode 0) and nothing here matches source
// text, so this file's own prose can neither satisfy nor defeat it — three
// guards in this directory have passed with the checked thing deleted by
// matching their own comments.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// outboxImportPath is the package whose importers define this guard's scope. It
// is the ONE input the guard is given; everything else is measured.
const outboxImportPath = "github.com/eighred/kanz/internal/outbox"

// These are non-vacuity floors: the current counts (eight
// call sites across 4 packages) minus a margin. Tripping one means the analysis
// stopped recognising the shape, not that the estate got smaller — a guard that
// finds nothing to check passes forever.
const (
	enqueueSiteFloor       = 6
	enqueuePackageFloor    = 3
	outboxParticipantFloor = 4
)

// outboxAtomicityExemptions is the DEFAULT-DENY allow-list, keyed
// "<package>.<Func>". It is EMPTY, and an empty allow-list is not the same thing
// as no allow-list: the next enqueue that cannot show its transaction's lifetime
// lands here, named, with the issue that retires it, rather than being waved
// through. A dead entry — one naming a site that no longer offends — fails the
// check below, so an exemption cannot outlive its repair.
var outboxAtomicityExemptions = map[string]string{}

// enqueueSite is one outbox.Enqueue call, with everything the analysis needs
// about the function around it.
type enqueueSite struct {
	Pkg  string // module-relative, slash-separated
	File string
	Func string
	Line int
	Tx   string // the identifier handed to Enqueue, "" if it was not one
	Fn   *ast.FuncDecl
	Pos  token.Pos
}

func (s enqueueSite) key() string { return s.Pkg + "." + s.Func }

// TestEveryOutboxEnqueueCommitsWithItsStateWrite is the guard #841 asks for:
// the OMS's one-package property, applied to every package that enqueues.
func TestEveryOutboxEnqueueCommitsWithItsStateWrite(t *testing.T) {
	sites, _, _, _ := scanOutboxImporters(t)

	if len(sites) < enqueueSiteFloor {
		t.Fatalf("found only %d outbox.Enqueue call sites in the module (at least %d expected) — "+
			"either the estate stopped committing FACTs transactionally, which is a much larger "+
			"problem than this guard, or this analysis has stopped recognising the call and is "+
			"passing by checking nothing", len(sites), enqueueSiteFloor)
	}
	pkgs := map[string]bool{}
	for _, s := range sites {
		pkgs[s.Pkg] = true
	}
	if len(pkgs) < enqueuePackageFloor {
		t.Fatalf("outbox.Enqueue was found in only %d packages (%v; at least %d expected). This "+
			"guard exists because the property was enforced in ONE package while accounting's book "+
			"of record was unguarded — a scope that shrinks back toward one package is the same "+
			"defect returning", len(pkgs), sortedKeysOf(pkgs), enqueuePackageFloor)
	}

	offending := map[string]bool{}
	for _, s := range sites {
		why := outboxAtomicityViolation(s)
		if why == "" {
			continue
		}
		offending[s.key()] = true
		if _, exempt := outboxAtomicityExemptions[s.key()]; exempt {
			continue
		}
		t.Errorf("%s:%d: %s enqueues an outbox record and %s\n\n"+
			"An outbox record earns its keep ONLY by committing with the state change it "+
			"announces. If the two can land apart, this is two independent writes with extra "+
			"steps: a crash or a rollback between them leaves a row committed whose FACT nobody "+
			"will ever hear, or a FACT published for a change that never happened. Downstream "+
			"there is no compensator — internal/cashview replaces a portfolio's level "+
			"unconditionally, so a lost or stale BALANCE is what every buying-power check reads "+
			"until unrelated activity arrives.\n\n"+
			"Open the transaction here, do the state write on the SAME handle, enqueue, then "+
			"Commit. If this site genuinely cannot show its transaction's lifetime, add %q to "+
			"outboxAtomicityExemptions with the issue that retires it. Do not add it silently (#841).",
			s.File, s.Line, s.Func, why, s.key())
	}

	// DEAD ENTRIES. An exemption naming a site that no longer offends is a
	// description of code that is gone, and it silently covers whatever takes
	// that name next.
	for _, k := range sortedKeys(outboxAtomicityExemptions) {
		if !offending[k] {
			t.Fatalf("outboxAtomicityExemptions names %q, which no longer violates the property. "+
				"Either it was repaired — delete the entry — or it was renamed or moved, and the "+
				"exemption is now covering nothing while looking load-bearing (#841).", k)
		}
	}

	// Said out loud on every run, so the inventory this guard actually checked
	// is in the log rather than inferred from its silence. Offenders are omitted:
	// a site reported above as a violation must not also appear here as verified.
	for _, s := range sites {
		if !offending[s.key()] {
			t.Logf("outbox enqueue verified atomic: %s:%d %s (tx %q)", s.File, s.Line, s.Func, s.Tx)
		}
	}
}

// TestTheOutboxParticipantsAreDerivedFromTheImporters is the derivation itself,
// asserted rather than assumed.
//
// The participant set is the importers of internal/outbox that either call
// outbox.Enqueue or hand an outbox.Queue out of a function — the writers and the
// packages that own a queue on their behalf. Two things follow, and both are
// failures the guard above cannot express:
//
//   - The set must not collapse. It is 6 packages today; a floor catches a
//     rename or an import-path change that would leave the atomicity analysis
//     looking at nothing while still passing.
//   - A package that EXPOSES a queue must either write to it or drain it. A
//     queue that is constructed, handed around and neither enqueued into nor
//     given to a relay is decoration — the "a thing constructed must also be
//     consumed" shape, applied to the outbox rather than to a metric.
func TestTheOutboxParticipantsAreDerivedFromTheImporters(t *testing.T) {
	_, enqueuers, exposers, relays := scanOutboxImporters(t)

	participants := map[string]bool{}
	for p := range enqueuers {
		participants[p] = true
	}
	for p := range exposers {
		participants[p] = true
	}
	if len(participants) < outboxParticipantFloor {
		t.Fatalf("derived only %d outbox participant packages (%v; at least %d expected). The "+
			"scope of the atomicity guard is DERIVED from this set, so a set that has collapsed "+
			"means that guard is checking less than it reads as checking (#841)",
			len(participants), sortedKeysOf(participants), outboxParticipantFloor)
	}

	for _, p := range sortedKeysOf(exposers) {
		if enqueuers[p] || relays[p] {
			continue
		}
		t.Errorf("%s returns an outbox.Queue but neither enqueues into one nor constructs a "+
			"relay over one.\n\n"+
			"An outbox that nothing writes and nothing drains is worse than no outbox: the "+
			"service reports success, the table is the only place the FACTs are, and every health "+
			"check is green. If this package legitimately only PASSES a queue through, it should "+
			"not be naming outbox.Queue in a signature — take the concrete store instead (#841).", p)
	}

	for _, p := range sortedKeysOf(participants) {
		t.Logf("outbox participant: %s (enqueues=%v exposes-queue=%v builds-relay=%v)",
			p, enqueuers[p], exposers[p], relays[p])
	}
}

// outboxAtomicityViolation reports why a site fails the property, or "" if it
// holds. The order of the checks is the order a reader would ask the questions.
func outboxAtomicityViolation(s enqueueSite) string {
	if s.Tx == "" {
		return "the transaction it is given is not a named local handle, so nothing here can " +
			"establish that the state write shares it"
	}
	begin, isParam := txOrigin(s.Fn, s.Tx)
	if isParam {
		return fmt.Sprintf("takes its transaction %q as a PARAMETER. The Begin and the Commit that "+
			"would make this atomic are in a caller this single-function analysis does not look at, "+
			"so the property is unchecked rather than held", s.Tx)
	}
	if !begin.IsValid() {
		return fmt.Sprintf("never opens %q with a Begin in this function", s.Tx)
	}
	commit := txCommit(s.Fn, s.Tx)
	if !commit.IsValid() {
		return fmt.Sprintf("never commits %q in this function, so the record's fate depends on a "+
			"Commit somewhere this analysis cannot see", s.Tx)
	}
	if begin > s.Pos {
		return fmt.Sprintf("enqueues before %q is opened", s.Tx)
	}
	if s.Pos > commit {
		return fmt.Sprintf("enqueues AFTER %q is committed — the FACT is outside the transaction "+
			"that recorded the state change, which is the commit-then-publish pair the outbox "+
			"replaced", s.Tx)
	}
	if !txCarriesOtherWork(s) {
		return fmt.Sprintf("uses %q for NOTHING BUT the outbox record. The state write is on "+
			"another handle — the pool, or a second transaction — so the row and its FACT commit "+
			"independently and a Begin around the enqueue alone buys nothing", s.Tx)
	}
	return ""
}

// txOrigin finds where a transaction identifier comes from inside a function:
// the position of the assignment whose right-hand side calls Begin/BeginTx, or
// isParam when the handle was passed in.
func txOrigin(fn *ast.FuncDecl, tx string) (pos token.Pos, isParam bool) {
	if fn.Type.Params != nil {
		for _, f := range fn.Type.Params.List {
			for _, n := range f.Names {
				if n.Name == tx {
					return token.NoPos, true
				}
			}
		}
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		bound := false
		for _, l := range as.Lhs {
			if id, ok := l.(*ast.Ident); ok && id.Name == tx {
				bound = true
			}
		}
		if !bound {
			return true
		}
		for _, r := range as.Rhs {
			ast.Inspect(r, func(m ast.Node) bool {
				call, ok := m.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					if sel.Sel.Name == "Begin" || sel.Sel.Name == "BeginTx" {
						if !pos.IsValid() || as.Pos() < pos {
							pos = as.Pos()
						}
					}
				}
				return true
			})
		}
		return true
	})
	return pos, false
}

// txCommit returns the position of the LAST tx.Commit(...) in the function, if
// any.
//
// THE LAST, NOT THE FIRST, and the direction is deliberate. A function with a
// Commit on an early-return branch and another on the main path would, measured
// from the first, report an enqueue between them as "after the commit" — a
// violation that is not there. This guard must err toward MISSING one, never
// toward inventing one, so that it cannot block honest work; the tests are the
// primary evidence and this is the backstop.
func txCommit(fn *ast.FuncDecl, tx string) token.Pos {
	var pos token.Pos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Commit" {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == tx {
			if call.Pos() > pos {
				pos = call.Pos()
			}
		}
		return true
	})
	return pos
}

// txCarriesOtherWork reports whether the transaction handle is used for anything
// besides being opened, committed, rolled back and handed to outbox.Enqueue.
//
// THIS IS THE ASSERTION THAT MAKES THE GUARD MORE THAN A SHAPE CHECK. Every
// other condition is satisfied by a transaction opened purely to wrap the
// enqueue while the row goes to the pool — the exact dual write the outbox
// exists to remove, dressed as a transaction. A "use" is deliberately broad: a
// direct tx.Exec/tx.QueryRow, or the handle passed to a helper that does the
// write (services/oms/internal/order.Save reaches its CAS and its fill claim
// that way). Broad in the direction of MISSING a violation, never of inventing
// one.
func txCarriesOtherWork(s enqueueSite) bool {
	excluded := map[token.Pos]bool{}
	ast.Inspect(s.Fn.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			// The binding itself: `tx, err := pool.Begin(ctx)`.
			for _, l := range v.Lhs {
				if id, ok := l.(*ast.Ident); ok && id.Name == s.Tx {
					excluded[id.Pos()] = true
				}
			}
		case *ast.CallExpr:
			// tx.Commit(...) / tx.Rollback(...): lifecycle, not work.
			if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
				if sel.Sel.Name == "Commit" || sel.Sel.Name == "Rollback" {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == s.Tx {
						excluded[id.Pos()] = true
					}
				}
			}
			// The Enqueue call's own transaction argument.
			if isOutboxEnqueue(v) {
				for _, a := range v.Args {
					if id, ok := a.(*ast.Ident); ok && id.Name == s.Tx {
						excluded[id.Pos()] = true
					}
				}
			}
		}
		return true
	})

	other := false
	ast.Inspect(s.Fn.Body, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if ok && id.Name == s.Tx && !excluded[id.Pos()] {
			other = true
		}
		return true
	})
	return other
}

// isOutboxEnqueue reports whether a call is `<pkg>.Enqueue(...)`. The package
// qualifier is not pinned to the literal "outbox": scanOutboxImporters only
// hands this file's analysis calls from files that import internal/outbox, and
// an aliased import is resolved there.
func isOutboxEnqueue(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Enqueue" {
		return false
	}
	_, ok = sel.X.(*ast.Ident)
	return ok
}

// scanOutboxImporters walks the whole module and derives, from the source alone:
// every outbox.Enqueue call site, the packages that contain one, the packages
// that return an outbox.Queue from a function, and the packages that construct a
// relay over one.
//
// The walk is rooted at the module and uses the shared skip set (walk_skip_test.go)
// so it does not descend into an agent worktree and read another checkout's
// files as the estate's.
func scanOutboxImporters(t *testing.T) (sites []enqueueSite, enqueuers, exposers, relays map[string]bool) {
	t.Helper()
	root := moduleRoot(t)
	enqueuers, exposers, relays = map[string]bool{}, map[string]bool{}, map[string]bool{}

	imported := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A worktree removed mid-walk is the benign case; anything else is a
			// real problem and must not be swallowed into a silent pass.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			if skipWalkDir(d) {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		// Mode 0: comments are NOT attached, so no paragraph in the estate — or
		// in this file — can be mistaken for a call.
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// A parse error is a hard failure, not a skip: a guard that quietly
			// analyses zero files passes forever.
			t.Fatalf("parse %s: %v", path, perr)
		}
		// importAlias is the shared resolver (halt_reaches_the_order_path_test.go):
		// one answer to "what does this file call that package?", so an aliased
		// import cannot slip the analysis.
		alias := importAlias(file, outboxImportPath)
		if alias == "" {
			return nil
		}
		imported++

		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			t.Fatalf("relativise %s: %v", path, rerr)
		}
		rel = filepath.ToSlash(rel)
		pkg := filepath.ToSlash(filepath.Dir(rel))

		// Queue exposure: any function type — declared, literal, or an interface
		// method — whose RESULTS name <alias>.Queue.
		ast.Inspect(file, func(n ast.Node) bool {
			ft, ok := n.(*ast.FuncType)
			if !ok || ft.Results == nil {
				return true
			}
			for _, r := range ft.Results.List {
				if isQualified(r.Type, alias, "Queue") {
					exposers[pkg] = true
				}
			}
			return true
		})
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewRelay" {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == alias {
					relays[pkg] = true
				}
			}
			return true
		})

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Enqueue" {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok || id.Name != alias {
					return true
				}
				enqueuers[pkg] = true
				s := enqueueSite{
					Pkg:  pkg,
					File: rel,
					Func: funcLabel(fn),
					Line: fset.Position(call.Pos()).Line,
					Fn:   fn,
					Pos:  call.Pos(),
				}
				// Enqueue(ctx, tx, records...) — the transaction is argument 2.
				if len(call.Args) >= 2 {
					if txID, ok := call.Args[1].(*ast.Ident); ok {
						s.Tx = txID.Name
					}
				}
				sites = append(sites, s)
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if imported == 0 {
		t.Fatalf("no non-test file in the module imports %s — the import path changed and this "+
			"guard is now analysing nothing", outboxImportPath)
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].File != sites[j].File {
			return sites[i].File < sites[j].File
		}
		return sites[i].Line < sites[j].Line
	})
	return sites, enqueuers, exposers, relays
}

// funcLabel renders a function as it would be referred to in review:
// "(*Postgres).Append" for a method, "openStores" for a function.
func funcLabel(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	var b strings.Builder
	switch t := fn.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			b.WriteString("(*" + id.Name + ").")
		}
	case *ast.Ident:
		b.WriteString(t.Name + ".")
	}
	b.WriteString(fn.Name.Name)
	return b.String()
}
