package arch

// tv-sync's RESIDENT BOOK MUST STAY BOUNDED, AND BOUNDING IT MUST STAY LOSSLESS
// (#809).
//
// #988 bounded the BOOT with a fold checkpoint. #809's other half is the HEAP:
// the projection appended an execution per fill, an order revision per status
// transition and a fill id per fill, for the life of the process, so a pod
// holding a busy fund grew until it was OOM-killed — and the operator interface
// disappeared at the moment an incident made somebody want it.
//
// The repair is a retention window over the RAM view only. Five ways it stops
// being safe, each of which is silent:
//
//  1. THE COMPOSITION ROOT STOPS APPLYING IT. WithRetention is an Option, so a
//     Projection built without it keeps everything — the original leak, back,
//     with a green suite and no error anywhere.
//  2. NOTHING RUNS THE SWEEP. Rehydrate evicts as it replays, so a pod that
//     never sweeps boots inside the window and then grows out of it for the rest
//     of its life. The only symptom is the pod dying.
//  3. THE WINDOW BECOMES OPTIONAL IN CONFIG. A zero is the leak; a value under
//     the floor drops a fill id before its websocket echo arrives, which folds
//     the fill twice and doubles a position the fund does not hold.
//  4. THE AS-OF REFUSAL GOES. A fold of a partial history succeeds and returns a
//     position, an average cost and a realized P&L that describe a book the fund
//     never had. That is worse than the leak it came from.
//  5. THE DURABLE LOG IS SHORTENED INSTEAD. Covered next door by
//     TestTheReplayWindowWasNotReintroduced, which is the rule this one must not
//     be confused with: retention here is memory, and tv_facts keeps everything.

import (
	"strings"
	"testing"
)

const (
	retProjRel   = "../../services/tv-sync/internal/projection/retention.go"
	retModelRel  = "../../services/tv-sync/internal/projection/model.go"
	retFoldRel   = "../../services/tv-sync/internal/projection/projection.go"
	retMainRel   = "../../services/tv-sync/cmd/tv-sync/main.go"
	retLoopRel   = "../../services/tv-sync/cmd/tv-sync/retention.go"
	retCfgRel    = "../../services/tv-sync/internal/config/config.go"
	retCPFileRel = "../../services/tv-sync/internal/projection/checkpoint.go"
	retSchemaRel = "../../../kanz-schemas/proto/tvsync/v1/snapshot.proto"
)

// TestTheCompositionRootBoundsTheResidentHistory holds rules one and two.
//
// STRUCTURAL, because both failures are silent: a projection with no window
// serves a perfectly correct book and simply grows, and a pod with no sweep
// boots correctly and then grows. Nothing errors, no probe fails, and the first
// signal is an OOM kill.
func TestTheCompositionRootBoundsTheResidentHistory(t *testing.T) {
	main := readStripped(t, retMainRel)
	if !strings.Contains(main, "projection.WithRetention(") {
		t.Fatal("tv-sync's composition root builds its Projection without a retention window. " +
			"WithRetention is an Option and its absence means KEEP EVERYTHING, which is the leak " +
			"#809 filed: an execution per fill, an order revision per transition and a fill id per " +
			"fill, held for the life of the process, until the pod is OOM-killed with no other " +
			"symptom (#809).")
	}
	if !strings.Contains(main, "runRetention(") {
		t.Fatal("tv-sync's composition root never starts the retention sweep. Rehydrate evicts as it " +
			"replays, so the pod BOOTS inside the window and then grows out of it for the rest of " +
			"its life — a leak with a delay on it, which is harder to see than the original (#809).")
	}

	// The resident-set series must be registered outside every branch that
	// depends on the database, for the reason #973, #963, #983 and #1005 each
	// shipped once: a rule over an absent series evaluates to nothing, and #809's
	// own "Verified when" — resident set flat against total history — is only
	// checkable on a running estate through these.
	reg := strings.Index(main, "registerRetentionMetrics(")
	rehydrate := strings.Index(main, "proj.Rehydrate(")
	switch {
	case reg < 0:
		t.Fatal("the resident-history metrics are never registered, so #809's 'Verified when' — the " +
			"resident set flat against total history — cannot be checked on a running estate at all.")
	case rehydrate < 0:
		t.Fatal("main no longer calls Rehydrate — this guard's premise has moved")
	case reg > rehydrate:
		t.Fatal("registerRetentionMetrics is called AFTER the rebuild, so a pod that dies during " +
			"rehydrate exports nothing about how much it was holding. Register before.")
	}

	// And the sweep must actually evict, rather than only measure. A loop that
	// publishes gauges and calls nothing would satisfy the name and none of the
	// purpose.
	loop := readStripped(t, retLoopRel)
	if !strings.Contains(loop, "proj.Evict(") {
		t.Fatal("runRetention no longer calls Evict — it reports a resident set that nothing is " +
			"bounding, which reads as a working control right up until the OOM kill (#809).")
	}
}

