package position

import (
	"context"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/internal/platform/subject"
	"github.com/eighred/kanz/pkg/bus"
)

type foldSample struct {
	lock, transaction time.Duration
}

type sampleKey struct{}
type lockStartKey struct{}
type foldTimings struct{}

func (foldTimings) TraceQueryStart(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryStartData) context.Context {
	if ctx.Value(sampleKey{}) != nil && strings.Contains(q.SQL, "pg_advisory_xact_lock") {
		return context.WithValue(ctx, lockStartKey{}, time.Now())
	}
	return ctx
}

func (foldTimings) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if started, ok := ctx.Value(lockStartKey{}).(time.Time); ok {
		ctx.Value(sampleKey{}).(*foldSample).lock = time.Since(started)
	}
}

// TestPostgresPositionFoldContentionProfile reports client-observed lock query and
// full transaction durations. The database/outbox invariants are acceptance checks.
// Timing depends on the runner and workload and carries no production threshold.
func TestPostgresPositionFoldContentionProfile(t *testing.T) {
	const workers, perWorker = 4, 12
	const portfolio = "capacity"
	for _, keys := range []int{1, workers} {
		t.Run(fmt.Sprintf("%d_keys", keys), func(t *testing.T) {
			pool := newTracedPool(t, testTenant, foldTimings{})
			ctx := bus.WithTenantID(context.Background(), testTenant)
			var privileged bool
			if err := pool.QueryRow(ctx, "SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user").Scan(&privileged); err != nil || privileged {
				t.Fatalf("require a NOSUPERUSER NOBYPASSRLS role: privileged=%t err=%v", privileged, err)
			}
			freshSchema(t, pool)
			book := NewPostgres(pool, "USD")
			projector := &Projector{tenant: testTenant}
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			instrument := func(key int) string { return fmt.Sprintf("ASSET-%d", key) }
			apply := func(ctx context.Context, worker, seq int) error {
				inst := instrument(worker % keys)
				fill := buy(fmt.Sprintf("w%d-f%02d", worker, seq), inst, "1", "100", at)
				key := subject.PositionFor(testTenant, portfolio, inst)
				_, err := book.Apply(ctx, portfolio, fill, at, func(ctx context.Context, a *Applied) ([]outbox.Record, error) {
					return projector.records(ctx, key, portfolio, fill, a)
				})
				return err
			}
			var group sync.WaitGroup
			var samples [workers * perWorker]foldSample
			errs := make(chan error, workers)
			start := make(chan struct{})
			for worker := range workers {
				group.Go(func() {
					<-start
					for seq := range perWorker {
						s := &samples[worker*perWorker+seq]
						call := context.WithValue(ctx, sampleKey{}, s)
						begin := time.Now()
						if err := apply(call, worker, seq); err != nil {
							errs <- err
							return
						}
						s.transaction = time.Since(begin)
					}
				})
			}
			begin := time.Now()
			close(start)
			group.Wait()
			elapsed := time.Since(begin)
			close(errs)
			for err := range errs {
				t.Fatal(err)
			}
			// Redelivery reannounces the current absolute state but cannot grow the book.
			for worker := range workers {
				if err := apply(ctx, worker, 0); err != nil {
					t.Fatal(err)
				}
			}
			var fills, records int
			if err := pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM position_fills), (SELECT count(*) FROM outbox)").Scan(&fills, &records); err != nil {
				t.Fatal(err)
			}
			if fills != len(samples) || records != 2*(len(samples)+workers) {
				t.Fatalf("claims=%d outbox records=%d", fills, records)
			}
			perKey := len(samples) / keys
			snap, err := book.Snapshot(ctx, portfolio, at)
			if err != nil || len(snap.GetPositions()) != keys {
				t.Fatalf("positions=%d want=%d error=%v", len(snap.GetPositions()), keys, err)
			}
			for _, pos := range snap.GetPositions() {
				if dec.FromProto(pos.GetQuantity()).Cmp(big.NewRat(int64(perKey), 1)) != 0 {
					t.Fatalf("unexpected snapshot quantity for %s: %v", pos.GetInstrumentId(), pos.GetQuantity())
				}
			}
			for key := range keys {
				subj := subject.PositionFor(testTenant, portfolio, instrument(key))
				rows, err := pool.Query(ctx, "SELECT payload FROM outbox WHERE subject=$1 ORDER BY id", subj)
				if err != nil {
					t.Fatal(err)
				}
				payloads, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
				if err != nil || len(payloads) != perKey+workers/keys {
					t.Fatalf("subject=%s records=%d error=%v", subj, len(payloads), err)
				}
				for i, data := range payloads {
					var state domainpb.PositionState
					if err := proto.Unmarshal(data, &state); err != nil {
						t.Fatal(err)
					}
					want := big.NewRat(int64(min(i+1, perKey)), 1)
					if dec.FromProto(state.GetQuantity()).Cmp(want) != 0 {
						t.Fatalf("subject=%s record=%d quantity=%v want=%s", subj, i, state.GetQuantity(), want)
					}
				}
			}
			var locks, transactions []time.Duration
			for _, sample := range samples {
				if sample.lock <= 0 || sample.transaction < sample.lock {
					t.Fatalf("invalid duration sample: %+v", sample)
				}
				locks = append(locks, sample.lock)
				transactions = append(transactions, sample.transaction)
			}
			t.Logf("workers=%d keys=%d fills=%d redeliveries=%d records=%d quantity_per_key=%d fills/s=%.1f", workers, keys, fills, workers, records, perKey, float64(fills)/elapsed.Seconds())
			for _, series := range []struct {
				name  string
				times []time.Duration
			}{{"advisory_lock_query", locks}, {"transaction", transactions}} {
				slices.Sort(series.times)
				q := func(percent int) time.Duration { return series.times[(len(series.times)*percent+99)/100-1] }
				t.Logf("%s n=%d p50=%v p95=%v p99=%v max=%v", series.name, len(series.times), q(50), q(95), q(99), series.times[len(series.times)-1])
			}
		})
	}
}
