package ledger

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type checkpointTraceKey struct{}

// Pause AFTER the checkpoint query has established its database snapshot, so
// the writer can commit a backdated action and an ordinary newer tail entry.
type checkpointReadPause struct {
	used   atomic.Bool
	loaded chan struct{}
	resume chan struct{}
}

func (p *checkpointReadPause) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.TrimSpace(data.SQL), "SELECT positions, cash, accrued") {
		return context.WithValue(ctx, checkpointTraceKey{}, true)
	}
	return ctx
}

func (p *checkpointReadPause) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if marked, _ := ctx.Value(checkpointTraceKey{}).(bool); marked && p.used.CompareAndSwap(false, true) {
		close(p.loaded)
		select {
		case <-p.resume:
		case <-ctx.Done():
		}
	}
}

func TestPostgresCurrentBookCannotMixInvalidatedCheckpointWithNewTail(t *testing.T) {
	pool := newPool(t)
	writer := NewPostgres(pool)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	hold := trade("hold", "AAPL", "100", "10", 1, 10)
	hold.Cash = nil
	if err := writer.Append(ctx, hold, nil); err != nil {
		t.Fatal(err)
	}
	if err := writer.SaveSnapshot(ctx, replayJournal(t, writer, "PF").Snapshot(day(10))); err != nil {
		t.Fatal(err)
	}
	pause := &checkpointReadPause{loaded: make(chan struct{}), resume: make(chan struct{})}
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = pause
	readerPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer readerPool.Close()
	type result struct {
		book *Book
		err  error
	}
	done := make(chan result, 1)
	go func() {
		book, _, err := MaterializeCurrent(ctx, NewPostgres(readerPool), "PF")
		done <- result{book, err}
	}()
	select {
	case <-pause.loaded:
	case <-ctx.Done():
		t.Fatal("reader never reached checkpoint")
	}
	if err := writer.Append(ctx, income(1, "1", 2), nil); err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(ctx, trade("new-tail", "AAPL", "1", "10", 11, 11), nil); err != nil {
		t.Fatal(err)
	}
	close(pause.resume)
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		// The read began before the two commits: it must retain that coherent
		// view, not combine old accrued=0 with the new trade's cash=-10.
		assertIncome(t, r.book, "0", "0", "0")
	case <-ctx.Done():
		t.Fatal("reader did not finish")
	}
	book, _, err := MaterializeCurrent(ctx, writer, "PF")
	if err != nil {
		t.Fatal(err)
	}
	assertIncome(t, book, "-10", "100", "0")
}
