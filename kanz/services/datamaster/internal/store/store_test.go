package store

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/services/datamaster/internal/master"
	"github.com/eighred/kanz/services/datamaster/internal/pricing"
)

func sampleGolden() master.SecurityMaster {
	return master.SecurityMaster{
		InstrumentID: "INST-1",
		Identifiers:  master.Identifiers{ISIN: "US0378331005"},
		AssetClass:   "EQUITY",
		CurrencyCode: "USD",
		Description:  "Apple Inc.",
		Provenance:   map[string]string{"asset_class": "bloomberg"},
	}
}

func sampleException() pricing.Exception {
	return pricing.Exception{
		ID:           "INST-1:STALE_PRICE:vendorX",
		Kind:         pricing.KindStalePrice,
		InstrumentID: "INST-1",
		Detail:       "candidate older than 24h",
		Status:       pricing.StatusOpen,
		DetectedAt:   time.Unix(1_700_000_000, 0).UTC(),
	}
}

// runGoldenContract exercises a GoldenStore: replace-on-write + miss.
func runGoldenContract(t *testing.T, ctx context.Context, gs GoldenStore) {
	t.Helper()
	if _, ok, err := gs.Get(ctx, "missing"); err != nil || ok {
		t.Fatalf("get missing: ok=%v err=%v, want false/nil", ok, err)
	}
	rec := sampleGolden()
	if err := gs.Put(ctx, rec); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok, err := gs.Get(ctx, "INST-1")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Description != rec.Description || got.Identifiers.ISIN != rec.Identifiers.ISIN {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	// Replace-on-write.
	rec.Description = "Apple Inc. (updated)"
	if err := gs.Put(ctx, rec); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got, _, _ := gs.Get(ctx, "INST-1"); got.Description != rec.Description {
		t.Fatalf("replace not applied: %q", got.Description)
	}
}

// runExceptionContract exercises an ExceptionStore: idempotent add, append-only
// override, open-queue filtering.
func runExceptionContract(t *testing.T, ctx context.Context, es ExceptionStore) {
	t.Helper()
	ex := sampleException()
	if err := es.Add(ctx, ex); err != nil {
		t.Fatalf("add: %v", err)
	}
	// Idempotent re-add keeps the entry (no error, no duplicate, state preserved).
	if err := es.Add(ctx, ex); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	open, err := es.Open(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if len(open) != 1 || open[0].ID != ex.ID {
		t.Fatalf("open = %d entries, want 1 (%s)", len(open), ex.ID)
	}

	at := time.Unix(1_700_001_000, 0).UTC()
	if err := es.Override(ctx, ex.ID, pricing.Override{Actor: "ops@kanz", Reason: "vendor confirmed", ChosenPrice: dec.Rat("152.40"), At: at}); err != nil {
		t.Fatalf("override: %v", err)
	}
	// Override requires actor + reason.
	if err := es.Override(ctx, ex.ID, pricing.Override{ChosenPrice: dec.Rat("1"), At: at}); err == nil {
		t.Fatal("override without actor/reason should error")
	}
	// Unknown id errors.
	if err := es.Override(ctx, "nope", pricing.Override{Actor: "a", Reason: "r", ChosenPrice: dec.Rat("1"), At: at}); err == nil {
		t.Fatal("override unknown id should error")
	}

	got, ok, err := es.Get(ctx, ex.ID)
	if err != nil || !ok {
		t.Fatalf("get after override: ok=%v err=%v", ok, err)
	}
	if got.Status != pricing.StatusOverridden {
		t.Fatalf("status = %s, want OVERRIDDEN", got.Status)
	}
	if len(got.Overrides) != 1 || got.Overrides[0].Actor != "ops@kanz" {
		t.Fatalf("override trail = %+v", got.Overrides)
	}
	// An OVERRIDDEN entry leaves the open queue.
	if open, _ := es.Open(ctx); len(open) != 0 {
		t.Fatalf("open after override = %d, want 0", len(open))
	}
}

func TestMemoryGoldenStore(t *testing.T) {
	runGoldenContract(t, context.Background(), NewMemoryGoldenStore())
}

func TestQueueStore(t *testing.T) {
	runExceptionContract(t, context.Background(), NewQueueStore(nil))
}

// --- Postgres (DB-gated, mirrors the risk-engine persist tests) -----------

const migrationDir = "../../migrations"

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to run datamaster store Postgres integration tests")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", "__system__")
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	applySchema(t, pool)
	return pool
}

func applySchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	// exception_override_proposals is in this list because 0004 creates it. A
	// table missing from here does not fail the FIRST gated run — it fails the
	// SECOND, on "already exists", which reads as a broken migration rather than
	// an incomplete teardown.
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS outbox, exception_override_proposals, exception_overrides, exceptions, golden_records CASCADE`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(migrationDir, "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob migrations %s: %v (found %d)", migrationDir, err, len(files))
	}
	sort.Strings(files)
	for _, f := range files {
		ddl, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read migration %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(ddl)); err != nil {
			t.Fatalf("apply migration %s: %v", f, err)
		}
	}
}

func TestPostgresGoldenStore(t *testing.T) {
	pool := newPool(t)
	runGoldenContract(t, context.Background(), NewPostgresGolden(pool))
}

func TestPostgresExceptionStore(t *testing.T) {
	pool := newPool(t)
	runExceptionContract(t, context.Background(), NewPostgresExceptions(pool, "__system__"))
}

// TestPostgresOverridePriceIsExact pins DATA-M8b at the durable boundary.
//
// chosen_price was DOUBLE PRECISION, described in 0001 as "an oversight statistic,
// not a stored price". It is neither: it is the price a NAMED HUMAN chose when
// accepting a break, stored in an append-only audit trail. 123456789.123456789 has
// 18 significant digits — more than an IEEE-754 double can hold — so a double
// column silently returns 123456789.12345679 and the compliance record no longer
// says what the human decided. TEXT holding the rational's exact RatString (the
// accounting ledger's stance for money) round-trips it untouched.
func TestPostgresOverridePriceIsExact(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	es := NewPostgresExceptions(pool, "__system__")

	ex := pricing.Exception{
		ID: "EXACT:PRICE_TOLERANCE:ICE", Kind: pricing.KindPriceTolerance,
		InstrumentID: "EXACT", Detail: "outlier", Status: pricing.StatusOpen,
		DetectedAt: time.Unix(1_700_000_000, 0).UTC(),
	}
	if err := es.Add(ctx, ex); err != nil {
		t.Fatal(err)
	}

	// 19 SIGNIFICANT DIGITS AT THE PLATFORM'S SCALE (8 decimals). A float64 holds
	// about 15-17, so this still proves what the test was written to prove: the
	// column must be TEXT, because `double` would round the price a named human
	// chose (DATA-M8b).
	//
	// IT USED TO CARRY NINE DECIMALS, which no longer survives the store. Since
	// #410 the override is announced as a FACT in the same transaction that
	// records it, and common.v1.Decimal has a fixed scale of 8 — so a
	// nine-decimal price is storable and NOT announceable, and the enqueue fails
	// the whole override rather than committing a decision the audit trail will
	// never hear about.
	//
	// That narrowing is deliberate and was already the API's behaviour: the HTTP
	// handler refuses an over-precise price with a 400, using the same check.
	// This test reached the store directly and was the only caller that could
	// still get past it. The column's losslessness is unchanged and is what the
	// assertion below still measures — it is the FIXTURE that had to move, not
	// the property.
	const chosen = "12345678901.12345678"
	want := dec.Rat(chosen)
	if err := es.Override(ctx, ex.ID, pricing.Override{Actor: "alice@kanz", Reason: "vendor confirmed", ChosenPrice: want, At: time.Unix(1_700_000_001, 0).UTC()}); err != nil {
		t.Fatal(err)
	}

	got, ok, err := es.Get(ctx, ex.ID)
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if len(got.Overrides) != 1 {
		t.Fatalf("override trail = %d entries, want 1", len(got.Overrides))
	}
	if got.Overrides[0].ChosenPrice.Cmp(want) != 0 {
		t.Fatalf("chosen price round-tripped as %s, want exactly %s — the audit trail no longer says what the human chose",
			got.Overrides[0].ChosenPrice.FloatString(9), chosen)
	}
}

// TestPostgresCycleLockElectsOneReplica drives the real advisory lock across two
// sessions — the two pods.
//
// Concurrent projection is harmless to the DATA (idempotent, content-identical
// writes), but every pod runs the projector and vendor reference data is metered
// per call: at replicas: 2 that is double the vendor bill and double the rate-limit
// budget for one cycle's worth of information. One replica wins, the other skips.
func TestPostgresCycleLockElectsOneReplica(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	podA := NewPostgresCycleLock(pool, "acme-capital")
	podB := NewPostgresCycleLock(pool, "acme-capital")

	releaseA, wonA, err := podA.TryAcquire(ctx)
	if err != nil || !wonA {
		t.Fatalf("pod A did not win an uncontended cycle: won=%v err=%v", wonA, err)
	}

	// TRY, not wait: pod B does not block behind A and then run a redundant refresh.
	_, wonB, err := podB.TryAcquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if wonB {
		t.Fatal("both replicas won the cycle — every vendor call would be made twice")
	}

	// The lock is handed back, so the next cycle is not starved by this one.
	releaseA()
	releaseB, wonB, err := podB.TryAcquire(ctx)
	if err != nil || !wonB {
		t.Fatalf("pod B could not take the released cycle: won=%v err=%v — the projector would never run again", wonB, err)
	}
	releaseB()
}

// The lock is namespaced per TENANT. A single shared key would let one tenant's
// deployment starve every other tenant's projector: the loser skips, every cycle,
// forever, and its security master would simply never be refreshed.
func TestPostgresCycleLockIsPerTenant(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	acme := NewPostgresCycleLock(pool, "acme-capital")
	other := NewPostgresCycleLock(pool, "other-fund")
	if acme.Key() == other.Key() {
		t.Fatal("two tenants share a cycle-lock key — one would starve the other forever")
	}

	releaseAcme, won, err := acme.TryAcquire(ctx)
	if err != nil || !won {
		t.Fatalf("acme: won=%v err=%v", won, err)
	}
	defer releaseAcme()

	releaseOther, won, err := other.TryAcquire(ctx)
	if err != nil || !won {
		t.Fatal("a second tenant was blocked by the first tenant's cycle — its master would never refresh")
	}
	releaseOther()
}
