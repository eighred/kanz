package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A SWITCH THAT TRIES TO ENUMERATE EVERY ORDER STATUS MUST ACTUALLY ENUMERATE
// EVERY ORDER STATUS.
//
// # The defect
//
// order.v1.OrderStatus gained ORDER_STATUS_WORKING_SCHEDULED in #435 — a parent
// order being sliced over a window. Adding an enum value is deliberately
// NON-BREAKING on the wire (kanz-schemas/docs/schema-evolution.md §4), and that
// is what makes this dangerous rather than safe: nothing in buf, the compiler,
// or `go vet` notices that a switch somewhere has stopped covering its own
// domain. Go has no exhaustiveness check for enums, because a protobuf enum is
// an int32.
//
// It had already happened when this guard was written. tv-sync's statusString
// mapped every other status to a name and fell through to "unspecified" — so a
// live parent order rendered on the operator's blotter as UNKNOWN, in a way that
// could not be told apart from a decode failure. The order was fine; the platform
// simply had no word for what it was doing.
//
// That is the "unknown is not zero" failure in its cheapest disguise: a `default`
// arm that was written to catch corruption quietly acquires a legitimate,
// everyday value.
//
// # What counts as "trying to be exhaustive"
//
// A switch that names ALL FOUR terminal statuses AND at least one non-terminal
// one is mapping the whole lifecycle, and must name every status. A switch that
// names only the terminal four is asking a narrower question — "is this order
// finished" — and its default is CORRECT for a new non-terminal value, which is
// why aggregate.go's IsTerminal is not caught here and must not be.
//
// The rule is deliberately shape-based rather than a list of files: a list would
// need editing by the same person who forgot the switch.
//
// # What it cannot check
//
// A switch that handles the new value by falling into an existing case on
// purpose, or one written with if/else instead of switch. This is a tripwire on
// the shape the real defect had, not a proof of exhaustiveness — which is why
// the behaviour is also asserted where it lives.

// statusScopes are the trees searched.
var statusScopes = []string{"services", "internal", "cmd", "tools", "pkg"}

// terminalStatuses are the four an order can finish in. A switch naming all of
// them plus any non-terminal status is enumerating the lifecycle.
var terminalStatuses = []string{
	"ORDER_STATUS_FILLED",
	"ORDER_STATUS_CANCELLED",
	"ORDER_STATUS_REJECTED",
	"ORDER_STATUS_EXPIRED",
}

// nonTerminalStatuses are the working states. WORKING_SCHEDULED is the one this
// guard is about and is checked separately.
var nonTerminalStatuses = []string{
	"ORDER_STATUS_PENDING_NEW",
	"ORDER_STATUS_ROUTED",
	"ORDER_STATUS_PARTIALLY_FILLED",
}

const scheduledStatus = "ORDER_STATUS_WORKING_SCHEDULED"

// statusExhaustiveExempt maps "file:switch-index" to an argued reason that switch
// may omit the scheduled status, and what retires the entry.
//
// EMPTY. An entry here is a decision that some surface will describe a live
// parent order as something other than what it is.
var statusExhaustiveExempt = map[string]string{}

