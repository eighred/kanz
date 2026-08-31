package arch

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// NOTHING IN THIS REPOSITORY MAY BUILD A RISK QUERY THAT PINS AN as_of (#859).
//
// # Why this exists, and it is not hypothetical
//
// The engine now REFUSES a pinned exposure/measures query with
// INVALID_ARGUMENT, because it cannot honour one: it would answer from live
// state and stamp the response with the live timestamp, which is a confidently
// wrong answer rather than a slightly stale one.
//
// The refusal landed with a blast-radius survey that checked the Go services and
// the web client and concluded "no first-party caller sets as_of". That was
// WRONG, and the miss cost a red build on a merged PR: kanz/test/load/config.js
// sent `?as_of=<now>` on every measures read. Before the refusal it got a 200 —
// because the field was ignored — so the load harness had been reporting green
// read-path throughput for a contract nothing honoured. After it, 46.5% of the
// smoke's checks failed and the latency gate went red.
//
// That is the failure mode this guard closes. A caller pinning a risk query is
// now a 400, and the places that build such a URL are spread across JavaScript,
// shell, YAML and runbook Markdown — none of which any Go test or compiler can
// see. `go build` cannot catch it, `go vet` cannot, and the only signal left was
// a k6 threshold breach in a separate workflow, which reports "46% of checks
// failed" rather than "this URL is refused".
//
// # What it checks
//
// A text scan for a URL naming the risk exposure or measures endpoint that also
// carries an `as_of` query parameter. Deliberately NOT Go-aware: the callers
// that matter here are exactly the non-Go ones, and the Go side is already held
// by the type system plus TestEveryRiskRequestFieldIsReadByTheEngine.
//
// _test.go files are excluded because the gateway's and grpcsrv's own refusal
// tests must build precisely these URLs to prove the 400 — a guard that
// forbade them would forbid the proof.
//
// # What it deliberately does NOT match
//
// Other endpoints' as_of are legitimate and honoured by their own handlers, and
// each was checked when this was written:
//
//   - `/v1/nav/{portfolio}?as_of=` (infra/dr/README.md) — the IBOR ledger,
//     kanz-books, which is bitemporal and answers a real point-in-time query;
//   - `/broker/accounts/.../positions?as_of=` (services/tv-sync) — honoured
//     there, "what we knew then" is that surface's whole purpose;
//   - `/v1/prices/{instrument}?as_of=` (api-gateway proxy) — market data;
//   - kanz-py's `as_of=` on feature vectors — a different domain object.
//
// So the pattern is anchored to the two REFUSING endpoints rather than to the
// parameter name. Widening it to any `as_of=` would fail four correct callers
// and get the guard deleted.
func TestNoCallerPinsARiskQuery(t *testing.T) {
	repoRoot := filepath.Dir(moduleRoot(t))

	// A risk read URL: .../exposure or .../measures, with an as_of parameter
	// somewhere in the same query string. Either order — as_of may be the first
	// parameter or follow another.
	pinned := regexp.MustCompile(`/(exposure|measures)\?[^"'\s` + "`" + `]*\bas_of=`)

	// NON-VACUITY: the scanner must reach real files. A walk that reads nothing
	// passes trivially, which is the shape of a guard that is green because it is
	// broken.
	scanned := 0
	var offenders []string

	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable path is not this guard's finding
		}
		if skipWalkDir(d) {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		// Go TEST files must be able to build these URLs: they are how the
		// refusal is proven to reach a caller as a 400.
		if strings.HasSuffix(name, "_test.go") {
			return nil
		}
		switch filepath.Ext(name) {
		case ".go", ".js", ".ts", ".tsx", ".sh", ".md", ".yaml", ".yml", ".py", ".json":
		default:
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		scanned++
		rel, _ := filepath.Rel(repoRoot, path)
		for i, line := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
			// A line that merely DISCUSSES the parameter is not a caller. This
			// guard's own subject is heavily commented — config.js carries a
			// paragraph naming `?as_of=` to stop it being re-added — and matching
			// prose is how three guards in this repository came to pass with the
			// checked thing deleted.
			//
			// WHOLE-LINE COMMENTS ONLY, NEVER TRUNCATION AT THE FIRST MARKER. The
			// first version of this cut each line at its first "//" and lost every
			// ABSOLUTE URL to the "//" in its own scheme: a literal
			// "http://gw/v1/portfolios/PF1/measures?as_of=..." became
			// "http:" and matched nothing. Caught by mutation, and it is the exact
			// shape of blind spot that makes a guard read as green while the
			// defect it names sits in the tree.
			if isWholeLineComment(line) {
				continue
			}
			if pinned.MatchString(line) {
				offenders = append(offenders,
					filepath.ToSlash(rel)+":"+strconv.Itoa(i+1)+"\n      "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", repoRoot, err)
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d files under %s — the walk is broken, not the callers. "+
			"This guard asserts nothing if it does not reach the tree", scanned, repoRoot)
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("these callers pin a risk query to a point in time, which the engine REFUSES:\n\n  %s\n\n"+
			"internal/risk/engine answers a non-zero as_of with ErrAsOfNotSupported, which "+
			"reaches the caller as INVALID_ARGUMENT and a 400 (#859). It cannot be honoured: "+
			"the engine holds only the latest applied state, so answering a pin would return "+
			"today's book stamped with today's timestamp — internally consistent and wrong.\n\n"+
			"Drop the parameter to query the latest state, which is what these call sites were "+
			"actually getting all along. Do not re-add it to exercise the pin: when as_of is "+
			"honoured it will need a point-in-time read of risk state, and the caller belongs "+
			"in the change that builds it.",
			strings.Join(offenders, "\n\n  "))
	}

	t.Logf("scanned %d files; no caller pins a risk exposure or measures query", scanned)
}

// isWholeLineComment reports whether the line is entirely prose in one of the
// comment syntaxes this scan crosses: Go/JS/TS (//), shell/YAML/Python (#), a
// continued block comment (*), and HTML/Markdown (<!--).
//
// A TRAILING comment is deliberately NOT stripped. Doing so requires knowing
// where string literals end — the problem that produced this guard's own blind
// spot — and the residual risk runs the safe way for a default-deny check: a
// code line whose trailing comment happens to quote a pinned risk URL fails the
// build with a message naming the file and line, which a reader resolves in
// seconds. The opposite error is silent.
func isWholeLineComment(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "//") ||
		strings.HasPrefix(t, "#") ||
		strings.HasPrefix(t, "*") ||
		strings.HasPrefix(t, "<!--")
}
