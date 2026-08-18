package arch

// THE OUTBOX GUARDS (#292).
//
// The OMS commits an order's ORDER_ACCEPTED FACT in the same transaction as the
// order itself, and a relay publishes it. Two things have to stay true for that
// to be worth anything, and neither is visible in a unit test:
//
//  1. NO NEW COMMIT-THEN-PUBLISH PAIR. The whole point of #292 is that adding a
//     FACT to a transition should not require adding a fourth hand-rolled
//     compensator. Every method that both writes and publishes is named below,
//     with the reason; one that is NOT named is the defect arriving by
//     instalment again.
//  2. THE OUTBOX MUST HAVE A DRAIN. A table nobody drains is worse than no
//     table: the OMS admits orders, commits their FACTs and tells nobody, with
//     the store, the handler and every health check reporting success. This is
//     the "a thing constructed must also be consumed" shape #283's
//     metric_writer_test.go established.
//  3. THE FILL FACT HAS NO WAY OUT EXCEPT THE TRANSACTION. Guard (1) would catch
//     a reintroduced EmitFill in work() or adopt() only as "an unnamed pair",
//     which reports the method and not the thing that matters; and it cannot see
//     the other two halves at all — that Emitter.FillFact still EXISTS and is
//     still handed to store.Save, and that no SECOND builder for a fill event
//     has appeared beside Emitter.fillEvent. A fill is the one FACT nothing can
//     rebuild, so it is asserted directly rather than inferred from the shape of
//     the method that emits it.
//
// # WHAT THE FIRST GUARD CAN AND CANNOT SEE
//
// It is a syntactic analysis of ONE package, over method calls on the Service
// receiver, closed transitively. That is sound for how this package is written
// — every write is s.store.X and every publish is s.emitter.EmitX or a helper
// that reaches one — and it is NOT a general call-graph analysis. It cannot
// follow a publish through an interface value, a function-typed field or another
// package. Those are named here rather than left for a reader to discover,
// because a guard whose blind spots are unwritten is a guard people trust too
// far. It errs toward MISSING a violation, never toward inventing one, so it
// cannot block honest work; the tests are what cover the rest.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// omsOrderPkg is the package the pair guard analyses.
const omsOrderPkg = "services/oms/internal/order"

// storeWriteMethods are the calls that change durable order state. Both are on
// the order.Store interface; a third would have to be added here, which is
// itself the point — the set is small and the guard names it.
var storeWriteMethods = map[string]bool{"Create": true, "Save": true}

// THE EXEMPTIONS ARE IN TWO MAPS, AND THE SPLIT IS THE POINT.
//
// They were one map, and every entry named #292 as the issue that retires it.
// That was true when the work started and is not true now: NONE of these is
// waiting on anything, and an exemption pointing at an issue that will never
// retire it is the same "reads as load-bearing" problem the *_announced_at
// markers had — with the added cost that #292 could not close while its own
// guard said six sites were pending.
//
// So: commitThenPublishPending is WORK. commitThenPublishByDesign is NOT. The
// split is what let the pending map empty out — the measure of #292 being
// finished — while five methods stay exempted for a reason that is a decision
// rather than a deferral.
//
// A method in EITHER map that no longer offends fails the dead-entry check
// below — an exemption must not outlive its repair, whichever kind it is.

