package terms

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"
)

func termsTimePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newPool(t)
	var privileged bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS
		(SELECT 1 FROM pg_roles WHERE pg_has_role(current_user, oid, 'MEMBER') AND (rolsuper OR rolbypassrls))`).Scan(&privileged); err != nil {
		t.Fatal("cannot verify restricted PostgreSQL role and memberships")
	}
	if privileged {
		t.Fatal("terms time acceptance requires NOSUPERUSER NOBYPASSRLS, including memberships")
	}
	t.Log("actual PostgreSQL role and memberships: NOSUPERUSER NOBYPASSRLS; terms are global reference facts")
	return pool
}

func TestTermsPostgresRejectsPrecisionLossAtomically(t *testing.T) {
	ctx := context.Background()
	pool := termsTimePool(t)
	st := NewPostgres(pool)
	seed := optionRec("EXISTING", "UNDERLYING", 100, t0, 1)
	if err := st.Put(ctx, []Record{seed}); err != nil {
		t.Fatal(err)
	}
	seedBlob, err := proto.Marshal(seed.Terms)
	if err != nil {
		t.Fatal(err)
	}
	for n, at := range []time.Time{t0.Add(time.Nanosecond), t0.Add(999 * time.Nanosecond), t0.Add(1001 * time.Nanosecond), time.Unix(-1, 999_999_999), t0.In(time.FixedZone("offset", 3*3600)).Add(time.Nanosecond)} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			pending := optionRec(fmt.Sprintf("PENDING-%d", n), "UNDERLYING", 100, t0, 1)
			bad := optionRec(fmt.Sprintf("BAD-%d", n), "UNDERLYING", 200, at, 1)
			before := proto.Clone(bad.Terms)
			if err := st.Put(ctx, []Record{pending, bad}); err == nil {
				t.Fatal("sub-microsecond row was accepted")
			}
			var count int
			var stored []byte
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM contract_terms`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT terms FROM contract_terms WHERE instrument_id='EXISTING'`).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if count != 1 || !bytes.Equal(stored, seedBlob) || !proto.Equal(before, bad.Terms) {
				t.Fatal("failed append wrote a partial batch or normalized history")
			}
		})
	}
}

func TestTermsPostgresMicrosecondHorizonBoundary(t *testing.T) {
	ctx := context.Background()
	pool := termsTimePool(t)
	st := NewPostgres(pool)
	for n, tc := range []struct {
		name, input, utc string
	}{
		{"positive epoch", "2026-03-01T00:00:00.000001Z", "2026-03-01T00:00:00.000001Z"},
		{"Unix epoch", "1970-01-01T00:00:00Z", "1970-01-01T00:00:00Z"},
		{"negative epoch", "1969-12-31T23:59:59.999999Z", "1969-12-31T23:59:59.999999Z"},
		{"before pgx epoch", "1999-12-31T23:59:59.999999Z", "1999-12-31T23:59:59.999999Z"},
		{"offset instant", "2026-03-01T00:00:00.000001+03:00", "2026-02-28T21:00:00.000001Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339Nano, tc.input)
			if err != nil {
				t.Fatal(err)
			}
			id, underlying := fmt.Sprintf("OPTION-%d", n), fmt.Sprintf("UNDERLYING-%d", n)
			old := optionRec(id, underlying, 100, at.Add(-time.Microsecond), 1)
			newer := optionRec(id, underlying, 200, at, 2)
			for range 2 {
				if err := st.Put(ctx, []Record{old, newer}); err != nil {
					t.Fatal(err)
				}
			}
			// PostgreSQL parses this independent UTC text oracle. It does not
			// reuse pgx's time.Time parameter encoder to establish the key.
			var exact bool
			if err := pool.QueryRow(ctx, `SELECT as_of = $2::text::timestamptz FROM contract_terms
				WHERE instrument_id=$1 ORDER BY as_of DESC LIMIT 1`, id, tc.utc).Scan(&exact); err != nil || !exact {
				t.Fatalf("SQL key differs from independent UTC instant: exact=%v error=%v", exact, err)
			}
			for _, horizon := range []struct {
				at   time.Time
				want Record
			}{
				{at.Add(-time.Nanosecond), old},
				{at, newer},
				{at.Add(time.Nanosecond), newer},
			} {
				got, err := st.LatestAsOf(ctx, id, horizon.at)
				if err != nil || !got.AsOf.Equal(horizon.want.AsOf) || !proto.Equal(got.Terms, horizon.want.Terms) {
					t.Fatalf("LatestAsOf %v leaked or normalized a version: %+v error=%v", horizon.at, got, err)
				}
				chain, err := st.ChainAsOf(ctx, underlying, horizon.at, KindOption)
				if err != nil || len(chain) != 1 || !proto.Equal(chain[0].Terms, horizon.want.Terms) {
					t.Fatalf("ChainAsOf %v leaked or duplicated a version: %+v error=%v", horizon.at, chain, err)
				}
			}
			before := old.AsOf.Add(-time.Nanosecond)
			if _, err := st.LatestAsOf(ctx, id, before); !errors.Is(err, ErrNoTerms) {
				t.Fatalf("before first version: want ErrNoTerms, got %v", err)
			}
			if chain, err := st.ChainAsOf(ctx, underlying, before, KindOption); err != nil || len(chain) != 0 {
				t.Fatalf("before first version: want empty chain, got %+v error=%v", chain, err)
			}
			var count int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM contract_terms WHERE instrument_id=$1`, id).Scan(&count); err != nil || count != 2 {
				t.Fatalf("replay rewrote/duplicated history: count=%d error=%v", count, err)
			}
		})
	}
}

