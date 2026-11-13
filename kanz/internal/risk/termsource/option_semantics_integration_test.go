package termsource

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/marketdata/terms"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/pricing"
	"github.com/eighred/kanz/internal/risk/scenario"
	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"
)

func optionSemanticsStore(t *testing.T) *terms.Postgres {
	t.Helper()
	st, pool := newStoreWithPool(t)
	var privileged bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS
		(SELECT 1 FROM pg_roles WHERE pg_has_role(current_user, oid, 'MEMBER') AND (rolsuper OR rolbypassrls))`).Scan(&privileged); err != nil {
		t.Fatal("cannot verify restricted PostgreSQL role and memberships")
	}
	if privileged {
		t.Fatal("option semantics acceptance requires NOSUPERUSER NOBYPASSRLS, including memberships")
	}
	t.Log("actual PostgreSQL role and memberships: NOSUPERUSER NOBYPASSRLS; contract terms are global reference data")
	return st
}

func TestOptionSemanticsPostgresMalformedAndValidControls(t *testing.T) {
	ctx := context.Background()
	st := optionSemanticsStore(t)
	spy := &missingSpy{}
	p := NewProvider(st, WithMissingTermsObserver(spy.observe))
	for n, tc := range invalidOptionSemantics() {
		t.Run(tc.name, func(t *testing.T) {
			id := fmt.Sprintf("INVALID-%d", n)
			rec := optionRec(id, "UNDERLYING", 100, t0, 1)
			tc.mut(rec.Terms.GetOption())
			rec.UnderlyingID = rec.Terms.GetOption().GetUnderlyingId()
			if err := st.Put(ctx, []terms.Record{rec}); err != nil {
				t.Fatalf("cannot persist malformed-field fixture: %v", err)
			}
			spy.seen = nil
			if spec, resolution, err := p.OptionTerms(ctx, id, t0); err != nil || resolution != compute.TermsUnusable || spec != (compute.OptionSpec{}) {
				t.Fatalf("stored malformed fields became usable/linear: %+v resolution=%v error=%v", spec, resolution, err)
			}
			if len(spy.seen) != 1 || spy.seen[0] != id {
				t.Fatalf("malformed terms were not observed: %v", spy.seen)
			}
			assertSemanticRefusalCoverage(t, p, id, t0)
		})
	}
	for _, style := range []referencepb.ExerciseStyle{referencepb.ExerciseStyle_EXERCISE_STYLE_EUROPEAN, referencepb.ExerciseStyle_EXERCISE_STYLE_AMERICAN} {
		for _, typ := range []referencepb.OptionType{referencepb.OptionType_OPTION_TYPE_CALL, referencepb.OptionType_OPTION_TYPE_PUT} {
			t.Run(fmt.Sprintf("valid-%s-%s", style, typ), func(t *testing.T) {
				id := fmt.Sprintf("VALID-%d-%d", style, typ)
				rec := optionRec(id, "UNDERLYING", 100, t0, 1)
				rec.Terms.GetOption().ExerciseStyle, rec.Terms.GetOption().OptionType = style, typ
				if err := st.Put(ctx, []terms.Record{rec}); err != nil {
					t.Fatal(err)
				}
				spy.seen = nil
				if spec, resolution, err := p.OptionTerms(ctx, id, t0); err != nil || resolution != compute.TermsResolved || spec.UnderlyingID != "UNDERLYING" || !spec.Expiry.Equal(t0.AddDate(0, 3, 0)) {
					t.Fatalf("valid stored option did not resolve: %+v resolution=%v error=%v", spec, resolution, err)
				}
				assertSemanticValidPricing(t, p, id)
				if len(spy.seen) != 0 {
					t.Fatalf("valid stored option was reported missing: %v", spy.seen)
				}
			})
		}
	}
	for _, variant := range []struct {
		kind    terms.Kind
		payload *referencepb.ContractTerms
	}{
		{terms.KindSwap, &referencepb.ContractTerms{Terms: &referencepb.ContractTerms_Swap{Swap: &referencepb.SwapTerms{}}}},
		{terms.KindFuture, &referencepb.ContractTerms{Terms: &referencepb.ContractTerms_Future{Future: &referencepb.FutureTerms{}}}},
		{terms.KindBond, &referencepb.ContractTerms{Terms: &referencepb.ContractTerms_Bond{Bond: &referencepb.BondTerms{}}}},
		{terms.KindStructured, &referencepb.ContractTerms{Terms: &referencepb.ContractTerms_Structured{Structured: &referencepb.StructuredTerms{}}}},
	} {
		t.Run("known-other-"+string(variant.kind), func(t *testing.T) {
			id := string(variant.kind)
			variant.payload.InstrumentId, variant.payload.AsOf = id, timestamppb.New(t0)
			if err := st.Put(ctx, []terms.Record{{InstrumentID: id, AsOf: t0, Kind: variant.kind, Terms: variant.payload}}); err != nil {
				t.Fatal(err)
			}
			spy.seen = nil
			if _, resolution, err := p.OptionTerms(ctx, id, t0); err != nil || resolution != compute.TermsOtherVariant {
				t.Fatalf("recognized non-option was refused: %v error=%v", resolution, err)
			}
			book := semanticPortfolio(id, t0)
			pos, _ := book.Position(v1.InstrumentID(id))
			rv := compute.NewRevaluer(compute.GreeksProviders{Terms: p})
			if result := rv.RevalueOption(ctx, id, t0, pos.MarketValue, compute.RevalShocks{}); result.Status != compute.RevalKnownLinear || result.Value != nil {
				t.Fatalf("recognized other variant lost its linear disposition: %+v", result)
			}
			projected, cov := scenario.EvaluateReval(book, []v1.ScenarioShock{v1.ParallelShift{Pct: dec(-2, -1)}}, nil, rv)
			m, _ := projected.Lookup(compute.MeasureGrossExposure)
			if cov.ExcludedCount != 0 || m.Value.GetCoefficient() != 80_000_000 || m.Value.GetExponent() != -6 || len(spy.seen) != 0 {
				t.Fatalf("known non-option lost clean USD80 linear result: coverage=%+v measure=%+v", cov, m)
			}
		})
	}
}

func assertSemanticValidPricing(t *testing.T, provider compute.TermsProvider, id string) {
	t.Helper()
	market := &semanticMarket{}
	providers := compute.GreeksProviders{Terms: provider, Spot: market, Vol: market, Curve: pricing.FlatCurve(0.03)}
	registry := compute.DefaultRegistry()
	compute.RegisterGreeks(context.Background(), registry, providers)
	book := semanticPortfolio(id, t0)
	measures := compute.ComputeMeasures(book, registry, nil)
	for _, name := range []v1.MeasureName{compute.MeasureDelta, compute.MeasureGamma, compute.MeasureVega, compute.MeasureTheta, compute.MeasureRho} {
		m, ok := measures.Lookup(name)
		if !ok || m.Coverage.ExcludedCount != 0 || m.Coverage.Contributed != 1 {
			t.Fatalf("valid stored option failed %s: %+v", name, m)
		}
	}
	pos, _ := book.Position(v1.InstrumentID(id))
	rv := compute.NewRevaluer(providers)
	if result := rv.RevalueOption(context.Background(), id, t0, pos.MarketValue, compute.RevalShocks{PriceFrac: -.2}); result.Status != compute.RevalPriced || result.Value.GetAmount() == nil {
		t.Fatalf("valid stored option was not repriced: %+v", result)
	}
	if _, cov := scenario.EvaluateReval(book, []v1.ScenarioShock{v1.ParallelShift{Pct: dec(-2, -1)}}, nil, rv); cov.ExcludedCount != 0 {
		t.Fatalf("valid stored option scenario was unassessed: %+v", cov)
	}
}

func TestOptionSemanticsPostgresEffectiveTimeRefusalAndRecovery(t *testing.T) {
	ctx := context.Background()
	st := optionSemanticsStore(t)
	badAt, recoveryAt := t0.Add(time.Hour), t0.Add(2*time.Hour)
	good := optionRec("OPTION", "UNDERLYING", 100, t0, 1)
	bad := optionRec("OPTION", "UNDERLYING", 100, badAt, 1)
	bad.Terms.GetOption().ExerciseStyle = referencepb.ExerciseStyle_EXERCISE_STYLE_UNSPECIFIED
	recovered := optionRec("OPTION", "UNDERLYING", 120, recoveryAt, 2)
	recovered.Terms.GetOption().ExerciseStyle = referencepb.ExerciseStyle_EXERCISE_STYLE_AMERICAN
	if err := st.Put(ctx, []terms.Record{good, bad, recovered}); err != nil {
		t.Fatal(err)
	}
	p := NewProvider(st)
	if _, resolution, err := p.OptionTerms(ctx, "OPTION", t0.Add(-time.Second)); err != nil || resolution != compute.TermsUnknown {
		t.Fatal("missing effective version lost its unknown/nil-error distinction")
	}
	for _, at := range []time.Time{t0, badAt.Add(-time.Nanosecond)} {
		if spec, resolution, err := p.OptionTerms(ctx, "OPTION", at); err != nil || resolution != compute.TermsResolved || spec.Exercise != pricing.European || spec.Strike != 100 || spec.Multiplier != 1 {
			t.Fatalf("future malformed amendment leaked into earlier PIT read: %+v resolution=%v error=%v", spec, resolution, err)
		}
	}
	assertSemanticRefusalCoverage(t, p, "OPTION", badAt)
	for _, at := range []time.Time{recoveryAt, recoveryAt.Add(time.Hour)} {
		if spec, resolution, err := p.OptionTerms(ctx, "OPTION", at); err != nil || resolution != compute.TermsResolved || spec.Exercise != pricing.American || spec.Strike != 120 || spec.Multiplier != 2 {
			t.Fatalf("valid effective version did not recover: %+v resolution=%v error=%v", spec, resolution, err)
		}
	}
}
