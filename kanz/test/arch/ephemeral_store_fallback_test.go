package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A COMPOSITION ROOT THAT PICKS A MAP INSTEAD OF A DATABASE MUST SAY SO.
//
// This is #261, and the defect it guards is the quietest one this estate has
// produced. Six composition roots contained, verbatim:
//
//	if cfg.DatabaseURL == "" {
//	    return ledger.NewMemoryStore(), func() {}, nil
//	}
//
// No log, no gauge, no error. The process starts clean, /readyz answers 200,
// reads come back correct-looking, and everything the service accumulated is
// discarded by the next rollout. For accounting that store was the fund's book
// of record. THE DEPLOYMENT THAT FORGOT ITS DSN AND THE DEPLOYMENT THAT WAS
// CONFIGURED CORRECTLY LOOKED IDENTICAL FROM EVERY SIGNAL A HUMAN LOOKS AT,
// which is exactly what CLAUDE.md forbids twice over: "never a default that
// looks healthy", and "'nothing configured' and 'checked, and fine' must never
// look the same".
//
// WHY THIS IS A GUARD AND NOT A REVIEW HABIT. Composition roots are this
// repository's proven blind spot — no unit test constructs main.go's wiring, so
// every one of these branches was unexecuted by the suite that was green over
// them. The same blind spot shipped a service answering 401 to every request, an
// estate-wide swallowed exit code, and two producers publishing without a tenant.
// A rule that lives only in a paragraph does not survive the next service.
//
// WHAT SATISFIES IT, AND WHY THE THREE ARE NOT INTERCHANGEABLE.
//
//   - a Warn/Error log — the operator reading pod logs at boot learns the posture;
//   - a `.Set(0)` gauge — the posture becomes ALERTABLE rather than merely
//     readable, which is what matters after the boot log has scrolled off;
//   - a non-nil error return — the deployment that forgot never starts at all.
//
// Any one of the three clears the bar because any one of them breaks the
// silence, which is the defect. Which one is CORRECT is a per-site judgement
// this guard deliberately does not make: warn-and-continue is right where the
// in-memory store is correct under a stated precondition (the OMS's is correct
// at exactly one replica, and it says so), and refusal is right where no
// deployment shape makes it acceptable (accounting's ledger, regulatory's
// hash chain). Encoding that choice here would be this guard asserting a
// judgement it cannot see the evidence for; encoding the SILENCE is exactly
// what it can see.
//
// SCOPE AND LIMITS, stated so a green run is not read for more than it carries.
//
//   - services/*/cmd/** non-test Go only. That is where the defect class lives:
//     a library returning an in-memory implementation is a legitimate choice made
//     by its caller, and it is the CALLER — the composition root — that owes the
//     operator a statement.
//   - Build constraints are IGNORED (parser.ParseFile does not apply them), on
//     purpose: webhook-ingest's nonce store has one branch per build tag and both
//     are real deployments. A tagged file must not be able to hide a silent
//     fallback.
//   - It matches an `if` whose condition tests something for ABSENCE and whose
//     body constructs a New(Memory|InMemory|Noop|NoOp)* value. An UNCONDITIONAL
//     in-memory construction is a different defect in kind and is not matched —
//     services/lineage's graph.NewMemory() is one, and #244 bounded it rather
//     than making it conditional.
//   - It proves the call site exists, not that it executes. A Warn behind a
//     second condition nothing satisfies still passes. The per-service tests
//     (services/audit/cmd/audit/main_test.go and its siblings) are what execute
//     these branches.
func TestNoCompositionRootSilentlyFallsBackToAnInMemoryStore(t *testing.T) {
	root := moduleRoot(t)
	sites := ephemeralFallbackSites(t, root)

	// NON-VACUITY, half one. A walk that finds no candidate branches passes no
	// matter how many silent fallbacks the estate carries — broken guard, green
	// run. #261 enumerated the conditional sites by hand; there were eleven when
	// this landed, across nine services.
	if len(sites) < 8 {
		t.Fatalf("found only %d conditional in-memory fallback(s) under services/*/cmd/** — the walk or the "+
			"matcher is broken, not the estate (#261 enumerated eleven, in accounting, alternatives, audit, "+
			"datamaster, market-data, oms, regulatory, venue-binance, venue-okx, wealth and webhook-ingest)",
			len(sites))
	}
	// NON-VACUITY, half two. If the satisfaction detector stopped recognising a
	// Warn or a gauge, every site would be reported at once and the natural
	// response is to weaken the guard rather than read it.
	satisfied := 0
	for _, s := range sites {
		if len(s.reasons) > 0 {
			satisfied++
		}
	}
	if satisfied == 0 {
		t.Fatal("not one of the conditional in-memory fallbacks was recognised as speaking up — the " +
			"satisfaction detector is broken. The OMS, both venue adapters, audit and webhook-ingest all " +
			"warn, gauge or refuse today")
	}
	// The census, logged rather than asserted: `go test -v -run …` shows WHAT this
	// guard examined, so a future reader can tell a genuinely clean estate from a
	// matcher that quietly stopped seeing half of it. The floors above are the
	// assertion; this is the evidence behind them.
	t.Logf("examined %d conditional in-memory fallback(s) under services/*/cmd/**, %d of which speak up",
		len(sites), satisfied)
	for _, s := range sites {
		how := "SILENT"
		if len(s.reasons) > 0 {
			how = strings.Join(s.reasons, ", ")
		}
		t.Logf("  %-64s %s → %s", s.key(), s.ctor, how)
	}

	seenExempt := map[string]bool{}
	var problems []string
	for _, s := range sites {
		if len(s.reasons) > 0 {
			continue
		}
		if why, ok := silentEphemeralFallbacks[s.key()]; ok {
			seenExempt[s.key()] = true
			t.Logf("%s: exempt from the speak-up rule — %s", s.key(), why)
			continue
		}
		problems = append(problems, fmt.Sprintf("%s  %s selects %s when %s is absent, and says nothing",
			s.key(), s.fn, s.ctor, s.cond))
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("%d composition root(s) fall back to an in-memory store in silence:\n  %s\n\n"+
			"Each of these starts clean, reports /readyz 200 and answers reads that look correct, right up "+
			"until a restart discards everything the service accumulated — and nothing in the process ever "+
			"said which store it picked. Decide per site which of the two postures is right, and make it "+
			"visible:\n\n"+
			"  WARN + GAUGE where the in-memory store is CORRECT UNDER A STATED PRECONDITION. Log the "+
			"precondition and the consequence, and Set(0) a durability gauge so the posture is alertable "+
			"after the boot log scrolls off. See services/venue-binance/cmd/venue-binance/main.go openView "+
			"and services/oms/cmd/oms/main.go openStores.\n\n"+
			"  REFUSE TO START where no deployment shape makes it acceptable. Return the error from the "+
			"config-validation path so the pod exits 2 rather than serving. See "+
			"services/tv-sync/internal/config/config.go validateBook (no escape hatch at all) and "+
			"services/audit/cmd/audit/main.go openStore (refuses unless an operator opted in OUT LOUD, and "+
			"gauges it when they did).\n\n"+
			"Adding an entry to silentEphemeralFallbacks means writing down that this service's state may be "+
			"discarded without warning, and it must carry the issue that retires it.",
			len(problems), strings.Join(problems, "\n  "))
	}

	// DEAD-ENTRY CHECK, the same shape as metricsWithoutAWriter in
	// metric_writer_test.go. An exemption that no longer names a real branch
	// protects nothing, reads as a considered decision, and silently covers the
	// next fallback that lands at the same address.
	var dead []string
	for key := range silentEphemeralFallbacks {
		if !seenExempt[key] {
			dead = append(dead, key)
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("silentEphemeralFallbacks names %d branch(es) that no longer exist or no longer fall back "+
			"in silence:\n\n  %s\n\nThe repair happened — delete the entry. A stale exemption is a standing "+
			"permission nobody granted.", len(dead), strings.Join(dead, "\n  "))
	}
}

// silentEphemeralFallbacks is a DEFAULT-DENY allow-list keyed "<file>:<line>":
// every conditional in-memory fallback under services/*/cmd/** must speak up
// unless it is named here with a reason and the issue that removes it.
//
// IT IS EMPTY, AND THAT IS THE ENTIRE POINT OF #261. This guard was designed
// while fixing #236 and deliberately NOT landed then, because at that moment it
// would have needed six exemptions — accounting, alternatives, datamaster,
// market-data, regulatory and wealth — and this repository requires every
// exemption to name the issue retiring it. Six carve-outs is not a guard, it is
// a list of known defects with a test around it. #261 repaired all six first, so
// this landed with nothing to excuse.
//
// Leave the map rather than deleting it. An empty default-deny list is a working
// guard with nothing carved out; removing the mechanism means the next person
// who needs a temporary exemption reaches for weakening the check instead.
var silentEphemeralFallbacks = map[string]string{}

// ephemeralSite is one `if <absence> { … New*Memory*… }` branch found in a
// composition root, reduced to what a failure message needs.
type ephemeralSite struct {
	file    string // module-relative, forward slashes
	line    int
	fn      string   // enclosing function name
	cond    string   // the absence test, rendered back to source-ish text
	ctor    string   // the in-memory constructor the branch builds
	reasons []string // how it speaks up; empty ⇒ it does not
}

func (s ephemeralSite) key() string { return fmt.Sprintf("%s:%d", s.file, s.line) }

// inMemoryCtor names a constructor that builds a value living only in this
// process. Anchored at New so a domain type called `Memory` (a memory-limit
// setting, say) is not mistaken for a store.
var inMemoryCtor = regexp.MustCompile(`^New(Memory|InMemory|Noop|NoOp)`)

// ephemeralFallbackSites walks services/*/cmd/** and returns every conditional
// in-memory fallback, each annotated with how — or whether — it speaks up.
func ephemeralFallbackSites(t *testing.T, root string) []ephemeralSite {
	t.Helper()

	servicesDir := filepath.Join(root, "services")
	var out []ephemeralSite

	err := filepath.WalkDir(servicesDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		// services/<svc>/cmd/** only. A service's internal/ packages are libraries;
		// the composition root is what owes the operator a statement.
		parts := strings.Split(rel, "/")
		if len(parts) < 4 || parts[0] != "services" || parts[2] != "cmd" {
			return nil
		}

		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			// A file this guard cannot parse is a hole it cannot see through, not a
			// pass. Silence here is how the next silent fallback hides.
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		out = append(out, sitesInFile(fset, f, rel)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", servicesDir, err)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].line < out[j].line
	})
	return out
}

func sitesInFile(fset *token.FileSet, f *ast.File, rel string) []ephemeralSite {
	var out []ephemeralSite
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			ifs, ok := n.(*ast.IfStmt)
			if !ok {
				return true
			}
			if !testsForAbsence(ifs.Cond) {
				return true
			}
			ctor := inMemoryCtorIn(ifs.Body)
			if ctor == "" {
				return true
			}
			out = append(out, ephemeralSite{
				file:    rel,
				line:    fset.Position(ifs.Pos()).Line,
				fn:      fd.Name.Name,
				cond:    renderCond(ifs.Cond),
				ctor:    ctor,
				reasons: speaksUp(ifs.Body),
			})
			return true
		})
	}
	return out
}

