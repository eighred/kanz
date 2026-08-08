// THE CONTRACT-TERMS STORE, AGAINST A REAL POSTGRES (#345).
//
// Gated on TEST_POSTGRES_URL. Every assertion here is about SQL behaviour —
// point-in-time selection, DISTINCT ON, a partial index's reverse lookup — so a
// fake would be testing the fake. kanz/test/backing/up.sh provides the database.
//
// Each test builds its own schema so the suite cannot collide with a shared
// contract_terms or with a developer's real one (#212: a test that proves
// isolation by destroying the thing it protects is its own bug report).
package terms

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"
)

var t0 = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL (kanz/test/backing/up.sh) to run the contract-terms store tests")
	}
	ctx := context.Background()

	schema := fmt.Sprintf("terms_test_%d", time.Now().UnixNano())
	boot, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect (bootstrap): %v", err)
	}
	defer boot.Close()
	if _, err := boot.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	// search_path WITHOUT public, so an unqualified name resolves here and
	// nowhere else — a missing object errors instead of silently hitting a
	// shared table.
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		drop, derr := pgxpool.New(context.Background(), url)
		if derr != nil {
			t.Errorf("reconnect to drop schema %s: %v — it is now residue", schema, derr)
			return
		}
		defer drop.Close()
		if _, derr := drop.Exec(context.Background(),
			`DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`); derr != nil {
			t.Errorf("drop schema %s: %v — it is now residue", schema, derr)
		}
	})

	b, err := os.ReadFile(filepath.Join("..", "..", "..", "services", "market-data", "migrations",
		"0002_contract_terms.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(b)); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	return pool
}

func dec(coeff int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coeff, Exponent: exp}
}

func optionRec(id, underlying string, strike int64, asOf time.Time, mult int64) Record {
	return Record{
		InstrumentID: id,
		AsOf:         asOf,
		UnderlyingID: underlying,
		Kind:         KindOption,
		Terms: &referencepb.ContractTerms{
			InstrumentId: id,
			AsOf:         timestamppb.New(asOf),
			Terms: &referencepb.ContractTerms_Option{Option: &referencepb.OptionTerms{
				UnderlyingId:       underlying,
				Strike:             dec(strike, 0),
				Expiry:             timestamppb.New(asOf.AddDate(0, 3, 0)),
				OptionType:         referencepb.OptionType_OPTION_TYPE_CALL,
				ExerciseStyle:      referencepb.ExerciseStyle_EXERCISE_STYLE_EUROPEAN,
				ContractMultiplier: dec(mult, 0),
			}},
		},
	}
}

