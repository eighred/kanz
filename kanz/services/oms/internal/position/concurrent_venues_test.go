package position

// #226 — folds of ONE instrument were not mutually exclusive, so a concurrent fill was
// dropped from the published fund-level aggregate.
//
// Apply locked ONE venue's row FOR UPDATE and then summed EVERY venue's row. Under READ
// COMMITTED, two fills folding at once each read the other venue at its PRE-FILL value, so
// the fund's total was never published by anybody.
//
// The durable rows stayed correct, which is what made it dangerous: nothing reconciles it.
// projector.go publishes the aggregate as an ABSOLUTE PositionState on a COMPACTED subject,
// so the short number is RETAINED, and the risk engine, the compliance monitor and tv-sync
// read a fund exposure short by one fill until the next fill in that instrument arrives.
//
// TestConcurrentFillsAtOneVenueKeepBothIsThe second defect the same lock closes, and it is
// strictly worse — there, the ROWS lose a fill.

import (
	"context"
	"math/big"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5"

	"github.com/eighred/kanz/internal/dec"
)

// foldRendezvous PINS THE INTERLEAVING instead of hoping for it.
//
// Two goroutines racing on a real database usually serialize by accident — the pool hands
// out connections at different moments, one wins the round trip, and the suite goes green on
// a broken store. This is a pgx QueryTracer that parks each transaction at a chosen
// statement and releases the parked ones only once every party has arrived. Parking on
// loadLot's `FOR UPDATE` puts both transactions in the exact state the defect needs: each
// has read the book, neither has written, neither has committed.
//
// The wait is BOUNDED because a correct store must never reach that state — the second fold
// blocks on the instrument lock and never arrives — so the barrier has to give up rather
// than hang the suite.
type foldRendezvous struct {
	parties  int
	timeout  time.Duration
	matchSQL string

	all  chan struct{} // closed once every party has parked
	once sync.Once

	mu                sync.Mutex
	armed             bool
	arrived           int
	releasedByBarrier bool
}

func newFoldRendezvous(parties int, timeout time.Duration, matchSQL string) *foldRendezvous {
	return &foldRendezvous{
		parties:  parties,
		timeout:  timeout,
		matchSQL: matchSQL,
		all:      make(chan struct{}),
	}
}

// arm turns the barrier on. Schema setup runs migrations through the same pool, so the
// tracer stays inert until the test proper begins.
func (r *foldRendezvous) arm() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.armed = true
}

// everyoneParked reports whether every fold reached the matched statement at all. It is the
// NON-VACUITY check: if the tracer's SQL never matched, or one goroutine died early, no
// concurrency was exercised and the run is not evidence. That is precisely how a
// concurrency test rots into a permanent pass.
func (r *foldRendezvous) everyoneParked() bool {
	select {
	case <-r.all:
		return true
	default:
		return false
	}
}

// overlapped reports whether the transactions were inside the fold AT THE SAME TIME — true
// if any party was released by the barrier rather than by its own timer.
//
// This is the direct probe of mutual exclusion. With the instrument lock in place the second
// fold cannot reach the matched statement until the first has committed, so the first party
// is always released by its timer and this is FALSE. Without the lock both park together and
// it is TRUE. Reported rather than asserted — a different sound fix could serialize
// elsewhere — but it is what tells a reader whether the race window was actually entered.
func (r *foldRendezvous) overlapped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.releasedByBarrier
}

func (r *foldRendezvous) meet() {
	r.mu.Lock()
	if !r.armed {
		r.mu.Unlock()
		return
	}
	r.arrived++
	full := r.arrived >= r.parties
	r.mu.Unlock()

	if full {
		r.once.Do(func() { close(r.all) })
	}
	timer := time.NewTimer(r.timeout)
	defer timer.Stop()
	select {
	case <-r.all:
		// Only a party that was WAITING when the barrier filled overlapped with another.
		// The one that filled it arrives to an already-closed channel and never waits.
		if !full {
			r.mu.Lock()
			r.releasedByBarrier = true
			r.mu.Unlock()
		}
	case <-timer.C:
	}
}

type tracedSQLKey struct{}

func (r *foldRendezvous) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, tracedSQLKey{}, d.SQL)
}

func (r *foldRendezvous) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	sql, _ := ctx.Value(tracedSQLKey{}).(string)
	if d.Err != nil || !strings.Contains(sql, r.matchSQL) {
		return
	}
	r.meet()
}

// forUpdateLoad is loadLot's read — the last statement before either fold writes anything.
const forUpdateLoad = "FOR UPDATE"

