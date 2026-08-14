package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Every bus consumer must wire a DLQ, and only a certified consumer may wire
// in-handler retry.
//
// A Consumer built without WithDLQ has dlq == nil, so consumer.go's terminal
// path releases the dedup claim and RETURNS THE ERROR — it asks the broker to
// redeliver. Nothing parks the event and nothing records that it failed. If the
// redelivery is then acked by a handler that mistakes it for a duplicate, the
// event is GONE: no DLQ entry, no metric that outlives the process, no log line
// tying the two deliveries together.
//
// That is exactly what the OMS did on the capital path. A SubmitOrder whose
// venue call failed after admission returned an error, the broker redelivered
// it, and handleSubmit's fast path found the order already in the store and
// acked it. The order sat at ROUTED forever — indistinguishable from a limit
// order resting normally — while nothing was working it and nothing said so.
//
// The DLQ subsystem to prevent this was already built, documented and tested
// (publishDLQ, routeToDLQ, the Kanz-DLQ-* headers, dlq.<subject>). It was wired
// by ZERO of the estate's consumers. This guard is what makes it true rather
// than available.

// busConsumerCall is one bus.NewConsumer(...) call site and the option
// constructors passed to it.
type busConsumerCall struct {
	file    string
	line    int
	options map[string]bool
}

func (c busConsumerCall) where() string { return fmt.Sprintf("%s:%d", c.file, c.line) }

// busConsumerCalls finds every non-test bus.NewConsumer call site in the module.
//
// Parsed with go/ast rather than matched with a regex: the calls span several
// lines, carry comments between arguments, and a textual scan would have to
// re-implement paren balancing and string-literal skipping to find where each
// call ends. The option list is the thing under test, so reading it wrongly
// would make this guard lie in whichever direction the parser drifted.
func busConsumerCalls(t *testing.T, root string) []busConsumerCall {
	t.Helper()
	var out []busConsumerCall
	fset := token.NewFileSet()

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case "vendor", ".git", "gen", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isSelector(call.Fun, "bus", "NewConsumer") {
				return true
			}
			opts := map[string]bool{}
			for _, arg := range call.Args {
				optCall, ok := arg.(*ast.CallExpr)
				if !ok {
					continue
				}
				if sel, ok := optCall.Fun.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "bus" {
						opts[sel.Sel.Name] = true
					}
				}
			}
			out = append(out, busConsumerCall{
				file:    filepath.ToSlash(rel),
				line:    fset.Position(call.Pos()).Line,
				options: opts,
			})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", root, err)
	}
	return out
}

func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

// dlqExemptBroadcastOnlyConsumers is the default-deny allow-list of
// bus.NewConsumer call sites certified safe to omit bus.WithDLQ because every
// subscription they make is SubscribeBroadcast/SubscribeBroadcastReady, never
// the grouped Subscribe.
//
// pkg/bus/consumer.go:262-265 (SubscribeBroadcastReady's shared dispatch path)
// documents that the broadcast path does NOT route to the DLQ at all — an
// unreadable or failed broadcast message "is returned as an error, the message
// is nacked", full stop. c.dlq is never consulted anywhere in that path
// (consumer.go:266-299). That makes bus.WithDLQ structurally INERT on a
// consumer used only for broadcast: there is no code path left that would
// ever read c.dlq, so its absence loses nothing.
//
// A listed entry must also be a consumer that CANNOT hold a DLQ publisher: a
// read-only observer whose tenancy grant denies all business publish (see
// test/arch/nats_identity_test.go's readOnlyObserverSVIDs) has no dlq.*
// publish grant to construct bus.WithDLQ from in the first place. Certifying
// the exemption on inertness alone, without also requiring read-only, would
// let a capital-path consumer skip the DLQ merely by *also* subscribing to a
// broadcast subject elsewhere — this list is for consumers with NOTHING but
// broadcast subscriptions, and specifically ones that could never wire a DLQ
// even if asked to.
//
// This is default-deny like retryCertifiedConsumers below: a NEW
// bus.NewConsumer call site that lacks WithDLQ still fails
// TestEveryBusConsumerWiresADLQ until an entry here names it explicitly, with
// its own justification. Listing cannot silently widen — only enumerating a
// specific file:line can, and that requires a reviewed edit to this file.
var dlqExemptBroadcastOnlyConsumers = map[string]string{
	"cmd/kanz-monitor/busreader.go:94": "kanz-monitor: this Consumer is used " +
		"SOLELY for two SubscribeBroadcast calls (order.> at busreader.go:112 and " +
		"risk.position.changed.> at busreader.go:155) — there is no Subscribe call anywhere " +
		"in this file, so the DLQ-routing branch of consumer.go's Subscribe path " +
		"(consumer.go:212-221) is unreachable from this call site; per " +
		"consumer.go:262-265 the broadcast path never consults c.dlq regardless, so " +
		"WithDLQ would be inert here. The monitor is also a read-only observer " +
		"(test/arch/nats_identity_test.go's readOnlyObserverSVIDs; its tenancy grant " +
		"denies all business publish) and physically cannot hold the dlq.* " +
		"publisher WithDLQ requires — there is no producer anywhere in this file, " +
		"see busreader.go's own READ-ONLY doc comment.",
}