// commitThenPublishByDesign names sites where no transaction exists for the FACT
// to ride in, so "convert it" is not a smaller change but a different, wrong
// one. These do not retire.
var commitThenPublishByDesign = map[string]string{
	"completeCancelAnnouncement": "BY DESIGN: this is a COMPENSATOR, and since the cancel pair " +
		"converted it is ONLY that. Its purpose is to republish for rows that have NO outbox " +
		"record — the population saved CANCELLED by the previous code, whose two publishes failed. " +
		"Routing it through the outbox means writing a record for the case defined by not having " +
		"one. It publishes and THEN Saves cancel_announced_at, which is the correct order for a " +
		"compensator: the marker must not claim an announcement that has not gone out. It has a " +
		"retirement condition, stated beside the function — an operational claim about the data, " +
		"not a code change.",

	// RENAMED, NOT CONVERTED (#539). handleSubmit became a thin wrapper that
	// unmarshals and delegates; the admission body it used to hold is now submit,
	// which additionally takes the dual-control Approval that releases a held
	// order. The argument below is unchanged because the code is unchanged — what
	// moved is the name the guard keys on, and this comment is here so the next
	// reader does not have to reconstruct that from git.
	"submit": "BY DESIGN: every write on this path already carries its FACT. Admission's " +
		"ACCEPTED rides store.Create, the ErrUnpriced reject's ORDER_REJECTED + outcome ride one " +
		"Save with outcome_announced_at, and a submit that FILLED rides the marker Save through " +
		"markOutcomeAnnounced. WHAT KEEPS IT LISTED is what has no write to ride: the pre-admission " +
		"refusals (refuse/outcomeReject, reached before anything is stored) and the trailing " +
		"ACCEPTED outcome for an order that did NOT fill — the order is resting or working, nothing " +
		"terminal happened, and stamping outcome_announced_at there would make a later fill's " +
		"outcome look already-announced to resume(). Converting it would mean an UPDATE whose only " +
		"purpose is to carry the record, which is the trade EmitOutcome's own comment refuses.",

	"handleCancel": "BY DESIGN: the write and the remaining publishes are in DISJOINT BRANCHES. " +
		"The live cancel IS converted — CancelledFact + OutcomeFact ride the same Save as the " +
		"CANCELLED state, and cancel_announced_at is stamped in that write rather than two writes " +
		"later. What remains publishes without writing: the refusal branches (unknown order, not " +
		"entitled, quarantined, terminal) reach outcomeReject having stored nothing, and the " +
		"already-CANCELLED branch calls completeCancelAnnouncement, which is a compensator for rows " +
		"with no outbox record. It would stop being listed only if those helpers stopped being " +
		"reachable from a method that also writes.",

	"handleAmend": "BY DESIGN: the write and the publishes are in DISJOINT BRANCHES. The success " +
		"path IS converted — OutcomeFact rides the Save. What remains are the reject branches " +
		"(quarantine, and Amend's RejectError), which reach outcomeReject WITHOUT writing " +
		"anything: no transaction for that outcome to ride in, and nothing to lose if the publish " +
		"fails, because no state changed. It would stop being listed only if outcomeReject stopped " +
		"being reachable from a method that also writes — a refactor of the helper, not of this " +
		"transition.",

	"resume": "BY DESIGN: the RECOVERY path, with no pair of its own. It Saves venue_ack_at on the " +
		"ActionLeave branch and reaches publishes through reannounceAccepted and " +
		"completeTerminalOutcome on OTHER branches — both compensators for rows with no outbox " +
		"record. Its other two callees no longer publish at all: work() and adopt() commit every " +
		"FACT they produce. It is listed because the syntactic guard sees a write and a publish in " +
		"one method; they never happen on the same path.",
}

// commitThenPublishPending is the DEFAULT-DENY allow-list of Service methods
// that still write order state and publish a FACT outside the transaction AND
// could ride the write.
//
// Every entry is a place where a crash or a broker refusal between the two
// leaves the store and the estate disagreeing, recovered — where it is recovered
// at all — by a `*_announced_at` marker and a compensator. #292 converted them
// one at a time, and each conversion removed its own entry: the guard tightened
// as the work landed rather than needing re-tightening by hand.
//
// # IT IS EMPTY, AND THE EMPTINESS IS THE STATEMENT
//
// Every transition in the OMS that writes order state now commits its FACT in
// the same transaction: admission's ACCEPTED, ROUTED, both fill folds, the amend
// outcome, the ErrUnpriced reject, adopt's VENUE_REJECTED reject, the
// cancellation with its outcome, and the trailing outcome of a submit that
// filled. #292's migration is done. What remains in commitThenPublishByDesign is
// not a queue of work: it is publishes with no state change to commit alongside
// them, which an outbox cannot help.
//
// DO NOT DELETE THIS MAP. An empty allow-list is not the same thing as no
// allow-list. The next genuine pair — a transition added with a FACT and no
// outbox record — must land here, named, with the issue that retires it, rather
// than in the BY DESIGN map, whose entries are a decision that a conversion
// would be wrong. Collapsing the two is what let every exemption claim #292
// would retire it, including the three that never will.
var commitThenPublishPending = map[string]string{}