// concurrentFolds runs one Apply per entry of fills, all at once, parked against rz, and
// returns each one's published FUND-LEVEL aggregate keyed by venue.
//
// It fails the test rather than returning if a fold errored: a fill that errors out of
// Projector.Handle is DLQ'd, not retried — no bus consumer in this estate wires
// bus.WithRetry and MaxAttempts is 1 — so an error is not an acceptable outcome for routine
// write contention, and a fix that produced one would be a regression, not a pass.
func concurrentFolds(t *testing.T, rz *foldRendezvous, store *Postgres, portfolio string,
	fills []*labelledFill, now time.Time) map[string]*big.Rat {
	t.Helper()

	type result struct {
		key string
		agg *big.Rat
		err error
	}
	out := make(chan result, len(fills))
	var start sync.WaitGroup
	start.Add(1)
	for _, f := range fills {
		go func(f *labelledFill) {
			start.Wait()
			applied, err := store.Apply(context.Background(), portfolio, f.fill, now, nil)
			if err != nil {
				out <- result{key: f.key, err: err}
				return
			}
			out <- result{key: f.key, agg: dec.FromProto(applied.Aggregate.GetQuantity())}
		}(f)
	}
	start.Done()

	published := map[string]*big.Rat{}
	var failures []result
	for range fills {
		r := <-out
		if r.err != nil {
			failures = append(failures, r)
			continue
		}
		published[r.key] = r.agg
	}

	if !rz.everyoneParked() {
		t.Fatalf("only %d of %d folds reached %q — the race window was never set up, so this run "+
			"is not evidence of anything", len(published)+len(failures), len(fills), rz.matchSQL)
	}
	t.Logf("the folds overlapped inside the transaction: %v (false ⇒ the instrument lock "+
		"serialized them)", rz.overlapped())

	for _, r := range failures {
		t.Fatalf("%s fold failed: %v — a fill that errors out of Projector.Handle is DLQ'd, not "+
			"retried (MaxAttempts is 1 estate-wide), so this is not an acceptable outcome for "+
			"routine write contention", r.key, r.err)
	}
	return published
}

// labelledFill pairs a fill with the label its result is reported under.
type labelledFill struct {
	key  string
	fill *orderpb.Fill
}

// TestConcurrentFillsAtTwoVenuesPublishTheWholeFund is THE test for #226.
//
// One portfolio, one instrument, two venues, two fills folding AT THE SAME TIME — the normal
// path, not a corner: oms-deploy.yaml runs `replicas: 2` and order/service.go dispatches from
// three independent goroutines.
//
// # What is asserted, and why it is not "both publish the total"
//
// #226 asks for both aggregates to equal the sum. NO CORRECT STORE CAN DO THAT. Once the two
// folds are properly serialized, the one that commits FIRST genuinely holds only its own fill
// — 1 BTC is the fund's true position at that instant, and publishing it is right. Requiring
// both to say 3 would require the first to publish a number the fund did not yet hold.
//
// The real invariant is that every published aggregate must be a state the book ACTUALLY
// PASSED THROUGH, and the fund's whole holding must be among them:
//
//	correct:  {1, 3}  or  {2, 3}   — one venue alone, then the fund
//	the bug:  {1, 2}               — a chain that never reaches 3
//
// {1, 2} is the signature of the defect: each transaction summed the other venue at its
// PRE-FILL value, so THE TOTAL IS NEVER PUBLISHED BY ANYBODY. The subject is compacted, so
// the last of those two short numbers is what the risk engine, the compliance monitor and
// tv-sync retain — permanently, because the durable rows are right and nothing reconciles it.
func TestConcurrentFillsAtTwoVenuesPublishTheWholeFund(t *testing.T) {
	// Long enough that a loaded machine cannot mistake a slow round trip for serialization,
	// short enough that the fixed path — which always burns exactly one of these, because
	// the second fold is blocked on the lock and cannot arrive — stays a few seconds.
	rz := newFoldRendezvous(2, 3*time.Second, forUpdateLoad)

	pool := newTracedPool(t, "__system__", rz)
	freshSchema(t, pool)
	now := time.Now().UTC()
	store := NewPostgres(pool, "USD")

	total := big.NewRat(3, 1) // the fund holds 3 BTC once both fills have landed
	rz.arm()

	published := concurrentFolds(t, rz, store, "fund-alpha", []*labelledFill{
		{key: "XBIN", fill: buyAtVenue("cv-bin", "BTC-USD", "XBIN", "1", "50000", now)},
		{key: "XOKX", fill: buyAtVenue("cv-okx", "BTC-USD", "XOKX", "2", "50000", now)},
	}, now)

	// Every published aggregate must be a state the book actually passed through: one
	// venue's fill alone (that fold committed first), or the fund's whole holding.
	held := map[string]bool{"1": true, "2": true, "3": true}
	for venue, got := range published {
		if !held[got.RatString()] {
			t.Errorf("%s published fund-level BTC = %s, which the fund NEVER HELD — the book went "+
				"0 -> 1 -> 3 or 0 -> 2 -> 3", venue, got.RatString())
		}
	}

	// And the fund's whole holding must be among them. This is the assertion the defect
	// fails: both folds summing the other venue at its pre-fill value publish {1, 2}, and
	// 3 — the number the compacted subject has to end up holding — is never emitted.
	if !containsTotal(published, total) {
		t.Errorf("neither fold published the fund's whole BTC position (%s); published %s.\n\n"+
			"Each summed the other venue at its PRE-FILL value, so the total was never emitted at "+
			"all. The positions ROWS are correct, which is what makes this unrecoverable: the "+
			"compacted subject retains a short exposure and nothing reconciles it until the next "+
			"fill in this instrument happens to arrive.", total.RatString(), fmtAggregates(published))
	}

	assertBookHolds(t, store, "fund-alpha", total, now)
}