func TestEveryExhaustiveOrderStatusSwitchNamesTheScheduledStatus(t *testing.T) {
	root := moduleRoot(t)

	var offenders []string
	seenExempt := map[string]bool{}
	scanned, exhaustive, covering := 0, 0, 0

	for _, scope := range statusScopes {
		for _, gf := range goFilesUnder(t, filepath.Join(root, scope)) {
			if strings.HasSuffix(gf.rel, "_test.go") {
				// A TEST MAY ENUMERATE A SUBSET. It is asserting about the cases it
				// names, not claiming to cover the enum.
				continue
			}
			scanned++
			rel := filepath.ToSlash(filepath.Join(scope, gf.rel))

			for i, body := range splitSwitches(t, filepath.Join(root, scope, gf.rel), gf.body) {
				if !namesAll(body, terminalStatuses) || !namesAny(body, nonTerminalStatuses) {
					continue // not trying to enumerate the lifecycle
				}
				exhaustive++
				key := rel + ":" + itoa(i)
				if strings.Contains(body, scheduledStatus) {
					covering++
					continue
				}
				if reason, ok := statusExhaustiveExempt[key]; ok {
					seenExempt[key] = true
					t.Logf("%s: exempt — %s", key, reason)
					continue
				}
				offenders = append(offenders, key)
			}
		}
	}

	// NON-VACUITY, the walk half: a moved tree scans nothing and passes.
	if scanned < 100 {
		t.Fatalf("scanned only %d non-test files across %v — the walk is broken, not the estate",
			scanned, statusScopes)
	}
	// NON-VACUITY, the match half: if the status identifiers are renamed or the
	// switch splitter breaks, this finds no lifecycle switches at all and passes
	// while asserting nothing.
	if exhaustive == 0 {
		t.Fatalf("found NO switch enumerating the order lifecycle anywhere in %v — the status "+
			"identifiers or the switch splitter stopped matching, and this guard is asserting "+
			"nothing", statusScopes)
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("%d switch(es) enumerate the order lifecycle without naming %s: %v.\n"+
			"Adding an enum value is non-breaking on the wire, so nothing — not buf, not the "+
			"compiler, not go vet — notices when a switch stops covering its domain. A `default` "+
			"arm written to catch corruption quietly acquires a legitimate, everyday value: "+
			"tv-sync rendered a live parent order as \"unspecified\" on the operator's blotter, "+
			"indistinguishable from a decode failure. Name the status, or add an argued entry to "+
			"statusExhaustiveExempt.\n"+
			"The switch index counts switch statements in the file from 0.",
			len(offenders), scheduledStatus, offenders)
	}

	// NON-VACUITY, the coverage half — AND IT RUNS AFTER THE OFFENDER CHECK, ON
	// PURPOSE.
	//
	// It answers "is this guard matching what it thinks it is", which is only a
	// meaningful question when nothing was reported. Run FIRST it would fire on
	// the real regression too, because there is presently exactly one lifecycle
	// switch in the module: reverting tv-sync's fix drives both `covering` to zero
	// AND puts that switch in offenders, and a Fatal here would have reported "the
	// guard is broken" for a genuine defect. Diagnosis matters more when a guard
	// fires than when it passes.
	if len(offenders) == 0 && covering == 0 {
		t.Fatalf("found %d lifecycle switch(es), none offending, and NOT ONE names %s — nothing "+
			"in this module handles a scheduled parent order, so this guard is asserting a "+
			"property no code has", exhaustive, scheduledStatus)
	}

	// DEAD-ENTRY ARM: an exemption whose switch now covers the status, moved, or
	// was deleted has outlived its repair.
	for key, reason := range statusExhaustiveExempt {
		if !seenExempt[key] {
			t.Errorf("exemption for %q (%s) matches no uncovered lifecycle switch — remove the "+
				"entry so it cannot silently license the next one", key, reason)
		}
	}
}

// splitSwitches returns, for each switch statement in a file, the source text of
// its CASE EXPRESSIONS ONLY — never its body.
//
// # Why the cases and not the body
//
// A switch is deciding on the order lifecycle when its CASES are order statuses.
// A switch that merely mentions statuses in its RESULTS is doing something else,
// and two real examples proved that distinction is not academic:
//
//   - binanceStatusToProto switches on a Binance status STRING and returns Kanz
//     statuses. It names six of them, all four terminal ones among them — but
//     the exchange has no concept of a Kanz schedule, so there is no venue string
//     that could map to ORDER_STATUS_WORKING_SCHEDULED and its default is right.
//     Judging it by its body would demand a case that cannot exist.
//   - aggregate.go's IsTerminal DOES switch on a status, names exactly the four
//     terminal ones, and is correct to default — which the terminal-plus-one rule
//     already excludes, because none of its cases is non-terminal.
//
// # And why it parses rather than cutting at the next `switch`
//
// The first version of this did the cheap thing and was wrong in the dangerous
// direction: a body cut at the START OF THE NEXT SWITCH swallows everything in
// between, so IsTerminal absorbed the Route() function below it, picked up
// ORDER_STATUS_ROUTED from its assignment, and was reported as a lifecycle switch
// missing a case. Bleeding forward makes a NARROW switch look exhaustive — a
// false accusation, not a missed offender.
func splitSwitches(t *testing.T, path, body string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, body, 0)
	if err != nil {
		// A file this walk cannot parse is not evidence of anything; the compiler
		// owns that failure and reports it far better than this guard would.
		return nil
	}
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok || sw.Body == nil {
			return true
		}
		var cases strings.Builder
		for _, stmt := range sw.Body.List {
			cc, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue
			}
			for _, expr := range cc.List { // nil for `default`, which names nothing
				lo := fset.Position(expr.Pos()).Offset
				hi := fset.Position(expr.End()).Offset
				if lo >= 0 && hi <= len(body) && lo < hi {
					cases.WriteString(body[lo:hi])
					cases.WriteByte('\n')
				}
			}
		}
		out = append(out, cases.String())
		return true
	})
	return out
}

func namesAll(body string, names []string) bool {
	for _, n := range names {
		if !strings.Contains(body, n) {
			return false
		}
	}
	return true
}

func namesAny(body string, names []string) bool {
	for _, n := range names {
		if strings.Contains(body, n) {
			return true
		}
	}
	return false
}

// itoa avoids pulling strconv in for one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
