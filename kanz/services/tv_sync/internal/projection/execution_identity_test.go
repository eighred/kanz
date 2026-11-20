package projection

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/fillfact"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	tvsyncpb "github.com/eighred/kanz/kanz-schemas-go/tvsync/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func venueAccountExecution(orderID, venueAccount string, quantity int64) *orderpb.OrderFilled {
	event := filledOrder(orderID, "fund", "BTC-USD", orderpb.Side_SIDE_BUY, quantity, 100)
	event.State.Venue, event.State.VenueAccountId = "BINANCE", venueAccount
	f := event.Fill
	f.FillId, f.VenueExecutionId = "BTCUSDT-7", "7"
	f.Venue, f.VenueAccountId = "BINANCE", venueAccount
	f.Fee = &commonpb.Money{Amount: d(1), CurrencyCode: "USDT"}
	f.ExecutedAt = timestamppb.New(time.Unix(1800000000, 123000000).UTC())
	return event
}

func TestVenueAccountExecutionsRestart(t *testing.T) { venueAccountExecutionsRestart(t, newMemLog()) }
func TestVenueAccountExecutionsPostgresRestart(t *testing.T) {
	pool := newPool(t, testTenant)
	freshSchema(t, pool)
	venueAccountExecutionsRestart(t, NewPostgresLog(pool))
}

func venueAccountExecutionsRestart(t *testing.T, log Log) {
	t.Helper()
	ctx := t.Context()
	live := New(time.Now, nil, WithLog(log, testTenant))
	first, second := venueAccountExecution("first", "book-A", 2), venueAccountExecution("second", "book-B", 3)
	// Earlier raw Binance fills lacked venue_account_id. Only this original
	// order's retained terms bind the old fill to book-A.
	old := proto.Clone(first).(*orderpb.OrderFilled)
	old.Fill.VenueAccountId = ""
	deliver(t, live, "legacy-first", evtFilled, old)
	if err := live.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	wire, _, _, err := log.LoadCheckpoint(ctx)
	var cp tvsyncpb.Checkpoint
	if err != nil || proto.Unmarshal(wire, &cp) != nil || cp.ExactEncodingVersion != 3 || cp.Accounts[0].Execs[0].GetSourceFill().GetVenueAccountId() != "book-A" {
		t.Fatal("checkpoint did not retain verified account or version", err)
	}
	restart := func() *Projection {
		p := New(time.Now, nil, WithLog(log, testTenant))
		if err := p.Rehydrate(ctx); err != nil {
			t.Fatal(err)
		}
		return p
	}
	live = restart()
	deliver(t, live, "echo-A", evtFilled, first)
	deliver(t, live, "second", evtFilled, second)
	if err := live.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	live = restart()
	deliver(t, live, "echo-B", evtFilled, second)
	deliver(t, live, "second", evtFilled, second)
	got, err := live.Executions(testTenant, "fund", "", time.Time{})
	if err != nil || len(got) != 2 || got[0].VenueFillID != "BTCUSDT-7" || got[1].VenueFillID != "BTCUSDT-7" || got[0].FillID == got[1].FillID || got[0].FillID != fillfact.ExecutionKey(first.Fill) || got[1].FillID != fillfact.ExecutionKey(second.Fill) || got[0].OrderID == got[1].OrderID {
		t.Fatalf("lookalike trade IDs did not yield two distinct orders: %+v %v", got, err)
	}
	pos, err := live.Positions(testTenant, "fund", time.Time{})
	if err != nil || len(pos) != 1 || pos[0].Qty != "5" || pos[0].AvgPrice != "100" {
		t.Fatalf("position after redelivery: %+v %v", pos, err)
	}
	conflict := proto.Clone(first).(*orderpb.OrderFilled)
	conflict.Fill.Fee.CurrencyCode = "BNB"
	before := live.seq
	if err := live.Handle(ctx, factEnv("conflict", evtFilled), mustMarshal(t, conflict)); !errors.Is(err, fillfact.ErrExecutionIdentityConflict) || live.seq != before {
		t.Fatalf("fee conflict advanced fold: %v", err)
	}
	if err := New(time.Now, nil, WithLog(log, testTenant)).Rehydrate(ctx); !errors.Is(err, fillfact.ErrExecutionIdentityConflict) {
		t.Fatal("restart accepted conflicting durable tail", err)
	}
}

