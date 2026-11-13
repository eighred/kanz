package termsource

import (
	"context"
	"testing"

	"github.com/eighred/kanz/internal/marketdata/terms"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/scenario"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"
)

func TestOptionRecordRequiresARecognizedOtherVariant(t *testing.T) {
	for _, rec := range []terms.Record{
		{},
		{Kind: terms.KindFuture, Terms: &referencepb.ContractTerms{}},
		{Kind: terms.KindFuture, Terms: &referencepb.ContractTerms{Terms: &referencepb.ContractTerms_Future{}}},
		{Kind: terms.KindOption, Terms: &referencepb.ContractTerms{Terms: &referencepb.ContractTerms_Future{Future: &referencepb.FutureTerms{}}}},
	} {
		if _, resolution, err := resolveOptionRecord(rec); err != nil || resolution != compute.TermsUnknown {
			t.Fatalf("unknown/mismatched variant certified linear: %v", resolution)
		}
	}
	known := terms.Record{Kind: terms.KindFuture, Terms: &referencepb.ContractTerms{Terms: &referencepb.ContractTerms_Future{Future: &referencepb.FutureTerms{}}}}
	if _, resolution, err := resolveOptionRecord(known); err != nil || resolution != compute.TermsOtherVariant {
		t.Fatalf("known future refused: %v", resolution)
	}
}

func TestOptionTermsReadFailureAfterReadinessIsUnassessed(t *testing.T) {
	ctx := context.Background()
	store, pool := newStoreWithPool(t)
	var superuser, bypass bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&superuser, &bypass); err != nil {
		t.Fatal("cannot verify restricted test role")
	}
	if superuser || bypass {
		t.Fatal("terms acceptance requires NOSUPERUSER NOBYPASSRLS")
	}
	t.Log("actual terms PostgreSQL role: NOSUPERUSER NOBYPASSRLS")
	if err := store.Put(ctx, []terms.Record{optionRec("OPTION", "UND", 100, t0, 1), optionRec("INVALID", "UND", 0, t0, 1)}); err != nil {
		t.Fatal("cannot store option fixtures")
	}
	provider := NewProvider(store)
	if err := store.Ping(ctx); err != nil {
		t.Fatal("fixture was not ready before read failure")
	}
	if _, resolution, err := provider.OptionTerms(ctx, "OPTION", t0); err != nil || resolution != compute.TermsResolved {
		t.Fatal("valid option fixture did not resolve")
	}
	assertProviderUnassessed(t, provider, "MISSING", compute.SkipNoTerms)
	assertProviderUnassessed(t, provider, "INVALID", compute.SkipInvalidOptionTerms)
	if _, err := pool.Exec(ctx, `ALTER TABLE contract_terms RENAME TO temporarily_unavailable_terms`); err != nil {
		t.Fatal("cannot inject private-schema read failure")
	}
	restored := false
	t.Cleanup(func() {
		if !restored {
			if _, err := pool.Exec(context.Background(), `ALTER TABLE temporarily_unavailable_terms RENAME TO contract_terms`); err != nil {
				t.Error("cannot restore private terms table")
			}
		}
	})
	// Connection readiness still succeeds: it cannot certify this later query.
	if err := store.Ping(ctx); err != nil {
		t.Fatal("fixture lost readiness rather than only the terms query")
	}
	if _, resolution, err := provider.OptionTerms(ctx, "OPTION", t0); err == nil || resolution != compute.TermsUnknown {
		t.Fatal("post-readiness error certified an instrument classification")
	}
	assertProviderUnassessed(t, provider, "OPTION", compute.SkipTermsReadFailed)
	if _, err := pool.Exec(ctx, `ALTER TABLE temporarily_unavailable_terms RENAME TO contract_terms`); err != nil {
		t.Fatal("cannot recover private terms query")
	}
	restored = true
	if spec, resolution, err := provider.OptionTerms(ctx, "OPTION", t0); err != nil || resolution != compute.TermsResolved || spec.Strike != 100 {
		t.Fatal("same-horizon option did not recover after the query failure")
	}
}

func assertProviderUnassessed(t *testing.T, provider *Provider, id, reason string) {
	t.Helper()
	p := domain.NewPortfolio("terms-resolution", "USD")
	mv := &commonpb.Money{Amount: dec(100, 0), CurrencyCode: "USD"}
	p.SetPosition(domain.Position{InstrumentID: domain.InstrumentID(id), AsOf: t0, Quantity: dec(1, 0), MarketValue: mv})
	registry := compute.DefaultRegistry()
	compute.RegisterGreeks(context.Background(), registry, compute.GreeksProviders{Terms: provider})
	m, _ := compute.ComputeMeasures(p, registry, nil).Lookup(compute.MeasureDelta)
	if m.Coverage.ExcludedCount != 1 || m.Coverage.Contributed != 0 || len(m.Coverage.Exclusions) != 1 || m.Coverage.Exclusions[0].Reason != reason {
		t.Fatalf("stored terms failure looked assessed: %+v", m)
	}
	_, cov := scenario.EvaluateReval(p, []v1.ScenarioShock{v1.ParallelShift{Pct: dec(-2, -1)}}, nil, compute.NewRevaluer(compute.GreeksProviders{Terms: provider}))
	if cov.ExcludedCount != 1 || len(cov.Exclusions) != 1 || cov.Exclusions[0].Reason != reason {
		t.Fatalf("stored terms failure produced clean scenario: %+v", cov)
	}
}