// testsForAbsence reports whether cond asks "is this thing missing?" — the three
// spellings this estate uses for a config field that was never set:
//
//	cfg.DatabaseURL == ""      cfg.Pool == nil      !cfg.AllowEphemeralLog
//
// `len(x) == 0` and `x == 0` come along with the empty-literal case for free.
//
// It is deliberately generous, because it is only half of the match: a bare `!ok`
// qualifies here and is then discarded unless the same branch also constructs an
// in-memory store. Tightening this half would cost more in missed fallbacks than
// the conjunction costs in false positives — of which, across the whole estate,
// there are currently none.
func testsForAbsence(cond ast.Expr) bool {
	found := false
	ast.Inspect(cond, func(n ast.Node) bool {
		if found {
			return false
		}
		switch v := n.(type) {
		case *ast.BinaryExpr:
			if v.Op == token.EQL && (isZeroValue(v.X) || isZeroValue(v.Y)) {
				found = true
			}
		case *ast.UnaryExpr:
			// !cfg.Flag — "the operator did not opt in".
			if v.Op == token.NOT && isNamedOperand(v.X) {
				found = true
			}
		}
		return !found
	})
	return found
}

// isZeroValue reports whether e is the empty string, nil, or 0 — the right-hand
// side of an absence test.
func isZeroValue(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name == "nil"
	case *ast.BasicLit:
		return (v.Kind == token.STRING && (v.Value == `""` || v.Value == "``")) ||
			(v.Kind == token.INT && v.Value == "0")
	}
	return false
}