// TestTheRetentionWindowIsRefusedRatherThanDefaulted holds rule three.
//
// The floor is a CORRECTNESS bound, not a comfort one: the resident window is
// also the fill-id dedup window for the dual fill path, so a short one folds an
// echoed fill twice.
func TestTheRetentionWindowIsRefusedRatherThanDefaulted(t *testing.T) {
	cfg := readStripped(t, retCfgRel)
	if !strings.Contains(cfg, "MinRetention") {
		t.Fatal("config no longer declares a floor under TV_SYNC_RETENTION. The resident window is " +
			"also the fill-id dedup window for the DUAL fill path — the synchronous venue response " +
			"and the asynchronous websocket echo of the same fill, two FACTs with two event_ids that " +
			"tv_facts's primary key does not deduplicate — so a short window folds the echo a second " +
			"time and doubles a position the fund does not hold (#809).")
	}
	body := funcBody(t, cfg, "func Load(")
	if !strings.Contains(body, "TV_SYNC_RETENTION") {
		t.Fatal("config.Load no longer reads TV_SYNC_RETENTION at all, so every deployment runs " +
			"whatever the Projection defaults to — and its default is KEEP EVERYTHING (#809).")
	}
	if !strings.Contains(body, "retention < MinRetention") {
		t.Fatal("config.Load no longer refuses a window below MinRetention. A non-positive value is " +
			"the unbounded heap #809 filed and a short one double-counts an echoed fill; both fail " +
			"silently, so both have to be refused at startup rather than defaulted into (#809).")
	}
}

// TestAnAsOfReadBehindTheResidentWindowIsRefused holds rule four.
//
// A fold of a partial history does not fail. It returns numbers, and they look
// exactly like correct ones — which is why the refusal is asserted structurally
// rather than left to the read tests alone.
func TestAnAsOfReadBehindTheResidentWindowIsRefused(t *testing.T) {
	src := readStripped(t, retProjRel)
	if !strings.Contains(src, "ErrBeforeRetention") {
		t.Fatal("the projection no longer refuses an as-of read behind its resident window. Folding " +
			"the surviving window instead produces a position, an average cost and a realized P&L " +
			"for a book the fund never had — indistinguishable from a correct answer, which is worse " +
			"than the leak this retention came from (#809).")
	}
	body := funcBody(t, src, "func (p *Projection) readableAcct(")
	switch {
	case !strings.Contains(body, "ErrBeforeRetention"):
		t.Fatal("readableAcct no longer returns ErrBeforeRetention. The sentinel still exists, so " +
			"every caller and every errors.Is in the estate keeps compiling while the refusal has " +
			"stopped happening (#809).")
	case !strings.Contains(body, "asOf.Before(a.retainedFrom)"):
		t.Fatal("readableAcct no longer compares the requested as-of against the account's horizon, " +
			"so the refusal it still names cannot fire (#809).")
	}
	// AND THE CONDITION CARRIES NO BOOL CONSTANT. A guarded refusal switched off
	// with `false &&` leaves every symbol above present and every assertion here
	// green (#771, #957).
	for _, dead := range []string{"false &&", "&& false", "true ||", "|| true"} {
		if strings.Contains(body, dead) {
			t.Fatalf("readableAcct's refusal is short-circuited by a bool constant (%q). Every symbol "+
				"this guard reads is still there and the refusal never fires — the shape #771 and "+
				"#957 each shipped once (#809).", dead)
		}
	}

	// The live read must NOT be gated by it: the horizon bounds historical
	// questions, never the current book, and a refusal reaching the live path
	// would black out the trader's screen instead of one query.
	fold := funcBody(t, readStripped(t, retFoldRel), "func (p *Projection) foldFor(")
	if !strings.Contains(fold, "asOf.IsZero()") {
		t.Fatal("foldFor no longer special-cases the live read. A zero as-of is the CURRENT book and " +
			"must be served from the maintained fold; routing it through the history path makes " +
			"every trader's screen pay the bitemporal cost (#995), and gating it on the retention " +
			"horizon would black it out entirely (#809).")
	}
}

