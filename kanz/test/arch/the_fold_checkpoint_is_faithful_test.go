package arch

import (
	"strings"
	"testing"
)

// A CHECKPOINTED BOOT MUST REACH THE BOOK A FULL REPLAY REACHES (#809).
//
// EXEC-M21 built tv_facts because a pod roll left a trader looking at an empty
// account while real positions sat open at the exchanges — realized P&L since
// inception was gone. The repair was to replay every fact on boot, and #809's
// original proposal was to bound that replay to a recent window. It was NOT
// implemented, and this guard exists partly to keep it that way: a windowed
// rebuild loses every position opened before the window and reports P&L since the
// window start, which is EXEC-M21's defect with a shorter horizon.
//
// What landed instead is a checkpoint, and it is safe only while
//
//	restore(checkpoint) + fold(facts after it)  ==  fold(all facts)
//
// Four ways that stops being true, each of which produces a book the fund never
// had rather than an error:
//
//  1. THE CHECKPOINT STOPS CARRYING seenFills. It is not part of the view, it is
//     the dedup for the DUAL fill path — the synchronous venue response and the
//     asynchronous websocket echo of the same fill. A fill id missing when its
//     echo arrives is a fill folded twice, which doubles a position the fund does
//     not hold. It is the cheapest-looking thing to drop and the most expensive.
//  2. THE WATERMARK STOPS BOUNDING THE REPLAY, or bounds it wrongly. Replaying
//     from 0 onto a restored checkpoint re-folds facts the view already holds;
//     replaying from too high a seq skips facts it does not.
//  3. A CHECKPOINT FROM ANOTHER TENANT IS RESTORED. RLS makes it unreachable
//     through the pool, so reaching it means the row was written under a different
//     scope — and restoring another tenant's book is a cross-tenant disclosure on
//     the surface a trader reads.
//  4. NOTHING EVER WRITES ONE. The pod still rebuilds correctly and every
//     successor pays the full replay again: a degradation whose only symptom is a
//     startup that lengthens with the fund's history.

const (
	cpFileRel   = "../../services/tv-sync/internal/projection/checkpoint.go"
	cpProjRel   = "../../services/tv-sync/internal/projection/projection.go"
	cpStoreRel  = "../../services/tv-sync/internal/projection/postgres.go"
	cpMainRel   = "../../services/tv-sync/cmd/tv-sync/main.go"
	cpLoopRel   = "../../services/tv-sync/cmd/tv-sync/checkpoint.go"
	cpSchemaRel = "../../../kanz-schemas/proto/tvsync/v1/snapshot.proto"
)

// TestTheCheckpointCarriesTheFillDedupSet holds rule one.
func TestTheCheckpointCarriesTheFillDedupSet(t *testing.T) {
	proto := readStripped(t, cpSchemaRel)
	if !strings.Contains(proto, "seen_fills") {
		t.Fatal("tvsync.v1.AccountSnapshot no longer carries seen_fills. That set is the dedup for " +
			"the DUAL fill path — the synchronous venue response and the asynchronous websocket " +
			"echo of the SAME fill — so a restored pod without it folds the next echo a second " +
			"time and doubles a position the fund does not hold. The bus-redelivery path is " +
			"protected separately by tv_facts's (tenant_id, event_id) primary key; this is not " +
			"that (#809).")
	}
	src := readStripped(t, cpFileRel)
	for _, half := range []struct{ what, needle string }{
		{"write", "out.SeenFills = append(out.SeenFills"},
		{"restore", "a.seenFills[fid] = true"},
	} {
		if !strings.Contains(src, half.needle) {
			t.Fatalf("the checkpoint does not %s the fill dedup set (%q). The schema field alone is "+
				"not the control: a snapshot that declares seen_fills and never fills it restores a "+
				"pod that will double-count the next echoed fill (#809).", half.what, half.needle)
		}
	}
}

// TestTheReplayIsBoundedByTheCheckpointWatermark holds rule two.
func TestTheReplayIsBoundedByTheCheckpointWatermark(t *testing.T) {
	store := readStripped(t, cpStoreRel)
	if !strings.Contains(store, "WHERE seq > $1") {
		t.Fatal("PostgresLog.Replay no longer bounds itself by a seq. Every boot then re-folds the " +
			"fund's entire history onto a restored checkpoint — which is correct (the fold is " +
			"idempotent through the dedup) and defeats the entire point, silently (#809).")
	}

	body := funcBody(t, readStripped(t, cpProjRel), "func (p *Projection) Rehydrate(")
	load := strings.Index(body, "LoadCheckpoint(")
	replay := strings.Index(body, "p.log.Replay(")
	switch {
	case load < 0:
		t.Fatal("Rehydrate no longer loads a checkpoint — the boot is unbounded again (#809)")
	case replay < 0:
		t.Fatal("Rehydrate no longer replays the tail. A restored checkpoint alone is a book that " +
			"is stale by everything folded since it was taken, which on a busy account is the " +
			"trader's most recent fills.")
	case load > replay:
		t.Fatal("Rehydrate replays BEFORE it loads the checkpoint, so the restore overwrites the " +
			"tail it just folded. The resulting book is the checkpoint exactly — every fact after " +
			"it silently discarded (#809).")
	}
}