func TestEveryBusConsumerWiresADLQ(t *testing.T) {
	calls := busConsumerCalls(t, moduleRoot(t))

	// NON-VACUITY: this estate definitely constructs consumers. A scan that finds
	// none would pass this guard no matter how many were wired without a DLQ,
	// which is the failure mode the guard exists to prevent.
	if len(calls) == 0 {
		t.Fatal("found zero bus.NewConsumer call sites — the scanner is broken, not the services")
	}

	// DEAD-ENTRY CHECK: an allow-list entry naming a call site that no longer
	// exists (the line moved, the file was refactored, the consumer was
	// deleted) is a certification that silently protects nothing — the same
	// failure mode retryCertifiedConsumers guards against below.
	live := map[string]bool{}
	for _, c := range calls {
		live[c.where()] = true
	}
	var dead []string
	for site := range dlqExemptBroadcastOnlyConsumers {
		if !live[site] {
			dead = append(dead, site)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Fatalf("dlqExemptBroadcastOnlyConsumers names %d call site(s) that no longer exist as "+
			"bus.NewConsumer calls: %s\n\n"+
			"Either the file moved (update the file:line key) or the consumer itself was "+
			"removed (delete the entry) — a stale exemption protects nothing and must not "+
			"sit in the allow-list looking load-bearing.",
			len(dead), strings.Join(dead, ", "))
	}

	var unwired []string
	for _, c := range calls {
		if c.options["WithDLQ"] {
			continue
		}
		if _, ok := dlqExemptBroadcastOnlyConsumers[c.where()]; ok {
			continue
		}
		unwired = append(unwired, c.where())
	}
	if len(unwired) > 0 {
		sort.Strings(unwired)
		t.Fatalf("%d of %d bus.NewConsumer call sites are wired WITHOUT bus.WithDLQ:\n  %s\n\n"+
			"Without a DLQ the consumer has nowhere to park a terminal failure, so it releases the "+
			"dedup claim and returns the error — asking the broker for a redelivery that any "+
			"already-handled check will silently ack. The event is then gone with no record. "+
			"Pass bus.WithDLQ(<publisher>) at the composition root, or — only if every "+
			"subscription this call site makes is SubscribeBroadcast/SubscribeBroadcastReady and "+
			"the consumer cannot hold a DLQ publisher at all — add a certified entry to "+
			"dlqExemptBroadcastOnlyConsumers with the structural proof.",
			len(unwired), len(calls), strings.Join(unwired, "\n  "))
	}
}

// THE DLQ MUST HAVE A READER (#220).
//
// TestEveryBusConsumerWiresADLQ above makes every consumer PARK its terminal
// failures. On its own that is only half a guarantee, and the missing half cost
// exactly what the guard was built to prevent: for the whole life of the DLQ
// subsystem, NOTHING IN THE ESTATE SUBSCRIBED TO `dlq.>`. Parking worked
// perfectly and the messages were unreachable.
//
// It was invisible because every signal read healthy. The park succeeds, the
// original delivery is acked, kanz_bus_consume_total records the failed
// dispatch, and the DLQ stream accrues messages on 720h retention that no
// consumer, tool or alert ever looks at. tools/replay REFUSES a `dlq.` subject
// by design (it republishes into the isolated replay namespace, which is the
// wrong destination for a redrive), so the one piece of tooling an operator
// would reach for declines the job. A 200ms Postgres blip therefore parked a
// SubmitOrder whose client already held a 202, and recovery meant a human
// hand-writing a republisher.
//
// "A drain exists" is the kind of claim that is true on the day it is written
// and quietly false a year later, when the tool is renamed, moved under a build
// tag, or deleted as unused because nothing imports it. So it is a guard: the
// module must contain a non-test subscription to a subject in the `dlq.`
// namespace. Deleting cmd/kanz-redrive fails this test.
//
// It checks TWO things, because either alone is satisfiable by something an
// operator cannot actually use:
//
//  1. An ENGINE — a Subscribe-shaped call that reads the dlq. namespace.
//  2. An ENTRY POINT — a `package main` that reaches it. A drain library with no
//     binary is the exact shape that gets deleted as unused, and it is no more
//     reachable during an incident than no drain at all.
//
// Neither half names a file. What matters is that the namespace is drainable
// from this module, not which binary does it — a future service that drains the
// DLQ automatically would satisfy this without editing an allow-list.
func TestTheDLQNamespaceHasAReader(t *testing.T) {
	root := moduleRoot(t)
	engines, entryPoints := dlqDrainSites(t, root)

	const why = "\n\nEvery consumer is required to wire bus.WithDLQ (TestEveryBusConsumerWiresADLQ), so " +
		"terminal failures on the capital path — a SubmitOrder that hit a Postgres failover, an " +
		"execution FACT that hit a slow venue gate — are parked on dlq.<subject> and ACKED. A DLQ " +
		"with no drain is not a dead-letter queue, it is a deletion with a 720h delay: the client " +
		"holds its 202, no ORDER_REJECTED FACT is emitted, kanz-replay refuses dlq. subjects by " +
		"design (it republishes into the isolated replay namespace, which is the wrong " +
		"destination), and recovery needs a human to hand-write a republisher.\n\n" +
		"pkg/bus.Redriver is the engine and cmd/kanz-redrive is the entry point (#220). If one " +
		"was removed, restore it or replace it with something that reads the namespace."

	if len(engines) == 0 {
		t.Fatal("NOTHING in this module subscribes to a dlq.* subject." + why)
	}
	if len(entryPoints) == 0 {
		t.Fatalf("the DLQ drain engine exists (%s) but NO `package main` reaches it, so there is "+
			"no command an operator can run.%s", strings.Join(engines, ", "), why)
	}
}

// dlqDrainSites returns the files that implement the drain engine and the
// `package main` files that reach it.
//
// An ENGINE is a Subscribe-shaped call whose subject is either a string literal
// starting "dlq.", or operator-supplied and guarded by IsDLQSubject. The second
// form is how the real drain is written — its subject comes from a flag, so it
// cannot carry a literal, and it proves the same property at runtime by
// REFUSING anything outside the namespace.
//
// Scope is honest and deliberately narrow. A drain that assembled its subject
// some third way, or reached JetStream directly instead of through a
// Subscribe-named method, would slip past this; extend the scanner rather than
// routing around it.
func dlqDrainSites(t *testing.T, root string) (engines, entryPoints []string) {
	t.Helper()
	fset := token.NewFileSet()

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case "vendor", ".git", "gen", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		var literalDLQ, guards, subscribes bool
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			// The namespace guard, qualified (bus.IsDLQSubject, from another
			// package) or bare (IsDLQSubject, from inside pkg/bus itself). Missing
			// the bare form would make this guard blind to a drain that lives in the
			// bus package — which is where the engine actually is.
			if isSelector(call.Fun, "bus", "IsDLQSubject") {
				guards = true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "IsDLQSubject" {
				guards = true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !strings.HasPrefix(sel.Sel.Name, "Subscribe") {
				return true
			}
			subscribes = true
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.BasicLit)
				if ok && lit.Kind == token.STRING {
					if v, uerr := strconv.Unquote(lit.Value); uerr == nil && strings.HasPrefix(v, dlqNamespace) {
						literalDLQ = true
					}
				}
			}
			return true
		})
		if literalDLQ || (guards && subscribes) {
			engines = append(engines, filepath.ToSlash(rel))
		}
		// An entry point is a `package main` that reaches the drain: it either
		// applies the namespace guard itself (the flag-validating CLI) or
		// constructs the engine type.
		if f.Name != nil && f.Name.Name == "main" && (guards || usesRedriver(f)) {
			entryPoints = append(entryPoints, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", root, err)
	}
	return engines, entryPoints
}