// A STORED RECORD ROUND-TRIPS THROUGH THE PROTO BLOB.
func TestPutAndLatestAsOfRoundTrip(t *testing.T) {
	st := NewPostgres(newPool(t))
	ctx := context.Background()

	if err := st.Put(ctx, []Record{optionRec("BTC-60000-C", "BTC-USD", 60000, t0, 1)}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := st.LatestAsOf(ctx, "BTC-60000-C", t0)
	if err != nil {
		t.Fatalf("LatestAsOf: %v", err)
	}
	opt := got.Terms.GetOption()
	if opt == nil {
		t.Fatal("the stored ContractTerms came back with no option variant — the oneof did not survive")
	}
	if opt.GetStrike().GetCoefficient() != 60000 {
		t.Errorf("strike = %v, want 60000", opt.GetStrike())
	}
	if got.UnderlyingID != "BTC-USD" {
		t.Errorf("underlying = %q, want BTC-USD", got.UnderlyingID)
	}
	if got.Kind != KindOption {
		t.Errorf("kind = %q, want OPTION", got.Kind)
	}
}

// AN AMENDMENT IS A NEW ROW, AND HISTORY STILL READS AS IT WAS.
//
// This is the property the whole as_of design exists for. Corporate actions
// amend listed options — strikes and multipliers change — and a store that
// overwrote would reprice yesterday against today's terms, silently.
func TestLatestAsOfReadsTheTermsThatAppliedThen(t *testing.T) {
	st := NewPostgres(newPool(t))
	ctx := context.Background()
	later := t0.AddDate(0, 1, 0)

	if err := st.Put(ctx, []Record{
		optionRec("BTC-60000-C", "BTC-USD", 60000, t0, 1),
		optionRec("BTC-60000-C", "BTC-USD", 30000, later, 2), // a 2:1 adjustment
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	before, err := st.LatestAsOf(ctx, "BTC-60000-C", later.Add(-time.Second))
	if err != nil {
		t.Fatalf("LatestAsOf(before): %v", err)
	}
	if got := before.Terms.GetOption().GetStrike().GetCoefficient(); got != 60000 {
		t.Fatalf("as of just before the amendment the strike is %d, want 60000 — the amendment "+
			"overwrote history, so a backtest would reprice the past against terms that did not "+
			"apply to it", got)
	}

	after, err := st.LatestAsOf(ctx, "BTC-60000-C", later)
	if err != nil {
		t.Fatalf("LatestAsOf(after): %v", err)
	}
	if got := after.Terms.GetOption().GetStrike().GetCoefficient(); got != 30000 {
		t.Fatalf("as of the amendment the strike is %d, want 30000 — the newer record is not being "+
			"selected", got)
	}
}

// MISSING TERMS ARE AN ERROR, NOT AN EMPTY ANSWER.
//
// compute.TermsProvider returns ok=false for "not an option", which a caller
// treats as "price it linearly". A derivative whose terms were never loaded must
// not reach that branch — it would be priced as if it were a share. The store
// distinguishes them so the caller can.
func TestLatestAsOfReportsMissingTermsDistinctly(t *testing.T) {
	st := NewPostgres(newPool(t))

	_, err := st.LatestAsOf(context.Background(), "NEVER-LOADED", t0)
	if !errors.Is(err, ErrNoTerms) {
		t.Fatalf("err = %v, want ErrNoTerms.\n\n"+
			"An instrument with no terms must be distinguishable from one that is not a derivative: "+
			"the second is priced linearly on purpose, the first would be priced linearly by mistake.", err)
	}
}

// AN as_of BEFORE THE FIRST RECORD IS ALSO MISSING — the terms did not exist yet.
func TestLatestAsOfBeforeTheFirstRecordIsMissing(t *testing.T) {
	st := NewPostgres(newPool(t))
	ctx := context.Background()

	if err := st.Put(ctx, []Record{optionRec("BTC-60000-C", "BTC-USD", 60000, t0, 1)}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	_, err := st.LatestAsOf(ctx, "BTC-60000-C", t0.Add(-time.Hour))
	if !errors.Is(err, ErrNoTerms) {
		t.Fatalf("err = %v, want ErrNoTerms — reading before a contract existed must not return its "+
			"later terms", err)
	}
}

// THE REVERSE DIRECTION: underlying → its chain. This is the query the
// TermsProvider seam cannot express and the reason for the partial index.
func TestChainAsOfReturnsEveryStrikeForTheUnderlying(t *testing.T) {
	st := NewPostgres(newPool(t))
	ctx := context.Background()

	if err := st.Put(ctx, []Record{
		optionRec("BTC-50000-C", "BTC-USD", 50000, t0, 1),
		optionRec("BTC-60000-C", "BTC-USD", 60000, t0, 1),
		optionRec("BTC-70000-C", "BTC-USD", 70000, t0, 1),
		optionRec("ETH-3000-C", "ETH-USD", 3000, t0, 1), // a different underlying
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	chain, err := st.ChainAsOf(ctx, "BTC-USD", t0, KindOption)
	if err != nil {
		t.Fatalf("ChainAsOf: %v", err)
	}
	if len(chain) != 3 {
		t.Fatalf("chain has %d contracts, want 3 — a surface fitted from a partial chain fits "+
			"cleanly and is wrong", len(chain))
	}
	for _, r := range chain {
		if r.UnderlyingID != "BTC-USD" {
			t.Errorf("chain contains %s whose underlying is %q — another underlying's strikes are "+
				"being fitted into this surface", r.InstrumentID, r.UnderlyingID)
		}
	}
}

// AN AMENDED CONTRACT APPEARS ONCE, AT ITS NEWEST TERMS.
//
// Without DISTINCT ON, a contract amended three times contributes three rows and
// the calibrator fits the same strike at three different multipliers.
func TestChainAsOfReturnsOneRowPerContract(t *testing.T) {
	st := NewPostgres(newPool(t))
	ctx := context.Background()
	mid, late := t0.AddDate(0, 1, 0), t0.AddDate(0, 2, 0)

	if err := st.Put(ctx, []Record{
		optionRec("BTC-60000-C", "BTC-USD", 60000, t0, 1),
		optionRec("BTC-60000-C", "BTC-USD", 30000, mid, 2),
		optionRec("BTC-60000-C", "BTC-USD", 15000, late, 4),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	chain, err := st.ChainAsOf(ctx, "BTC-USD", late, KindOption)
	if err != nil {
		t.Fatalf("ChainAsOf: %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("an amended contract appears %d times in the chain, want 1 — the surface would "+
			"carry the same strike at several multipliers", len(chain))
	}
	if got := chain[0].Terms.GetOption().GetStrike().GetCoefficient(); got != 15000 {
		t.Fatalf("chain strike = %d, want 15000 (the newest amendment)", got)
	}
}

// AND THE CHAIN IS POINT-IN-TIME TOO: an amendment after the read instant is
// not visible.
func TestChainAsOfIgnoresAmendmentsAfterTheInstant(t *testing.T) {
	st := NewPostgres(newPool(t))
	ctx := context.Background()
	later := t0.AddDate(0, 1, 0)

	if err := st.Put(ctx, []Record{
		optionRec("BTC-60000-C", "BTC-USD", 60000, t0, 1),
		optionRec("BTC-60000-C", "BTC-USD", 30000, later, 2),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	chain, err := st.ChainAsOf(ctx, "BTC-USD", later.Add(-time.Second), KindOption)
	if err != nil {
		t.Fatalf("ChainAsOf: %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("chain has %d contracts, want 1", len(chain))
	}
	if got := chain[0].Terms.GetOption().GetStrike().GetCoefficient(); got != 60000 {
		t.Fatalf("chain strike = %d, want 60000 — a future amendment leaked into a historical read", got)
	}
}

// RE-PUTTING THE SAME RECORD IS A NO-OP, so a chain reload is idempotent rather
// than a primary-key violation that aborts the whole transaction.
func TestPutIsIdempotent(t *testing.T) {
	st := NewPostgres(newPool(t))
	ctx := context.Background()
	rec := optionRec("BTC-60000-C", "BTC-USD", 60000, t0, 1)

	if err := st.Put(ctx, []Record{rec}); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if err := st.Put(ctx, []Record{rec}); err != nil {
		t.Fatalf("second Put: %v — a chain reload must be idempotent", err)
	}
	chain, err := st.ChainAsOf(ctx, "BTC-USD", t0, KindOption)
	if err != nil {
		t.Fatalf("ChainAsOf: %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("re-put produced %d rows, want 1", len(chain))
	}
}

// A RECORD WITH NO PAYLOAD IS REFUSED rather than stored as an empty blob that
// decodes into a ContractTerms with no terms — which would read as a derivative
// whose specification is silently absent.
func TestPutRefusesARecordWithNoTerms(t *testing.T) {
	st := NewPostgres(newPool(t))

	err := st.Put(context.Background(), []Record{{
		InstrumentID: "BTC-60000-C", AsOf: t0, Kind: KindOption,
	}})
	if err == nil {
		t.Fatal("a record with a nil ContractTerms was stored — it would decode into an empty terms " +
			"message, which reads as a derivative whose specification is absent rather than as an error")
	}
}