// TestNoNewCommitThenPublishPairInTheOMS is guard (1). Default-deny: a Service
// method that both writes order state and publishes a FACT must be named above.
func TestNoNewCommitThenPublishPairInTheOMS(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, filepath.FromSlash(omsOrderPkg))

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", omsOrderPkg, err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			// A PARSE ERROR IS A HARD FAILURE, not a skip. A guard that silently
			// analyses zero files is a guard that passes forever.
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatalf("no non-test Go source under %s — the guard is looking in the wrong place", dir)
	}

	writes, publishes, calls := scanServiceMethods(files)

	// NON-VACUITY. If the shapes this guard matches ever stop appearing, it
	// starts passing for the wrong reason. These floors are the current counts
	// minus a margin, and tripping one means the analysis broke, not that the
	// code got better.
	if len(writes) < 5 {
		t.Fatalf("found store writes in only %d Service methods (%v) — the OMS has more than that, "+
			"so this guard has stopped recognising them", len(writes), sortedKeysOf(writes))
	}
	if len(publishes) < 5 {
		t.Fatalf("found publishes in only %d Service methods (%v) — this guard has stopped "+
			"recognising them", len(publishes), sortedKeysOf(publishes))
	}

	// THE WRITE SIDE STAYS DIRECT; THE PUBLISH SIDE IS CLOSED OVER CALLS.
	//
	// The asymmetry is deliberate and it is what keeps the report readable.
	// Closing BOTH sides makes every ancestor of a pair an offender — sweep
	// calls resume calls work, so sweep gets reported for a pair three levels
	// down and the operator has to work out which one is real. Closing only the
	// publish side reports the method that OWNS the write, which is the method
	// that has to change.
	//
	// The publish side must be closed, because delegating the publish to a
	// helper is the one-line defeat: handleCancel Saves and then calls
	// completeCancelAnnouncement, and a guard that only looked at direct calls
	// would see neither as a pair.
	//
	// THE BLIND SPOT THIS LEAVES, stated rather than discovered: extracting the
	// WRITE into a helper that does not itself publish drops the pair off this
	// report. That is a two-step refactor with no other motive, and the
	// direction of the error is toward missing a violation rather than
	// inventing one — but it is why the tests, not this, are the primary
	// evidence.
	publishes = closeOver(publishes, calls)

	var offenders []string
	for m := range writes {
		if publishes[m] {
			offenders = append(offenders, m)
		}
	}
	sort.Strings(offenders)

	for _, m := range offenders {
		if !commitThenPublishAllowed(m) {
			t.Errorf("(*Service).%s writes order state AND publishes a FACT outside the "+
				"transaction.\n\n"+
				"That is the #292 defect: a crash or a broker refusal between the two leaves the "+
				"store and the estate disagreeing, and the only recovery is another hand-rolled "+
				"`*_announced_at` marker plus another compensator plus another test — none of which "+
				"anything forces you to add.\n\n"+
				"Pass the FACT to the write instead. BOTH store.Create and store.Save take "+
				"[]outbox.Record, and every caller already states its answer — so converting this "+
				"transition is an edit to one call site, not a signature change.\n\n"+
				"If it genuinely cannot be converted yet, add %q to commitThenPublishPending with "+
				"the issue that retires it. If NO transaction exists for the FACT to ride in — a "+
				"compensator, or a publish on a branch that writes nothing — it belongs in "+
				"commitThenPublishByDesign instead, with the reason. Do not add it silently.", m, m)
		}
	}

	// DEAD ENTRIES. An exemption must not outlive its repair — otherwise the
	// list slowly becomes a description of code that no longer exists and stops
	// meaning anything.
	offending := map[string]bool{}
	for _, m := range offenders {
		offending[m] = true
	}
	for m := range allCommitThenPublishExemptions() {
		if !offending[m] {
			t.Fatalf("an exemption names (*Service).%s, which no longer writes state and "+
				"publishes outside the transaction. Either the method was converted — delete the "+
				"entry, and if it was the last one, delete the map and make this guard absolute — "+
				"or it was renamed and the exemption is now silently covering nothing.", m)
		}
	}

	// Said out loud on every run: passing means the remaining exposure is
	// RECORDED, not gone.
	for _, m := range sortedKeys(commitThenPublishPending) {
		t.Logf("PENDING COMMIT-THEN-PUBLISH PAIR: (*Service).%s — %s", m, commitThenPublishPending[m])
	}
	for _, m := range sortedKeys(commitThenPublishByDesign) {
		t.Logf("BY DESIGN, does not retire: (*Service).%s — %s", m, commitThenPublishByDesign[m])
	}
}