// dlqNamespace is the reserved DLQ subject prefix (bus.dlqSubjectPrefix, and
// kanz-schemas/docs/subject-taxonomy.md §6). Spelled here rather than imported
// because test/arch is deliberately dependency-free over the module it scans.
const dlqNamespace = "dlq."

// usesRedriver reports whether f mentions bus.Redriver — a main package wiring
// the drain engine up to a real transport.
func usesRedriver(f *ast.File) bool {
	var found bool
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Redriver" {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "bus" {
				found = true
			}
		}
		return true
	})
	return found
}

// retryCertifiedConsumers is the default-deny allow-list of bus.NewConsumer call
// sites certified safe to wire bus.WithRetry. A call site NOT listed here fails
// the build the moment it wires WithRetry, regardless of how safe it looks —
// certification happens HERE, reviewed in, not at the call site.
//
// WithRetry re-runs the SAME handler in-process, on the SAME delivery, after a
// failed attempt. Certification requires that EVERY handler the call site
// dispatches — a single bus.NewConsumer commonly serves several subjects — is
// RE-ENTERABLE: a second in-process attempt, resuming from whatever partial
// work the first attempt left behind, either
//
//	(a) redoes the work (it is naturally idempotent, or dedups atomically in
//	    the same transaction as the persist), or
//	(b) detects completion and finishes whatever the first attempt left
//	    unfinished (resumes),
//
// and never takes a branch that returns nil (acks) because it mistook
// in-flight or already-completed work for something to skip. That last shape
// is what made this ban estate-wide in the first place: the OMS's handleSubmit
// found the order it had itself just created, on redelivery, and acked without
// checking whether the venue call that followed had actually finished — the
// failure never reached the DLQ, and the order sat at ROUTED forever. See
// services/oms/internal/order/reconcile.go for the fix (venue_ack_at) and
// .superpowers/sdd/withretry-narrowing-report.md for the per-call-site
// evidence behind every entry below (and every refusal).
//
// Only the consumer.Subscribe path is at stake: SubscribeBroadcast and
// SubscribeBroadcastReady never retry (consumer.go builds no retry loop around
// either), so a call site whose every subscription runs through one of those
// two has nothing here to certify — WithRetry there would be inert, not
// dangerous, and is deliberately left OFF this list rather than certified for
// a decision that cannot arise (services/compliance, services/webhook-ingest).
//
// Keyed on "file:line" — busConsumerCall.where(), the same identity the guard
// already reports failures against. An edit that moves the call site
// un-certifies it until this entry is updated to match; that fails CLOSED,
// which is the safe direction for a capital-path guard.
var retryCertifiedConsumers = map[string]string{
	"services/tv-sync/cmd/tv-sync/main.go:123": "tv-sync: the sole handler is " +
		"projection.Projection.Handle. Its only error return before the fold is " +
		"PostgresLog.Append itself failing (services/tv-sync/internal/projection/postgres.go:68), " +
		"which means nothing committed. fold() never returns an error and Handle " +
		"unconditionally returns nil once it runs, so there is no error path between a " +
		"successful Append and the fold that a retry could trigger — a retry can only " +
		"ever re-attempt an Append that did not durably land, never skip a fold whose " +
		"Append already committed.",

	"services/audit/cmd/audit/main.go:195": "audit: the sole handler is audit.Projector.Handle, " +
		"which appends through Postgres.Append (services/audit/internal/audit/postgres.go:32). " +
		"The event_id dedup check and the insert run inside ONE transaction under an " +
		"advisory xact lock — atomic claim-and-persist, not check-then-act — so a retry " +
		"after a failed transaction is a clean redo and a retry after a committed one is " +
		"a no-op read of the same row, never a skip of unfinished work.",

	"services/accounting/cmd/accounting/main.go:472": "accounting (fills+cash): dispatches " +
		"consume.Folder.Handle and Folder.HandleCash, both of which resolve to " +
		"ledger.Postgres.Append (services/accounting/internal/ledger/postgres.go:31) — a single " +
		"INSERT ... ON CONFLICT (tenant_id, entry_id) DO NOTHING inside one transaction. " +
		"No read-then-decide gap exists for a retry to land in; it either redoes an " +
		"uncommitted write or no-ops an already-committed one.",

	"services/accounting/cmd/accounting/main.go:594": "accounting (live FX): the sole handler is " +
		"fxfeed.LiveFX.Handler (services/accounting/internal/fxfeed/fxfeed.go:65) — an " +
		"unconditional last-value cache write with no dedup branch at all. Re-running it " +
		"with the same quote sets the same rate; there is nothing to skip.",

	"services/risk-engine/cmd/risk-engine/main.go:295": "risk-engine (state ingest): dispatches " +
		"ingest.Ingestor.Handler -> engine.TriggeringApplier -> state.Store.ApplyPortfolioRevalued" +
		"/ApplyPositionChanged/ApplyPortfolioSnapshot (internal/risk/state/store.go:202,227,246). " +
		"Each Apply* checks its per-portfolio dedup window and mutates in-memory state with " +
		"no I/O and no possible error in between — dw.Record always follows the mutation " +
		"immediately, so an Apply* either runs to completion or (on a deterministic " +
		"pre-mutation error, e.g. a missing aggregate id) never starts. TriggeringApplier's " +
		"Trigger (the async debounced recompute) fires only after Apply* returns nil and " +
		"cannot itself fail the handler, so a retry can never observe a Trigger that ran " +
		"without its Apply* having actually completed.",

	"services/risk-engine/cmd/risk-engine/main.go:391": "risk-engine (calibration quotes): the " +
		"sole handler is livequote.LiveQuotes.Handler (internal/risk/pricing/livequote/livequote.go:80) " +
		"— an unconditional last-value cache write, same shape as accounting's live FX feed. " +
		"Nothing to skip.",

	"services/market-data/cmd/market-data/main.go:184": "market-data: the sole handler is " +
		"marketdata.Ingestor.Handler, which writes through Postgres.Put " +
		"(internal/marketdata/store/postgres.go:49) — INSERT ... ON CONFLICT (instrument_id, " +
		"observation_time, kind, knowledge_time) DO NOTHING inside one transaction. Same " +
		"atomic-claim shape as audit/accounting; no check-then-act gap.",

	"services/autopilot/cmd/autopilot/main.go:147": "autopilot: the sole handler is " +
		"controller.Controller.Handle, which has no dedup-and-skip branch at all — a retry " +
		"always re-runs Dispatch (match -> runbook -> escalate) from the top. Every " +
		"runbook.Action is a documented MUST-be-idempotent contract " +
		"(services/autopilot/internal/runbook/runbook.go:15) precisely because the controller " +
		"already re-runs runbooks on ordinary at-least-once redelivery; in-process retry adds " +
		"no new failure shape. A doubled escalation page is a duplicate alert, not lost work.",

	"services/lineage/cmd/lineage/main.go:369": "lineage: the sole handler is harvest.Harvester.Handle. " +
		"graph.Memory.Observe (services/lineage/internal/graph/graph.go:71) always runs to " +
		"completion (its own doc comment: 're-observing an event re-counts it but the edges " +
		"are a set') before the OpenLineage Emit call that can fail — so a retry re-observes " +
		"(accepted, pre-existing double-count on the Events tally, not a skip) and re-emits; " +
		"it never skips the observe that a first attempt already made.",

	"services/lake-sink/cmd/lake-sink/main.go:142": "lake-sink: the sole handler is cdc.EventSink.Handle, " +
		"which has no dedup-and-skip branch — every attempt decodes, writes and flushes from " +
		"scratch, and the doc comment is explicit that a duplicate row is expected and " +
		"resolved by downstream compaction (services/lake-sink/internal/cdc/sink.go:49). A retry " +
		"redoes the row; it cannot skip it.",

	"services/alternatives/cmd/alternatives/main.go:290": "alternatives: the sole handler is " +
		"consume.Folder.Handle, which appends through fund.Postgres.Append " +
		"(services/alternatives/internal/fund/postgres.go:30) — a single INSERT ... ON CONFLICT " +
		"(tenant_id, event_id) DO NOTHING. Same atomic-claim shape as the ledger and audit " +
		"stores.",

	"services/wealth/cmd/wealth/main.go:306": "wealth: the sole handler is consume.Folder.Handle, " +
		"which Puts through book.Postgres.Put (services/wealth/internal/book/postgres.go:30) — an " +
		"unconditional last-write-wins UPSERT keyed on household_id. Re-running it with the " +
		"same composition is a no-op change; there is no dedup branch to skip through.",

	"services/oms/cmd/oms/main.go:595": "oms: dispatches handleSubmit, handleCancel, handleAmend " +
		"(order.Service.Handle) and position.Projector.Handle (fills), re-derived fresh against " +
		"250fe00 rather than assumed fixed — see .superpowers/sdd/oms-recert-report.md for the " +
		"full per-failure-point walk. handleSubmit: every failure point after store.Create either " +
		"redoes unexecuted work (resume's Unknown+no-ack -> ActionRedrive re-drives Route/Save/" +
		"EmitRouted/Execute, and SimVenue.Execute is idempotent by its own executed-fills record, " +
		"venue/internal/execution/venue.go:170-221) or completes an interrupted terminal " +
		"announcement via outcome_announced_at + completeTerminalOutcome (service.go:745,773-778,910-937 " +
		"— the 250fe00 fix, re-verified to actually fire on a fill-loop EmitFill failure that leaves " +
		"the order FILLED-but-unannounced). A partial, non-terminal fill interrupted mid-loop is " +
		"unreachable with SimVenue (single full-leaves fill only) and, for FIXVenue/GRPCVenue, " +
		"resume's own Querier check quarantines rather than guessing (neither implements " +
		"execution.Querier) — an alerting freeze, never a silent ack. handleCancel: the " +
		"cancel_announced_at resume branch (service.go:505-506) completes the announcement without " +
		"re-dispatching closeAtVenue, confirmed by reading the call graph, not just the comment. " +
		"handleAmend rewrites absolute values under an IsTerminal guard that a live amend target " +
		"never trips, so a retry recomputes and re-persists the same result. position.Projector.Handle " +
		"folds through Postgres.Apply's single-transaction fill_id claim-and-fold (position/postgres.go:105-159) " +
		"— atomic dedup, not check-then-act. Two completeness gaps found and reported but judged " +
		"non-blocking because neither skips durably-committed work: resume's PENDING_NEW/ActionRedrive " +
		"branches and adopt()'s fill loop never (re-)emit the CommandOutcome/EmitAccepted a direct " +
		"admission would — HALF OF THAT IS NOW CLOSED (#238): resume's PENDING_NEW branch re-emits " +
		"EmitAccepted whenever accepted_announced_at is unset, so a redelivery or a kanz-redrive " +
		"replay of a SubmitOrder whose store.Create committed and whose ORDER_ACCEPTED publish did " +
		"not now repairs the announcement instead of silently working the order for an estate that " +
		"never heard of it. The CommandOutcome half of the gap, and adopt()'s fill loop, remain open. " +
		"ActionRedrive still does not special-case " +
		"ErrUnpriced the way handleSubmit's own admission path does (service.go:260-277 vs 839-841) — " +
		"both fail by never producing an announcement or by nacking loudly toward the DLQ, never by " +
		"acking work that was never done. RE-CERTIFIED for the per-order lock: handleCancel and " +
		"handleAmend now take awaitClaim before reading (service.go), which adds ONE new failure " +
		"point to each — the acquisition timing out. It is the most trivially re-enterable failure " +
		"point in the file: on that branch nothing is read, nothing is persisted, nothing is " +
		"emitted and no venue call is made, so a second in-process attempt simply re-attempts the " +
		"acquisition against unchanged state — category (a), a clean redo with no partial work to " +
		"resume. It notably does NOT introduce the banned shape: the timeout returns the error " +
		"(nack), it does not return nil. Taking the try-lock instead and acking when the lock is " +
		"held WOULD be exactly the forbidden branch — mistaking in-flight work for something to " +
		"skip, and silently discarding an operator's cancel — which is why cancel and amend wait " +
		"on the lock rather than probing it. One caveat if WithRetry is ever actually wired here " +
		"(it is not today): an in-process retry of a timed-out cancel re-attempts against a lock " +
		"the SAME process still holds inside work(), so it will burn another full claimWait before " +
		"failing again. That is a latency and head-of-line cost, not a correctness break — the " +
		"retry still cannot ack undone work.",
}