func TestTermsPostgresLegacyReadRefusalDoesNotRewrite(t *testing.T) {
	ctx := context.Background()
	pool := termsTimePool(t)
	st := NewPostgres(pool)
	for n, tc := range inconsistentStoredTerms() {
		t.Run(tc.name, func(t *testing.T) {
			underlying := fmt.Sprintf("UNDERLYING-%d", n)
			rec := optionRec(fmt.Sprintf("B-LEGACY-%d", n), underlying, 100, t0, 1)
			older := optionRec(rec.InstrumentID, underlying, 50, t0.Add(-time.Microsecond), 1)
			tc.mut(&rec)
			control := optionRec(fmt.Sprintf("A-CONTROL-%d", n), underlying, 100, t0, 1)
			if rec.Kind == KindFuture {
				control.Kind, control.Terms.Terms = KindFuture, &referencepb.ContractTerms_Future{Future: &referencepb.FutureTerms{UnderlyingId: underlying}}
			} else if rec.Kind == KindStructured {
				control.Kind, control.Terms.Terms = KindStructured, &referencepb.ContractTerms_Structured{Structured: &referencepb.StructuredTerms{DealId: underlying}}
			}
			if err := st.Put(ctx, []Record{older, control}); err != nil {
				t.Fatal(err)
			}
			blob, err := proto.Marshal(rec.Terms)
			if err != nil {
				t.Fatal(err)
			}
			// Model a legacy row inside this fixture's private schema, bypassing
			// append validation without touching or repairing existing history.
			if _, err := pool.Exec(ctx, `INSERT INTO contract_terms (instrument_id, as_of, underlying_id, kind, terms)
				VALUES ($1,$2,$3,$4,$5)`, rec.InstrumentID, rec.AsOf, rec.UnderlyingID, string(rec.Kind), blob); err != nil {
				t.Fatal(err)
			}
			if got, err := st.LatestAsOf(ctx, rec.InstrumentID, t0); err == nil || errors.Is(err, ErrNoTerms) || got.Terms != nil {
				t.Fatalf("legacy row was accepted, skipped for older terms, or reported missing: %+v error=%v", got, err)
			}
			if chain, err := st.ChainAsOf(ctx, underlying, t0, rec.Kind); err == nil || chain != nil {
				t.Fatalf("legacy row yielded a complete/partial chain: %+v error=%v", chain, err)
			}
			var stored []byte
			var key time.Time
			if err := pool.QueryRow(ctx, `SELECT as_of, terms FROM contract_terms WHERE instrument_id=$1 AND as_of=$2`, rec.InstrumentID, t0).Scan(&key, &stored); err != nil || !key.Equal(rec.AsOf) || !bytes.Equal(stored, blob) {
				t.Fatalf("read refusal rewrote legacy evidence: key=%v error=%v", key, err)
			}
			if got, err := st.LatestAsOf(ctx, control.InstrumentID, t0); err != nil || !proto.Equal(got.Terms, control.Terms) {
				t.Fatalf("legacy refusal damaged the valid control: %+v error=%v", got, err)
			}
		})
	}
}

func TestTermsPostgresRejectsIndexedMismatchAtomically(t *testing.T) {
	ctx := context.Background()
	pool := termsTimePool(t)
	st := NewPostgres(pool)
	for _, tc := range indexedGroupingMismatches() {
		t.Run(tc.name, func(t *testing.T) {
			bad := optionRec("BAD", "UNDERLYING", 100, t0, 1)
			tc.mut(&bad)
			pending := optionRec("PENDING", "UNDERLYING", 100, t0, 1)
			if err := st.Put(ctx, []Record{pending, bad}); err == nil {
				t.Fatal("indexed/payload grouping mismatch accepted")
			}
			var count int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM contract_terms`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("rejected grouping batch was partly appended: count=%d error=%v", count, err)
			}
		})
	}
}
