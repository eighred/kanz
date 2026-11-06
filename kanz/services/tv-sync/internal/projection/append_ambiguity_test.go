package projection

import (
	"context"
	"errors"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// The storage owner really commits; only the response delivered to Handle fails.
// This models uncertainty, not a claim to have injected transport loss into PG.
type committedErrorLog struct {
	Log
	failed bool
}

func (l *committedErrorLog) Append(ctx context.Context, f Fact) (bool, int64, error) {
	fresh, seq, err := l.Log.Append(ctx, f)
	if err == nil && fresh && !l.failed {
		l.failed = true
		return false, 0, errors.New("injected committed response loss")
	}
	return fresh, seq, err
}

type replayErrorLog struct {
	Log
	failed bool
}

func (l *replayErrorLog) Replay(ctx context.Context, after int64, fn func(Fact) error) error {
	return l.Log.Replay(ctx, after, func(f Fact) error {
		if err := fn(f); err != nil {
			return err
		}
		if !l.failed {
			l.failed = true
			return errors.New("injected cursor failure after folded prefix")
		}
		return nil
	})
}

func TestAmbiguousAppendRepairsDurablePrefix(t *testing.T) {
	for _, retry := range []bool{true, false} {
		t.Run(map[bool]string{true: "duplicate", false: "later-fact"}[retry], func(t *testing.T) { ambiguousAppend(t, newMemLog(), retry) })
	}
}
func TestPostgresAmbiguousAppendRepairsDurablePrefix(t *testing.T) {
	pool := newPool(t, testTenant)
	for _, retry := range []bool{true, false} {
		t.Run(map[bool]string{true: "duplicate", false: "later-fact"}[retry], func(t *testing.T) { freshSchema(t, pool); ambiguousAppend(t, NewPostgresLog(pool), retry) })
	}
}
func ambiguousAppend(t *testing.T, source Log, retry bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	now := time.Now().UTC()
	l := &committedErrorLog{Log: source}
	p := New(func() time.Time { return now }, nil, WithLog(l, testTenant))
	buy := mustMarshal(t, filledOrder("buy", "fund", "BTC", orderpb.Side_SIDE_BUY, 2, 100))
	if err := p.Handle(ctx, factEnv("buy", evtFilled), buy); err == nil {
		t.Fatal("committed response loss was acknowledged")
	}
	if p.seq != 0 {
		t.Fatal("uncertain append advanced fold watermark")
	}
	if err := p.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := source.LoadCheckpoint(ctx); err != nil || ok {
		t.Fatalf("unfolded append checkpointed: %v %v", ok, err)
	}
	now = now.Add(time.Minute)
	if retry {
		// Redelivery content is not the authority: use the first stored bytes/time.
		wrong := mustMarshal(t, filledOrder("buy", "fund", "BTC", orderpb.Side_SIDE_BUY, 99, 900))
		if err := p.Handle(ctx, factEnv("buy", evtFilled), wrong); err != nil {
			t.Fatal(err)
		}
	}
	deliver(t, p, "sell", evtFilled, filledOrder("sell", "fund", "BTC", orderpb.Side_SIDE_SELL, 1, 120))
	deliver(t, p, "buy", evtFilled, filledOrder("buy", "fund", "BTC", orderpb.Side_SIDE_BUY, 99, 900))
	assertRecoveredPosition(t, p)
	historical, err := p.Positions(testTenant, "fund", now.Add(-30*time.Second))
	if err != nil || len(historical) != 1 || historical[0].Qty != "2" {
		t.Fatalf("stored knowledge time lost: %+v %v", historical, err)
	}
	if err := p.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	restored := New(func() time.Time { return now }, nil, WithLog(source, testTenant))
	if err := restored.Rehydrate(ctx); err != nil {
		t.Fatal(err)
	}
	assertRecoveredPosition(t, restored)
	// Independent full log replay, without the checkpoint shortcut.
	full := New(func() time.Time { return now }, nil)
	if err := source.Replay(ctx, 0, func(f Fact) error { full.fold(testTenant, f.EventType, f.Payload, f.Knowledge); return nil }); err != nil {
		t.Fatal(err)
	}
	assertRecoveredPosition(t, full)
}
func assertRecoveredPosition(t *testing.T, p *Projection) {
	t.Helper()
	positions, err := p.Positions(testTenant, "fund", time.Time{})
	if err != nil || len(positions) != 1 || positions[0].Qty != "1" || positions[0].AvgPrice != "100" {
		t.Fatalf("want 2-1=1, got %+v %v", positions, err)
	}
	state, err := p.State(testTenant, "fund", time.Time{})
	if err != nil || state.RealizedPnl != "20" {
		t.Fatalf("want realized (120-100)*1=20: %+v %v", state, err)
	}
	executions, err := p.Executions(testTenant, "fund", "", time.Time{})
	if err != nil || len(executions) != 2 {
		t.Fatalf("want exactly two stored fills, got %+v %v", executions, err)
	}
}
func TestReplayFailureRetainsOnlyCompletedPrefix(t *testing.T) {
	source := newMemLog()
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		if _, _, err := source.Append(ctx, Fact{EventID: id, EventType: evtFilled, Payload: mustMarshal(t, filledOrder(id, "fund", "BTC", orderpb.Side_SIDE_BUY, 1, 100)), Knowledge: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	l := &replayErrorLog{Log: source}
	p := New(time.Now, nil, WithLog(l, testTenant))
	if err := p.Handle(ctx, factEnv("b", evtFilled), nil); err == nil {
		t.Fatal("cursor failure acknowledged")
	}
	if p.seq != 1 {
		t.Fatalf("partial replay watermark=%d", p.seq)
	}
	if err := p.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if source.cpSeq != 1 {
		t.Fatal("checkpoint skipped an unread fact")
	}
	if err := p.Handle(ctx, factEnv("b", evtFilled), nil); err != nil {
		t.Fatal(err)
	}
	positions, _ := p.Positions(testTenant, "fund", time.Time{})
	if p.seq != 2 || len(positions) != 1 || positions[0].Qty != "2" {
		t.Fatalf("resume replay double-folded/lost prefix: %d %+v", p.seq, positions)
	}
}

func TestPostgresAppendSerializesBeforeSequenceAllocation(t *testing.T) {
	pool := newPool(t, testTenant)
	var unsafe bool
	if err := pool.QueryRow(context.Background(), "SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user").Scan(&unsafe); err != nil || unsafe {
		t.Fatalf("requires non-bypass role: %v %v", unsafe, err)
	}
	for _, commit := range []bool{true, false} {
		t.Run(map[bool]string{true: "commit", false: "rollback-gap"}[commit], func(t *testing.T) {
			freshSchema(t, pool)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, factAppendLockSQL); err != nil {
				t.Fatal(err)
			}
			var first int64
			if err := tx.QueryRow(ctx, `INSERT INTO tv_facts(event_id,event_type,payload,knowledge_at) VALUES('held','test',$1,now()) RETURNING seq`, []byte{1}).Scan(&first); err != nil {
				t.Fatal(err)
			}
			// Another connection must time out BEFORE allocating a sequence, even though
			// its event identity differs (the unique constraint alone cannot serialize it).
			// Pre-establish a second connection so cancellation cannot merely be
			// a cold connection timeout instead of reaching the database boundary.
			spare, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := spare.Ping(ctx); err != nil {
				spare.Release()
				t.Fatal(err)
			}
			spare.Release()
			short, stop := context.WithTimeout(ctx, 500*time.Millisecond)
			_, _, err = NewPostgresLog(pool).Append(short, Fact{EventID: "blocked", EventType: "test", Payload: []byte{2}, Knowledge: time.Now()})
			stop()
			if err == nil {
				t.Fatal("append bypassed unresolved earlier transaction")
			}
			var allocated int64
			if err := tx.QueryRow(ctx, `SELECT last_value FROM tv_facts_seq_seq`).Scan(&allocated); err != nil {
				t.Fatal(err)
			}
			if allocated != first {
				t.Fatalf("sequence allocated before tenant lock: %d > %d", allocated, first)
			}
			// Different tenants have independent locks and cannot discover held rows.
			other := newPool(t, "other")
			if _, _, err := NewPostgresLog(other).Append(ctx, Fact{EventID: "held", EventType: "test", Payload: []byte{3}, Knowledge: time.Now()}); err != nil {
				t.Fatal(err)
			}
			if commit {
				err = tx.Commit(ctx)
			} else {
				err = tx.Rollback(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			fresh, last, err := NewPostgresLog(pool).Append(ctx, Fact{EventID: "next", EventType: "test", Payload: []byte{4}, Knowledge: time.Now()})
			if err != nil || !fresh || last <= first {
				t.Fatalf("append after resolved boundary: %v %d %v", fresh, last, err)
			}
			var ids []string
			if err := NewPostgresLog(pool).Replay(ctx, 0, func(f Fact) error { ids = append(ids, f.EventID); return nil }); err != nil {
				t.Fatal(err)
			}
			want := 1
			if commit {
				want = 2
			}
			if len(ids) != want || ids[len(ids)-1] != "next" {
				t.Fatalf("invalid committed tenant prefix: %v", ids)
			}
		})
	}
}
func TestReadOnlyLogCannotAppend(t *testing.T) {
	l := &PostgresLog{}
	if _, _, err := l.Append(context.Background(), Fact{EventID: "x"}); err == nil {
		t.Fatal("read-only verifier acquired write authority")
	}
}