// TestConcurrentFillsAtOneVenueKeepBoth is the SECOND defect the instrument lock closes, and
// it is strictly worse than #226: here the DURABLE ROWS lose a fill, not just the FACT.
//
// Two fills at the SAME venue for a holding that does not exist yet. loadLot's `FOR UPDATE`
// matches no row — Postgres cannot lock a row that has not been created — so both folds read
// a ZERO lot and both compute their quantity from zero. The first INSERT creates the row; the
// second blocks on the primary key and then takes the `DO UPDATE SET quantity =
// EXCLUDED.quantity` branch, OVERWRITING the first fill's quantity with its own.
//
// That is the failure 0004_positions_per_venue.sql was written to end ("the second venue's
// fill OVERWROTE the first — the book said 2 BTC and the fund had 3"), reappearing on the
// venue's own axis because the lock never covered the row's CREATION.
//
// It also explains why widening the FOR UPDATE to the portfolio+instrument predicate — the
// remedy #226 proposes — closes nothing: a predicate cannot lock rows that do not exist.
func TestConcurrentFillsAtOneVenueKeepBoth(t *testing.T) {
	rz := newFoldRendezvous(2, 3*time.Second, forUpdateLoad)

	pool := newTracedPool(t, "__system__", rz)
	freshSchema(t, pool)
	now := time.Now().UTC()
	store := NewPostgres(pool, "USD")

	total := big.NewRat(3, 1)
	rz.arm()

	published := concurrentFolds(t, rz, store, "fund-alpha", []*labelledFill{
		{key: "fill-1", fill: buyAtVenue("sv-1", "BTC-USD", "XBIN", "1", "50000", now)},
		{key: "fill-2", fill: buyAtVenue("sv-2", "BTC-USD", "XBIN", "2", "50000", now)},
	}, now)

	if !containsTotal(published, total) {
		t.Errorf("neither fold published the venue's whole BTC holding (%s); published %s",
			total.RatString(), fmtAggregates(published))
	}

	// THE ROWS. Both fills were claimed in position_fills — each is recorded as folded
	// exactly once — so a lost quantity here can never be recovered by a redelivery: the
	// dedup ledger says the fill was already applied. The fund's book is simply wrong.
	assertBookHolds(t, store, "fund-alpha", total, now)
}

func containsTotal(published map[string]*big.Rat, total *big.Rat) bool {
	for _, got := range published {
		if got.Cmp(total) == 0 {
			return true
		}
	}
	return false
}

func fmtAggregates(published map[string]*big.Rat) string {
	parts := make([]string, 0, len(published))
	for k, v := range published {
		parts = append(parts, k+"="+v.RatString())
	}
	sort.Strings(parts)
	return "{" + strings.Join(parts, " ") + "}"
}

// assertBookHolds checks the DURABLE rows through the read side the pre-trade gate uses.
// #226's aggregate is a publishing defect and leaves these correct; asserting them is what
// separates it from a fix that repaired the FACT by corrupting the book.
func assertBookHolds(t *testing.T, store *Postgres, portfolio string, want *big.Rat, now time.Time) {
	t.Helper()
	snap, err := store.Snapshot(context.Background(), portfolio, now)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if n := len(snap.GetPositions()); n != 1 {
		t.Fatalf("snapshot has %d positions, want 1", n)
	}
	if got := dec.FromProto(snap.GetPositions()[0].GetQuantity()); got.Cmp(want) != 0 {
		t.Errorf("the DURABLE book holds %s BTC, want %s — a fill was lost from the rows "+
			"themselves, and position_fills already records it as folded, so no redelivery "+
			"will ever put it back", got.RatString(), want.RatString())
	}
}