func TestNoBusConsumerWiresRetryWhileHandlersResumeByAcking(t *testing.T) {
	calls := busConsumerCalls(t, moduleRoot(t))

	if len(calls) == 0 {
		t.Fatal("found zero bus.NewConsumer call sites — the scanner is broken, not the services")
	}

	// DEAD-ENTRY CHECK: an allow-list entry naming a call site that no longer
	// exists (the line moved, the file was refactored) is a certification that
	// silently protects nothing — the same failure mode nats_identity_test.go's
	// operatorSVIDs guards against for SVID entries.
	live := map[string]bool{}
	for _, c := range calls {
		live[c.where()] = true
	}
	var dead []string
	for site := range retryCertifiedConsumers {
		if !live[site] {
			dead = append(dead, site)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Fatalf("retryCertifiedConsumers names %d call site(s) that no longer exist as "+
			"bus.NewConsumer calls: %s\n\n"+
			"Either the file moved (update the file:line key) or the consumer itself was "+
			"removed (delete the entry) — a stale certification protects nothing and must "+
			"not sit in the allow-list looking load-bearing.",
			len(dead), strings.Join(dead, ", "))
	}

	var uncertified []string
	for _, c := range calls {
		if !c.options["WithRetry"] {
			continue
		}
		if _, ok := retryCertifiedConsumers[c.where()]; ok {
			continue
		}
		uncertified = append(uncertified, c.where())
	}
	if len(uncertified) > 0 {
		sort.Strings(uncertified)
		t.Fatalf("%s wire bus.WithRetry without being in retryCertifiedConsumers.\n\n"+
			"WithRetry re-runs the SAME handler in-process, on the SAME delivery, after a "+
			"failed attempt. Certification requires that EVERY handler this call site "+
			"dispatches is RE-ENTERABLE: a second in-process attempt must either redo the "+
			"work (it is naturally idempotent, or dedups atomically in the same transaction "+
			"as the persist) or detect completion and finish what the first attempt left "+
			"unfinished — and it must never take a branch that returns nil because it "+
			"mistook in-flight or already-completed work for something to skip.\n\n"+
			"That last shape is exactly what made this ban estate-wide: the OMS's "+
			"handleSubmit found the order it had itself just created, on redelivery, and "+
			"acked without checking whether the venue call that followed had actually "+
			"finished — the failure never reached the DLQ, and the order sat at ROUTED "+
			"forever (fixed since; see services/oms/internal/order/reconcile.go).\n\n"+
			"To wire WithRetry here: read every handler this call site dispatches, add a "+
			"named entry to retryCertifiedConsumers in this file with the code-evidence "+
			"justification for each one, and record it in "+
			".superpowers/sdd/withretry-narrowing-report.md — the same bar every entry "+
			"already in that map had to clear.",
			strings.Join(uncertified, ", "))
	}
}