// TestEvictedHistoryIsFoldedRatherThanDropped is the property that separates this
// from the windowed rebuild #988 refused.
//
// A window that simply forgets reports the fund's positions and realized P&L
// SINCE THE WINDOW OPENED — a year-old position gone, P&L restarted — which is
// EXEC-M21's defect with a shorter horizon, on the surface a trader acts from.
// Folding what leaves into a per-account baseline is the whole difference, and it
// has to survive a checkpoint or the next boot loses it anyway.
func TestEvictedHistoryIsFoldedRatherThanDropped(t *testing.T) {
	ev := funcBody(t, readStripped(t, retProjRel), "func (a *account) evict(")
	if !strings.Contains(ev, "applyExecution(a.baselinePos(") {
		t.Fatal("account.evict drops executions without folding them into the account baseline. The " +
			"fund's positions and realized P&L then start when the retention window does — a " +
			"year-old position simply gone — which is EXEC-M21 (a trader looking at an empty account " +
			"while the fund's positions sat open at the exchanges) with a shorter horizon (#809).")
	}
	if !strings.Contains(ev, "delete(a.seenFills, e.fillID)") {
		t.Fatal("the fill-id dedup set is no longer evicted IN LOCKSTEP with the execution it " +
			"belongs to. Dropping it on any other schedule means an id can go before its websocket " +
			"echo arrives — the fill is folded twice and the position doubles — or never go at all, " +
			"which is one string per fill retained forever, the leak this change closed (#809).")
	}
	if !strings.Contains(ev, "isTerminal(last.status)") {
		t.Fatal("account.evict no longer restricts order eviction to TERMINAL orders. An order " +
			"resting at a venue can still fill; dropping it for age removes it from the blotter an " +
			"operator acts from while the fund's exposure is live (#809).")
	}

	live := funcBody(t, readStripped(t, retModelRel), "func (a *account) rebuildLive(")
	if !strings.Contains(live, "foldPositionsFrom(a.baseline") {
		t.Fatal("rebuildLive no longer starts from the baseline, so a pod restored from a checkpoint " +
			"serves the book since the retention window rather than since inception — and every " +
			"other field beside it is right, which is what makes this invisible (#809).")
	}

	proto := readStripped(t, retSchemaRel)
	for _, field := range []string{"baseline", "retained_from"} {
		if !strings.Contains(proto, field) {
			t.Fatalf("tvsync.v1.AccountSnapshot no longer carries %q. account.execs is a retention "+
				"window, so the fold of what fell out of it is the only record of the fund's "+
				"positions and realized P&L from before that window; a checkpoint without it "+
				"restores a book that starts on the horizon (#809).", field)
		}
	}
	cp := readStripped(t, retCPFileRel)
	for _, half := range []struct{ what, needle string }{
		{"write", "out.Baseline[inst] = &tvsyncpb.Lot{"},
		{"restore", "a.baseline[inst] = lot"},
	} {
		if !strings.Contains(cp, half.needle) {
			t.Fatalf("the checkpoint does not %s the evicted fold (%q). The schema field alone is not "+
				"the control: a snapshot that declares baseline and never fills it restores a pod "+
				"reporting P&L since the retention window (#809).", half.what, half.needle)
		}
	}
	// AND EXACTLY, not through a scaled Decimal. An average cost is cost/quantity
	// and a realized P&L accumulates (price-avg)*closed, so both are routinely
	// non-terminating; rendering them at the platform scale moved the fund's
	// realized P&L by its last digit on every restore, and it was measured on the
	// first round-trip test rather than feared.
	if !strings.Contains(cp, "l.Realized.RatString()") {
		t.Fatal("the baseline is no longer checkpointed as an exact rational. A derived average cost " +
			"and realized P&L are routinely non-terminating, so a scaled Decimal truncates them and " +
			"the fund's realized P&L drifts by the last digit on every restart, with nothing to " +
			"compare it against. services/oms/internal/position/postgres.go persists its own durable " +
			"lots as RatString for the same reason (#809).")
	}
}
