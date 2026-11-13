package termsource

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/marketdata/terms"
	"github.com/eighred/kanz/internal/risk/compute"
	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"
)

func bondTimestampStore(t *testing.T) *terms.Postgres {
	t.Helper()
	st, pool := newStoreWithPool(t)
	var privileged bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS
		(SELECT 1 FROM pg_roles WHERE pg_has_role(current_user, oid, 'MEMBER') AND (rolsuper OR rolbypassrls))`).Scan(&privileged); err != nil {
		t.Fatal("cannot verify restricted PostgreSQL role and memberships")
	}
	if privileged {
		t.Fatal("bond timestamp acceptance requires NOSUPERUSER NOBYPASSRLS, including memberships")
	}
	t.Log("actual PostgreSQL role and memberships: NOSUPERUSER NOBYPASSRLS; contract terms are global reference data")
	return st
}

func bondTimeRecord(id string, at time.Time, bond *referencepb.BondTerms) terms.Record {
	return terms.Record{InstrumentID: id, AsOf: at, Kind: terms.KindBond, Terms: &referencepb.ContractTerms{
		InstrumentId: id, AsOf: timestamppb.New(at), Terms: &referencepb.ContractTerms_Bond{Bond: bond},
	}}
}

func TestBondTimestampPostgresRefusalRecoveryAndPIT(t *testing.T) {
	st := bondTimestampStore(t)
	ctx := context.Background()
	spy := &missingSpy{}
	p := NewProvider(st, WithMissingTermsObserver(spy.observe))
	at, recoveredAt := bondIssue, bondIssue.Add(time.Hour)
	for n, tc := range malformedBondTimes() {
		t.Run(tc.name, func(t *testing.T) {
			id := fmt.Sprintf("BOND-TIME-%d", n)
			valid := bondTimeZero(recoveredAt)
			// Record keys remain exact microseconds. The fixture deliberately
			// retains malformed nested dates; the provider must refuse them.
			if err := st.Put(ctx, []terms.Record{bondTimeRecord(id, at, tc.bond), bondTimeRecord(id, recoveredAt, valid)}); err != nil {
				t.Fatal(err)
			}
			// AsOf is this store's effective-time selector. This does not claim
			// a separate ingested_at/knowledge-time predicate or rewrite history.
			for _, step := range []struct {
				at   time.Time
				want compute.TermsResolution
			}{
				{at.Add(-time.Nanosecond), compute.TermsUnknown},
				{at, compute.TermsUnusable},
				{recoveredAt.Add(-time.Nanosecond), compute.TermsUnusable},
				{recoveredAt, compute.TermsResolved},
				{recoveredAt.Add(time.Nanosecond), compute.TermsResolved},
			} {
				spy.seen = nil
				spec, status := p.BondTerms(ctx, id, step.at)
				if status != step.want {
					t.Fatalf("at %v: resolution=%v want %v", step.at, status, step.want)
				}
				if status == compute.TermsResolved {
					if spec.CouponRate != 0 || spec.Frequency != 0 || !spec.Issue.Equal(recoveredAt) || !spec.Maturity.Equal(recoveredAt.AddDate(1, 0, 0)) || len(spy.seen) != 0 {
						t.Fatalf("valid PIT recovery changed: %+v observed=%v", spec, spy.seen)
					}
				} else if len(spy.seen) != 1 || spy.seen[0] != id || spec.CanonicalTerms != nil {
					t.Fatalf("unknown/unusable stored terms were not observed/refused: %+v observed=%v", spec, spy.seen)
				}
			}
			old, err := st.LatestAsOf(ctx, id, at)
			if err != nil || !proto.Equal(old.Terms.GetBond(), tc.bond) {
				t.Fatal("reader validation rewrote the immutable malformed historical record")
			}
		})
	}
}

func TestBondTimestampPostgresFICoverageAndZeroCouponOracle(t *testing.T) {
	st := bondTimestampStore(t)
	ctx := context.Background()
	spy := &missingSpy{}
	p := NewProvider(st, WithMissingTermsObserver(spy.observe))
	for n, tc := range malformedBondTimes() {
		t.Run(tc.name, func(t *testing.T) {
			id := fmt.Sprintf("BOND-FI-TIME-%d", n)
			if err := st.Put(ctx, []terms.Record{bondTimeRecord(id, bondIssue, tc.bond)}); err != nil {
				t.Fatal(err)
			}
			spy.seen = nil
			assertBondTimestampFIRefusal(t, p, id, bondIssue)
			// Four independently registered measures and the existing bond
			// revaluer each resolve the same unusable stored record.
			if len(spy.seen) != 5 {
				t.Fatalf("missing observer calls: %v", spy.seen)
			}
			for _, observed := range spy.seen {
				if observed != id {
					t.Fatal("refusal observer lost instrument attribution")
				}
			}
		})
	}
	t.Run("valid zero coupon", func(t *testing.T) {
		if err := st.Put(ctx, []terms.Record{bondTimeRecord("ZERO-TIME", bondIssue, bondTimeZero(bondIssue))}); err != nil {
			t.Fatal(err)
		}
		spy.seen = nil
		assertBondTimestampZeroOracle(t, p, "ZERO-TIME", bondIssue)
		if len(spy.seen) != 0 {
			t.Fatalf("valid zero coupon was reported unusable: %v", spy.seen)
		}
	})
}