// TestTheOMSOutboxHasADrain is guard (2), and it is the one that matters most on
// the day somebody refactors the composition root.
//
// order.NewService builds the relay itself, from the store's own queue and the
// emitter's own bus, so a Service always HAS one — that part is enforced by the
// compiler. What is not enforced by anything is that cmd/oms/main.go RUNS it.
// Without the Run, admission commits FACTs into a table forever and the only
// symptom is a gauge nobody is looking at yet.
func TestTheOMSOutboxHasADrain(t *testing.T) {
	root := moduleRoot(t)

	// The producer side: something must enqueue, or the table is decoration.
	orderPkg := readGoFiles(t, filepath.Join(root, filepath.FromSlash(omsOrderPkg)))
	if !strings.Contains(orderPkg, "outbox.Enqueue(") {
		t.Error("nothing in " + omsOrderPkg + " calls outbox.Enqueue — the transactional outbox has " +
			"no writer, so every FACT is back to being a second independent write (#292)")
	}
	if !strings.Contains(orderPkg, "outbox.NewRelay(") {
		t.Error("order.NewService no longer constructs the relay. It does so deliberately — from the " +
			"store's own queue and the emitter's own bus — so that a composition holding a Service " +
			"cannot be missing its drain. Moving that back to the composition root reintroduces a " +
			"wiring step that can be omitted (#292)")
	}

	// The consumer side: the composition root must run it.
	mainGo := filepath.Join(root, "services", "oms", "cmd", "oms", "main.go")
	src, err := os.ReadFile(mainGo)
	if err != nil {
		t.Fatalf("read %s: %v", mainGo, err)
	}
	body := string(src)
	for _, want := range []struct{ frag, why string }{
		{"svc.Outbox()", "the composition root no longer takes the relay off the service"},
		{".Run(ctx)", "nothing runs the outbox relay's background drain, so a FACT whose inline " +
			"publish failed is never retried"},
		{".DrainOnce(ctx)", "the startup drain is gone — a pod inheriting records its predecessor " +
			"committed and never published would admit new orders before saying the old ones"},
	} {
		if !strings.Contains(body, want.frag) {
			t.Errorf("services/oms/cmd/oms/main.go no longer contains %q: %s (#292)", want.frag, want.why)
		}
	}

	// AND THE RELAY MUST BE JOINED. closeStores() is deferred in runConsumers, so
	// it runs after wg.Wait(); an unjoined drainer would still be writing to a
	// closing pool. test/arch/consumer_goroutine_join_test.go does not classify
	// this goroutine (it only looks for bus.NewConsumer inside one), so the join
	// is asserted here instead of being left to a comment.
	if !strings.Contains(body, "relay.Run(ctx)") {
		t.Error("the outbox relay is not run through the identifier this guard tracks; if it was " +
			"renamed, rename it here too rather than deleting the assertion")
	}
	if idx := strings.Index(body, "relay.Run(ctx)"); idx >= 0 {
		// The wg.Add(1)/defer wg.Done() pair must be within the same goroutine
		// literal. Checking the 400 bytes before the Run keeps this a locality
		// check rather than a whole-file grep that any wg.Add would satisfy.
		start := idx - 400
		if start < 0 {
			start = 0
		}
		window := body[start:idx]
		if !strings.Contains(window, "wg.Add(1)") || !strings.Contains(window, "defer wg.Done()") {
			t.Error("the outbox relay goroutine is not joined into the WaitGroup runConsumers waits " +
				"on. closeStores() is deferred above it, so it closes the pool the relay is still " +
				"reading and writing — and the relay's per-key advisory lock would be released by a " +
				"connection teardown rather than by its own unlock (#292)")
		}
	}
}