// TestARestoredCheckpointIsTenantChecked holds rule three.
func TestARestoredCheckpointIsTenantChecked(t *testing.T) {
	body := funcBody(t, readStripped(t, cpFileRel), "func (p *Projection) restore(")
	if !strings.Contains(body, "ErrCheckpointTenantMismatch") {
		t.Fatal("restore no longer refuses a checkpoint written for another tenant. RLS makes that " +
			"unreachable through the pool, which is exactly why reaching it means something " +
			"structural is wrong — and restoring another tenant's orders, fills and P&L into this " +
			"pod is a cross-tenant disclosure on the surface a trader reads, not merely a wrong " +
			"number. Tenant isolation here is deny-by-default and covers discovery (#809).")
	}
}

// TestSomethingActuallyWritesACheckpoint holds rule four.
//
// STRUCTURAL, because the failure is silent: a pod with no checkpoint loop
// rebuilds a correct book and simply takes longer every time, so nothing errors
// and no probe fails.
func TestSomethingActuallyWritesACheckpoint(t *testing.T) {
	main := readStripped(t, cpMainRel)
	if !strings.Contains(main, "runCheckpoints(") {
		t.Fatal("tv-sync's composition root never starts the checkpoint loop. The pod boots " +
			"correctly from whatever checkpoint it finds and writes none of its own, so every " +
			"successor pays the full replay — a degradation whose only symptom is a startup that " +
			"lengthens with the fund's history (#809).")
	}

	// The metrics must be registered outside any branch that depends on the
	// database, for the reason #973, #963 and #983 each shipped once: a rule over
	// an absent series evaluates to nothing.
	reg := strings.Index(main, "registerCheckpointMetrics(")
	rehydrate := strings.Index(main, "proj.Rehydrate(")
	switch {
	case reg < 0:
		t.Fatal("the checkpoint metrics are never registered, so boot cost is unmeasurable and " +
			"#809's own 'Verified when' — boot time flat against total history — cannot be " +
			"checked on a running estate.")
	case rehydrate < 0:
		t.Fatal("main no longer calls Rehydrate — this guard's premise has moved")
	case reg > rehydrate:
		t.Fatal("registerCheckpointMetrics is called AFTER the rebuild it measures, so a pod that " +
			"fails during rehydrate exports nothing about how long it was taking. Register before.")
	}

	loop := readStripped(t, cpLoopRel)
	if !strings.Contains(loop, "case <-ctx.Done():") || !strings.Contains(loop, "context.Background()") {
		t.Fatal("runCheckpoints no longer takes a final checkpoint on shutdown, on a context that " +
			"is not the cancelled one. A graceful stop is the cheapest checkpoint there is — the " +
			"fold has stopped, so the state is quiet — and skipping it throws away everything " +
			"folded since the last tick (#809).")
	}
}

// TestTheReplayWindowWasNotReintroduced is the negative guard.
//
// #809's original recommendation was to bound the replay to a recent window and to
// add retention to tv_facts. Both were deliberately NOT implemented, and both look
// like the obvious next optimisation to somebody reading the issue rather than this
// file: a window loses every position opened before it, and retention destroys the
// only record a rebuild is defined against.
func TestTheReplayWindowWasNotReintroduced(t *testing.T) {
	store := readStripped(t, cpStoreRel)
	for _, banned := range []string{"knowledge_at >", "knowledge_at >=", "DELETE FROM tv_facts"} {
		if strings.Contains(store, banned) {
			t.Fatalf("the fact log now filters or deletes by time (%q). #809 proposed exactly that "+
				"and it was rejected: a windowed rebuild loses every position opened before the "+
				"window and reports realized P&L since the window start, which is EXEC-M21's "+
				"defect — a trader looking at a book the fund does not have — with a shorter "+
				"horizon. The bound that IS safe is the checkpoint watermark, which shortens the "+
				"work without shortening the history (#809).", banned)
		}
	}
}