func TestVenueAccountLegacyCheckpointRequiresSourceProof(t *testing.T) {
	ctx := t.Context()
	log := newMemLog()
	p := New(time.Now, nil, WithLog(log, testTenant))
	first := venueAccountExecution("order-A", "account-A", 2)
	old := proto.Clone(first).(*orderpb.OrderFilled)
	old.Fill.VenueAccountId = ""
	deliver(t, p, "original", evtFilled, old)
	if err := p.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	// The original v1 binary retained no execution ID, venue account or fee currency.
	var legacy tvsyncpb.Checkpoint
	if err := proto.Unmarshal(log.cp, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy.ExactEncodingVersion = 1
	legacy.Accounts[0].ExecutionHistoryVerified = false
	legacy.Accounts[0].Execs[0].SourceFill = nil
	legacy.Accounts[0].SeenFills = []string{first.Fill.FillId}
	log.cp = mustMarshal(t, &legacy)
	restored := New(time.Now, nil, WithLog(log, testTenant))
	if err := restored.Rehydrate(ctx); err != nil {
		t.Fatal(err)
	}
	before := restored.seq
	if err := restored.Handle(ctx, factEnv("accounted-echo", evtFilled), mustMarshal(t, first)); !errors.Is(err, ErrUnresolvedFillAlias) || restored.seq != before || before != 1 {
		t.Fatalf("identity-free snapshot unexpectedly advanced: seq=%d err=%v", restored.seq, err)
	}
	if err := restored.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if log.cpSeq != 1 || restored.Resident().Executions != 1 {
		t.Fatal("ambiguous alias crossed the folded watermark")
	}
	// Moving the resident v1 row into the baseline must not certify it, even
	// if its original raw row can still be found in the log.
	restored.retention = time.Hour
	restored.Evict(time.Now().Add(24 * time.Hour))
	if err := restored.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	unverified := New(time.Now, nil, WithLog(log, testTenant))
	if err := unverified.Rehydrate(ctx); !errors.Is(err, ErrUnresolvedFillAlias) {
		t.Fatal("eviction promoted a v1 alias into an established history", err)
	}
	// Independently replay both original raw facts into an empty projection.
	// Here the timestamp, fee, venue execution ID and bound order account survive.
	replay := New(time.Now, nil)
	if err := log.Replay(ctx, 0, func(f Fact) error { _, err := replay.fold(testTenant, f); return err }); err != nil {
		t.Fatal(err)
	}
	if replay.Resident().Executions != 1 {
		t.Fatal("provable legacy trade was counted twice during full replay")
	}
}

func TestVenueAccountAmbiguousLegacyRawFacts(t *testing.T) {
	for _, legacyFirst := range []bool{true, false} {
		log := newMemLog()
		p := New(time.Now, nil, WithLog(log, testTenant))
		known := venueAccountExecution("order-A", "account-A", 2)
		unknown := proto.Clone(known).(*orderpb.OrderFilled)
		unknown.State.VenueAccountId = ""
		unknown.Fill.VenueAccountId = ""
		first, second := known, unknown
		if legacyFirst {
			first, second = unknown, known
		}
		deliver(t, p, "first", evtFilled, first)
		if err := p.Handle(t.Context(), factEnv("second", evtFilled), mustMarshal(t, second)); !errors.Is(err, ErrUnresolvedFillAlias) || p.seq != 1 || p.Resident().Executions != 1 {
			t.Fatal("unbound account overlap advanced fold", err)
		}
	}
}

func TestVenueAccountEvictedPostgresEvidence(t *testing.T) {
	for _, unindexed := range []bool{false, true} {
		name := "indexed"
		if unindexed {
			name = "historical_raw"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			pool := newPool(t, testTenant)
			freshSchema(t, pool)
			log := NewPostgresLog(pool)
			clock := &fakeClock{t: time.Unix(1800000000, 0).UTC()}
			newView := func() *Projection { return New(clock.now, nil, WithLog(log, testTenant), WithRetention(time.Hour)) }
			live := newView()
			first := venueAccountExecution("first", "book-A", 2)
			if unindexed {
				raw := proto.Clone(first).(*orderpb.OrderFilled)
				raw.Fill.VenueAccountId = ""
				if _, err := pool.Exec(ctx, "INSERT INTO tv_facts(event_id,event_type,payload,knowledge_at) VALUES('old-first',$1,$2,$3)", evtFilled, mustMarshal(t, raw), clock.t); err != nil {
					t.Fatal(err)
				}
				if err := live.Rehydrate(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				deliver(t, live, "first", evtFilled, first)
				if _, err := pool.Exec(ctx, "UPDATE tv_facts SET execution_key_digest=NULL WHERE event_id='first'"); err == nil {
					t.Fatal("indexed execution could be detached from its raw FACT")
				}
			}
			clock.t = clock.t.Add(2 * time.Hour)
			live.Evict(clock.t)
			if live.Resident().Fills != 0 || live.Resident().Executions != 0 {
				t.Fatal("evicted identities remained resident")
			}
			if err := live.Checkpoint(ctx); err != nil {
				t.Fatal(err)
			}
			live = newView()
			if err := live.Rehydrate(ctx); err != nil {
				t.Fatal(err)
			}
			deliver(t, live, "late-echo", evtFilled, first)
			if live.Resident().Executions != 0 {
				t.Fatal("evicted execution folded twice")
			}
			// Even an earlier economic time is legitimate on another venue account.
			second := venueAccountExecution("second", "book-B", 3)
			second.Fill.ExecutedAt = timestamppb.New(first.Fill.ExecutedAt.AsTime().Add(-time.Hour))
			deliver(t, live, "legitimate-late-fill", evtFilled, second)
			pos, err := live.Positions(testTenant, "fund", time.Time{})
			if err != nil || len(pos) != 1 || pos[0].Qty != "5" || live.Resident().Executions != 1 {
				t.Fatalf("late fill balances %+v: %v", pos, err)
			}
			change := proto.Clone(first).(*orderpb.OrderFilled)
			change.Fill.ExecutedAt = timestamppb.New(first.Fill.ExecutedAt.AsTime().Add(30 * 24 * time.Hour))
			before := live.seq
			if err := live.Handle(ctx, factEnv("changed-later-clock", evtFilled), mustMarshal(t, change)); !errors.Is(err, fillfact.ErrExecutionIdentityConflict) || live.seq != before {
				t.Fatal("contradictory later clock bypassed durable identity", err)
			}
			if err := newView().Rehydrate(ctx); !errors.Is(err, fillfact.ErrExecutionIdentityConflict) {
				t.Fatal("restart crossed the unresolved tail", err)
			}
		})
	}
}

func TestVenueAccountLegacyEvictedCheckpointRefusesUnprovenTail(t *testing.T) {
	ctx := t.Context()
	clock := &fakeClock{t: time.Unix(1800000000, 0).UTC()}
	log := newMemLog()
	live := New(clock.now, nil, WithLog(log, testTenant), WithRetention(time.Hour))
	original := venueAccountExecution("legacy", "book-A", 2)
	deliver(t, live, "legacy", evtFilled, original)
	clock.t = clock.t.Add(2 * time.Hour)
	live.Evict(clock.t)
	if err := live.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	var old tvsyncpb.Checkpoint
	if err := proto.Unmarshal(log.cp, &old); err != nil {
		t.Fatal(err)
	}
	old.ExactEncodingVersion = 1
	old.Accounts[0].ExecutionHistoryVerified = false
	log.cp = mustMarshal(t, &old)
	// The restored source has an aggregate and a sequence, but no retained fill.
	log.facts[0] = Fact{EventID: "other", EventType: evtAccepted, Seq: 1, Knowledge: clock.t}
	restored := New(clock.now, nil, WithLog(log, testTenant), WithRetention(time.Hour))
	if err := restored.Rehydrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := restored.Handle(ctx, factEnv("late", evtFilled), mustMarshal(t, original)); !errors.Is(err, ErrUnresolvedFillAlias) || restored.seq != 1 || restored.Resident().Executions != 0 {
		t.Fatal("legacy aggregate permitted an unverified execution", err)
	}
}

func TestVenueAccountUnindexedLimitAndVerifiedReplacement(t *testing.T) {
	ctx := t.Context()
	source := newPool(t, testTenant)
	freshSchema(t, source)
	know := time.Unix(1800000000, 0).UTC()
	for i := range maxPriorExecutionCandidates + 1 {
		id := strconv.Itoa(i)
		e := venueAccountExecution("historical-"+id, "book-A", 1)
		e.Fill.FillId = "BTCUSDT-" + id
		e.Fill.VenueExecutionId = id
		data := mustMarshal(t, e)
		if _, err := source.Exec(ctx, "INSERT INTO tv_facts(event_id,event_type,payload,knowledge_at) VALUES($1,$2,$3,$4)", id, evtFilled, data, know); err != nil {
			t.Fatal(err)
		}
	}
	old := NewPostgresLog(source)
	fresh := venueAccountExecution("new-order", "book-B", 1)
	if _, err := old.priorExecution(ctx, maxPriorExecutionCandidates+2, "fund", fresh.Fill); !errors.Is(err, ErrUnresolvedFillAlias) {
		t.Fatal("unbounded historical search accepted", err)
	}

	// Populate a separate, empty projection from the original ordered FACTs.
	// The read-only replay alone cannot manufacture lookup evidence on its source.
	const replacementSchema = testSchema + "_replacement"
	targetPool := poolForTVSchema(t, testTenant, replacementSchema)
	freshTVSchema(t, targetPool, replacementSchema)
	target := NewPostgresLog(targetPool)
	if err := old.Replay(ctx, 0, func(f Fact) error {
		fresh, _, err := target.Append(ctx, f)
		if !fresh || err != nil {
			t.Fatal("replacement append", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, store := range []*PostgresLog{old, target} {
		view := New(func() time.Time { return know }, nil, WithLog(store, testTenant), WithRetention(time.Hour))
		if err := view.Rehydrate(ctx); err != nil {
			t.Fatal(err)
		}
		pos, err := view.Positions(testTenant, "fund", time.Time{})
		if err != nil || len(pos) != 1 || pos[0].Qty != strconv.Itoa(maxPriorExecutionCandidates+1) {
			t.Fatal("replacement prefix differs", pos, err)
		}
	}
	if seen, err := target.priorExecution(ctx, maxPriorExecutionCandidates+2, "fund", fresh.Fill); err != nil || seen {
		t.Fatal("replacement did not establish a disjoint account", err)
	}
	first := venueAccountExecution("historical-7", "book-A", 1)
	if seen, err := target.priorExecution(ctx, maxPriorExecutionCandidates+2, "fund", first.Fill); err != nil || !seen {
		t.Fatal("replacement lost earlier indexed execution", err)
	}
	var unchanged int
	if err := source.QueryRow(ctx, "SELECT count(*) FROM tv_facts WHERE fill_alias_digest IS NULL").Scan(&unchanged); err != nil || unchanged != maxPriorExecutionCandidates+1 {
		t.Fatal("original historical rows changed", err)
	}
}