// TestTheFillFactHasNoWayOutExceptTheTransaction is guard (3).
//
// A fill is the only FACT this service emits that NOTHING can rebuild. The
// stored OrderState keeps the cumulative aggregate and not the individual Fill —
// no fill_id, no price, no venue_execution_id — which is why
// completeTerminalOutcome says in its own comment that re-emitting ORDER_FILLED
// would mean fabricating one. Every other lost FACT is recoverable from
// committed state by some compensator; this one was simply gone.
//
// Since #292's fill conversion the ONLY way to produce it is Emitter.FillFact,
// which returns an outbox.Record that has to be handed to a store write. There
// is deliberately no EmitFill any more, and this guard fails if one comes back.
//
// THE PAIR GUARD ABOVE IS NOT A SUBSTITUTE, for three reasons. It would report a
// reintroduced EmitFill in work() or adopt() as an unnamed pair — true, but it
// names the method rather than the loss, and the repair it suggests ("add it to
// commitThenPublishPending with the issue that retires it") is exactly the wrong
// one here, because there is no marker and no compensator that could retire it.
// It cannot see that FillFact still exists, so a fill that stopped being emitted
// entirely would leave it silent. And it cannot see a SECOND builder for the
// fill event, which is how the enqueued FACT and a republished one would come to
// differ in a field nobody compares.
func TestTheFillFactHasNoWayOutExceptTheTransaction(t *testing.T) {
	root := moduleRoot(t)
	src := readGoFiles(t, filepath.Join(root, filepath.FromSlash(omsOrderPkg)))

	// NON-VACUITY FIRST. If the fill FACT stopped being built at all, every
	// assertion below would pass by describing code that no longer exists.
	if !strings.Contains(src, "func (e *Emitter) FillFact(") {
		t.Fatal("Emitter.FillFact is gone from " + omsOrderPkg + " — this guard would pass vacuously. " +
			"If the fill FACT was renamed, rename it here; if it stopped being emitted at all, that " +
			"is a much larger problem than this guard (#292)")
	}
	if !strings.Contains(src, "s.emitter.FillFact(ctx, fill, next)") {
		t.Error("nothing in " + omsOrderPkg + " captures a fill FACT for a fold. work() and adopt() " +
			"are supposed to build the record and pass it to store.Save; if neither does, fills are " +
			"being persisted with no announcement committed alongside them (#292)")
	}
	if !strings.Contains(src, "s.store.Save(ctx, next, ver, []outbox.Record{fact})") {
		t.Error("no fold passes its fill FACT to store.Save. The record must ride the SAME " +
			"transaction as the state it announces — that is the entire property, and a record " +
			"built and then enqueued separately is two independent writes with extra steps (#292)")
	}

	// DEFAULT-DENY: no direct publish of a fill, by any route. The budget is how
	// many times the fragment may legitimately appear — one each for the two
	// constructions inside Emitter.fillEvent, which is the single builder the
	// enqueue and any future republish must share, and ZERO for the direct
	// emitter that no longer exists.
	for _, banned := range []struct {
		frag, why string
		budget    int
	}{
		{"EmitFill(", "a direct fill publish. store.Save takes []outbox.Record — build the FACT with " +
			"Emitter.FillFact and pass it to the write, so it commits with the state that records it", 0},
		{`EventTypeFilled, "OrderFilled"`, "a fill FACT built outside Emitter.fillEvent", 1},
		{`EventTypePartiallyFilled, "OrderPartiallyFilled"`, "a partial-fill FACT built outside " +
			"Emitter.fillEvent", 1},
	} {
		if n := strings.Count(src, banned.frag); n > banned.budget {
			t.Errorf("%q appears %d times in %s (at most %d expected): that is %s.\n\n"+
				"A fill is money that moved and it is the ONE FACT no compensator can rebuild — "+
				"completeTerminalOutcome says so in its own comment. A publish outside the "+
				"transaction that recorded the fill is therefore not a late FACT, it is a lost one, "+
				"and nothing downstream will ever learn the trade happened (#292).",
				banned.frag, n, omsOrderPkg, banned.budget, banned.why)
		}
	}
}