// isNamedOperand reports whether e is a plain name or a field selector, i.e.
// something a config flag can be spelled as.
func isNamedOperand(e ast.Expr) bool {
	switch e.(type) {
	case *ast.Ident, *ast.SelectorExpr:
		return true
	}
	return false
}

// inMemoryCtorIn returns the first in-memory constructor called anywhere in body,
// or "".
//
// Anywhere in the body, not only in a return: `s := ledger.NewMemoryStore()`
// followed by `return s, …` is the same defect written over two lines, and a
// matcher that only reads return statements is one refactor away from blind.
func inMemoryCtorIn(body *ast.BlockStmt) string {
	name := ""
	ast.Inspect(body, func(n ast.Node) bool {
		if name != "" {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			if inMemoryCtor.MatchString(fn.Sel.Name) {
				name = renderCond(fn)
			}
		case *ast.Ident:
			if inMemoryCtor.MatchString(fn.Name) {
				name = fn.Name
			}
		}
		return name == ""
	})
	return name
}

// speaksUp returns every way body breaks the silence. Empty ⇒ it does not.
func speaksUp(body *ast.BlockStmt) []string {
	var reasons []string
	add := func(r string) {
		if !contains(reasons, r) {
			reasons = append(reasons, r)
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.ReturnStmt:
			// A non-nil error in the LAST result position is a refusal. Every store
			// opener in this estate returns error last, and `return store, close, nil`
			// — the silent path — is precisely the case that must not count.
			if len(v.Results) > 0 && !isNilIdent(v.Results[len(v.Results)-1]) {
				add("returns an error")
			}
		case *ast.CallExpr:
			sel, ok := v.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "Warn", "WarnContext":
				add("logs a Warn")
			case "Error", "ErrorContext":
				// err.Error() takes no arguments; logger.Error always has a message.
				// Without this, an `if err != nil` branch mentioning err.Error() would
				// read as an operator-visible warning.
				if len(v.Args) > 0 {
					add("logs an Error")
				}
			case "Set":
				if len(v.Args) == 1 {
					if lit, ok := v.Args[0].(*ast.BasicLit); ok && lit.Kind == token.INT && lit.Value == "0" {
						add("sets a durability gauge to 0")
					}
				}
			}
		}
		return true
	})
	sort.Strings(reasons)
	return reasons
}

func isNilIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

// renderCond turns an expression back into something readable for a failure
// message. Not a formatter — just enough of one that the reported condition is
// recognisable in the file it came from.
func renderCond(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return renderCond(v.X) + "." + v.Sel.Name
	case *ast.BasicLit:
		return v.Value
	case *ast.UnaryExpr:
		return v.Op.String() + renderCond(v.X)
	case *ast.BinaryExpr:
		return renderCond(v.X) + " " + v.Op.String() + " " + renderCond(v.Y)
	case *ast.CallExpr:
		args := make([]string, 0, len(v.Args))
		for _, a := range v.Args {
			args = append(args, renderCond(a))
		}
		return renderCond(v.Fun) + "(" + strings.Join(args, ", ") + ")"
	case *ast.ParenExpr:
		return "(" + renderCond(v.X) + ")"
	}
	return "<expr>"
}

// TestEphemeralFallbackGuardSeparatesSilenceFromSpeech is the guard's own proof
// of work.
//
// The count floor above shows the walk found SOMETHING; it does not show the
// analysis tells a silent fallback from a loud one. This runs both through the
// same functions the guard uses and asserts it separates them, so an edit that
// makes the matcher permissive fails HERE rather than turning the real guard
// silently green — the failure mode that let #261's six sites sit unnoticed
// under a suite that was green the whole time.
//
// The fixture carries the shapes that actually broke drafts of this matcher:
//
//   - `silentTwoLine` constructs the store into a local and returns the local.
//     A matcher that only reads return statements calls it clean.
//   - `looksLikeSpeech` returns `nil, nil, nil` after calling err.Error(). Both
//     a nil last result and a zero-argument Error() must fail to count, or the
//     guard is satisfied by a branch that tells the operator nothing.
//   - `unconditional` is services/lineage's shape — an in-memory store built with
//     no absence test at all. It must not be matched: #261 says so explicitly,
//     and #244 bounded it a different way.
func TestEphemeralFallbackGuardSeparatesSilenceFromSpeech(t *testing.T) {
	const src = `package main

func silentReturn(cfg Config) (Store, func(), error) {
	if cfg.DatabaseURL == "" {
		return ledger.NewMemoryStore(), func() {}, nil
	}
	return ledger.NewPostgres(nil), func() {}, nil
}

func silentTwoLine(cfg Config) (Store, func(), error) {
	if cfg.DatabaseURL == "" {
		s := book.NewMemoryStore()
		return s, func() {}, nil
	}
	return book.NewPostgres(nil), func() {}, nil
}

func looksLikeSpeech(cfg Config, err error) (Store, func(), error) {
	if cfg.DatabaseURL == "" {
		_ = err.Error()
		return store.NewMemory(), func() {}, nil
	}
	return store.NewPostgres(nil), func() {}, nil
}

func warns(cfg Config, logger *slog.Logger) (Store, func(), error) {
	if cfg.DatabaseURL == "" {
		logger.Warn("IN-MEMORY — this deployment must run exactly one replica")
		return order.NewMemoryStore(), func() {}, nil
	}
	return order.NewPostgres(nil), func() {}, nil
}

func gauges(cfg Config, logger *slog.Logger) (Store, func(), error) {
	if cfg.DatabaseURL == "" {
		logger.Warn("ORDER VIEW IS IN-MEMORY")
		orderViewDurable.Set(0)
		return orderview.NewMemory(), func() {}, nil
	}
	return orderview.NewPostgres(nil), func() {}, nil
}

func refuses(cfg Config, logger *slog.Logger) (Store, func(), error) {
	if cfg.DatabaseURL == "" {
		if !cfg.AllowEphemeral {
			return nil, nil, errors.New("no DSN: the book of record would be IN-MEMORY")
		}
		logger.Warn("EPHEMERAL")
		bookDurable.Set(0)
		return audit.NewMemory(), func() {}, nil
	}
	return audit.NewPostgres(nil), func() {}, nil
}

func notOptedIn(cfg Config) (Store, error) {
	if !cfg.AllowEphemeral {
		return nil, errors.New("opt in")
	}
	return nil, nil
}

func unconditional() Graph {
	return graph.NewMemory(graph.WithEventIndexCapacity(1000))
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "sample.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}

	got := map[string]string{}
	for _, s := range sitesInFile(fset, f, "sample.go") {
		if len(s.reasons) == 0 {
			got[s.fn] = "silent"
			continue
		}
		got[s.fn] = strings.Join(s.reasons, "+")
	}

	want := map[string]string{
		"silentReturn":    "silent",
		"silentTwoLine":   "silent",
		"looksLikeSpeech": "silent",
		"warns":           "logs a Warn",
		"gauges":          "logs a Warn+sets a durability gauge to 0",
		"refuses":         "logs a Warn+returns an error+sets a durability gauge to 0",
	}
	for fn, expect := range want {
		if got[fn] != expect {
			t.Errorf("%s: analyzer says %q, want %q — the guard no longer separates a silent in-memory "+
				"fallback from one that speaks up, so TestNoCompositionRootSilentlyFallsBackToAnInMemoryStore "+
				"is green for the wrong reason", fn, got[fn], expect)
		}
	}
	// notOptedIn tests a flag for absence but builds no store; unconditional builds
	// one with no test at all. Matching either would make the guard fire on code
	// that carries none of this defect — and #261 rules the second one out by name.
	for _, fn := range []string{"notOptedIn", "unconditional"} {
		if r, matched := got[fn]; matched {
			t.Errorf("%s was matched as a conditional in-memory fallback (%q), and it is not one", fn, r)
		}
	}
	if len(got) != len(want) {
		t.Errorf("analyzer found %d site(s) in the fixture, want %d: %v", len(got), len(want), got)
	}
}