// scanServiceMethods returns, per (*Service) method name: whether it writes
// order state, whether it publishes a FACT, and which other methods on the same
// receiver it calls.
func scanServiceMethods(files []*ast.File) (writes, publishes map[string]bool, calls map[string][]string) {
	writes = map[string]bool{}
	publishes = map[string]bool{}
	calls = map[string][]string{}

	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil || !isServiceReceiver(fn.Recv) {
				continue
			}
			name := fn.Name.Name
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				// s.store.Create / s.store.Save
				if inner, ok := sel.X.(*ast.SelectorExpr); ok {
					if ident, ok := inner.X.(*ast.Ident); ok && ident.Name == "s" {
						switch inner.Sel.Name {
						case "store":
							if storeWriteMethods[sel.Sel.Name] {
								writes[name] = true
							}
						case "emitter":
							if strings.HasPrefix(sel.Sel.Name, "Emit") {
								publishes[name] = true
							}
						}
					}
					return true
				}
				// s.somethingElse(...) — an intra-package call on the receiver.
				if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "s" {
					calls[name] = append(calls[name], sel.Sel.Name)
				}
				return true
			})
		}
	}
	return writes, publishes, calls
}

// isServiceReceiver reports whether a method is on *Service.
func isServiceReceiver(recv *ast.FieldList) bool {
	if len(recv.List) != 1 {
		return false
	}
	star, ok := recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	ident, ok := star.X.(*ast.Ident)
	return ok && ident.Name == "Service"
}

// closeOver propagates a property backwards through the call graph to a fixed
// point: a method that calls something with the property has it too.
//
// This is what makes the guard resistant to the one-line defeat — extracting the
// publish into a helper. handleCancel Saves and then calls
// completeCancelAnnouncement, which publishes; without this, neither method
// would look like a pair and the guard would report nothing.
func closeOver(have map[string]bool, calls map[string][]string) map[string]bool {
	out := map[string]bool{}
	for k := range have {
		out[k] = true
	}
	for changed := true; changed; {
		changed = false
		for caller, callees := range calls {
			if out[caller] {
				continue
			}
			for _, callee := range callees {
				if out[callee] {
					out[caller] = true
					changed = true
					break
				}
			}
		}
	}
	return out
}

func sortedKeysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// readGoFiles concatenates every non-test .go file in a directory.
func readGoFiles(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	var b strings.Builder
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		b.Write(src)
	}
	if b.Len() == 0 {
		t.Fatalf("no non-test Go source found under %s — this guard would pass vacuously", dir)
	}
	return b.String()
}

// commitThenPublishAllowed reports whether a method is exempted, by either map.
//
// TWO MAPS RATHER THAN ONE because they mean different things: one is a queue of
// work, the other is a decision. Collapsing them is what let every entry claim
// #292 would retire it — including the three that never will, which is why that
// issue could not close.
func commitThenPublishAllowed(method string) bool {
	if _, ok := commitThenPublishPending[method]; ok {
		return true
	}
	_, ok := commitThenPublishByDesign[method]
	return ok
}

// allCommitThenPublishExemptions is both maps, for the dead-entry check. An
// exemption must not outlive its repair whichever kind it is: a BY DESIGN entry
// for a method that no longer writes-and-publishes describes code that is gone.
func allCommitThenPublishExemptions() map[string]string {
	out := make(map[string]string, len(commitThenPublishPending)+len(commitThenPublishByDesign))
	for k, v := range commitThenPublishPending {
		out[k] = v
	}
	for k, v := range commitThenPublishByDesign {
		out[k] = v
	}
	return out
}
