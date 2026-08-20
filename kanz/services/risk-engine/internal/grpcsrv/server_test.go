package grpcsrv_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/services/risk-engine/internal/grpcsrv"
)

// fakeEngine is a hand-rolled v1.Engine returning the concrete domain types
// the real engine produces, so the adapter's type-asserts + converters run.
type fakeEngine struct {
	exposure func(v1.ExposureRequest) (v1.ExposureResponse, error)
	measures func(v1.MeasuresRequest) (v1.MeasuresResponse, error)
	scenario func(v1.ScenarioRequest) (v1.ScenarioResponse, error)
	health   func() (v1.Health, error)
	list     func() ([]v1.PortfolioSummary, error)
}

func (f fakeEngine) Exposure(_ context.Context, r v1.ExposureRequest) (v1.ExposureResponse, error) {
	return f.exposure(r)
}
func (f fakeEngine) Measures(_ context.Context, r v1.MeasuresRequest) (v1.MeasuresResponse, error) {
	return f.measures(r)
}
func (f fakeEngine) EvaluateScenario(_ context.Context, r v1.ScenarioRequest) (v1.ScenarioResponse, error) {
	return f.scenario(r)
}
func (f fakeEngine) Health(context.Context) (v1.Health, error) { return f.health() }

// A nil list func returns nothing rather than panicking: most cases here are
// about the per-portfolio adapters and have no opinion about listing.
func (f fakeEngine) ListPortfolios(context.Context) ([]v1.PortfolioSummary, error) {
	if f.list == nil {
		return nil, nil
	}
	return f.list()
}

func money(units int64) *commonpb.Money {
	return &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: units, Exponent: 0}, CurrencyCode: "USD"}
}

var asOf = time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

func TestExposureMapsSetAndFlags(t *testing.T) {
	es := domain.NewExposureSet("PF1", asOf, []domain.Exposure{
		{Dimension: domain.ExposureByCurrency, Key: "USD", Gross: money(1000), Net: money(600)},
	})
	srv := grpcsrv.New(fakeEngine{exposure: func(r v1.ExposureRequest) (v1.ExposureResponse, error) {
		if r.PortfolioID != "PF1" {
			t.Errorf("portfolio id not decoded: %q", r.PortfolioID)
		}
		return v1.ExposureResponse{
			PortfolioID: "PF1", AsOf: asOf, Set: es,
			QualityFlags: []v1.QualityFlag{v1.QualityFlagDegraded},
		}, nil
	}}, "acme")

	resp, err := srv.Exposure(context.Background(), &querypb.ExposureRequest{PortfolioId: "PF1"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetPortfolioId() != "PF1" || !resp.GetAsOf().AsTime().Equal(asOf) {
		t.Errorf("response id/as_of wrong: %v %v", resp.GetPortfolioId(), resp.GetAsOf().AsTime())
	}
	if got := resp.GetSet().GetExposures(); len(got) != 1 || got[0].GetBucket() != "USD" ||
		got[0].GetDimension() != domainpb.ExposureDimension_EXPOSURE_DIMENSION_CURRENCY {
		t.Errorf("exposure set not converted: %+v", got)
	}
	if fl := resp.GetQualityFlags(); len(fl) != 1 || fl[0] != querypb.QualityFlag_QUALITY_FLAG_DEGRADED {
		t.Errorf("flags = %v", fl)
	}
	// WIRE-02a: the owning tenant is stamped as the authz-gate input.
	if resp.GetOwnerTenant() != "acme" {
		t.Errorf("owner_tenant = %q, want acme", resp.GetOwnerTenant())
	}
}

// WIRE-03: the engine's SourcePosition is stamped onto the proto
// source_position — the verifiable citation seed — and a nil position stays
// absent rather than becoming a zero coordinate.
func TestExposureStampsSourcePosition(t *testing.T) {
	es := domain.NewExposureSet("PF1", asOf, nil)
	lp := &commonpb.LogPosition{Topic: "risk.state", Partition: 2, Offset: 777}
	srv := grpcsrv.New(fakeEngine{exposure: func(v1.ExposureRequest) (v1.ExposureResponse, error) {
		return v1.ExposureResponse{PortfolioID: "PF1", AsOf: asOf, Set: es, SourcePosition: lp}, nil
	}}, "acme")

	resp, err := srv.Exposure(context.Background(), &querypb.ExposureRequest{PortfolioId: "PF1"})
	if err != nil {
		t.Fatal(err)
	}
	got := resp.GetSourcePosition()
	if got == nil || got.GetTopic() != "risk.state" || got.GetPartition() != 2 || got.GetOffset() != 777 {
		t.Fatalf("source_position = %+v, want risk.state/2/777", got)
	}
}

func TestExposureNilSourcePositionStaysAbsent(t *testing.T) {
	es := domain.NewExposureSet("PF1", asOf, nil)
	srv := grpcsrv.New(fakeEngine{exposure: func(v1.ExposureRequest) (v1.ExposureResponse, error) {
		return v1.ExposureResponse{PortfolioID: "PF1", AsOf: asOf, Set: es}, nil // no position
	}}, "acme")

	resp, err := srv.Exposure(context.Background(), &querypb.ExposureRequest{PortfolioId: "PF1"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetSourcePosition() != nil {
		t.Fatalf("source_position = %+v, want nil", resp.GetSourcePosition())
	}
}

func TestMeasuresStampsSourcePosition(t *testing.T) {
	ms := domain.NewMeasureSet("PF1", asOf, map[v1.MeasureName]v1.Measure{})
	lp := &commonpb.LogPosition{Topic: "risk.state", Partition: 0, Offset: 12}
	srv := grpcsrv.New(fakeEngine{measures: func(v1.MeasuresRequest) (v1.MeasuresResponse, error) {
		return v1.MeasuresResponse{PortfolioID: "PF1", AsOf: asOf, Set: ms, SourcePosition: lp}, nil
	}}, "acme")

	resp, err := srv.Measures(context.Background(), &querypb.MeasuresRequest{PortfolioId: "PF1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.GetSourcePosition(); got == nil || got.GetOffset() != 12 {
		t.Fatalf("source_position = %+v, want offset 12", got)
	}
}

func TestMeasuresPassesFilterAndConverts(t *testing.T) {
	ms := domain.NewMeasureSet("PF1", asOf, map[v1.MeasureName]v1.Measure{
		"VaR99": {Name: "VaR99", Value: &commonpb.Decimal{Coefficient: 42, Exponent: 0}},
	})
	srv := grpcsrv.New(fakeEngine{measures: func(r v1.MeasuresRequest) (v1.MeasuresResponse, error) {
		if len(r.Measures) != 1 || r.Measures[0] != "VaR99" {
			t.Errorf("measure filter not forwarded: %v", r.Measures)
		}
		return v1.MeasuresResponse{PortfolioID: "PF1", AsOf: asOf, Set: ms}, nil
	}}, "acme")

	resp, err := srv.Measures(context.Background(), &querypb.MeasuresRequest{
		PortfolioId: "PF1", Measures: []string{"VaR99"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.GetSet().GetMeasures(); len(got) != 1 || got[0].GetName() != "VaR99" {
		t.Errorf("measure set not converted: %+v", got)
	}
}

// THE CLIENT THE ISSUE IS ABOUT IS THIS ONE (#509).
//
// #509 opened on measures that reach no client at all. The families since wired
// reach it and answer zero: the contract-terms store has no production writer,
// so every DV01 served today is computed over no bond. Whether the caller can
// TELL is this converter's job — protoFlags carries one set-level bit, and
// RiskMeasure.coverage is what names the measure, the magnitude, and a sample.
//
// Both measures below are in one response on purpose. That is the case a
// set-level flag cannot serve: refusing the whole response over DV01 discards a
// GrossExposure that was never in doubt.
func TestMeasuresCarryTheirCoverageToTheCaller(t *testing.T) {
	ms := domain.NewMeasureSet("PF1", asOf, map[v1.MeasureName]v1.Measure{
		"DV01": {
			Name:  "DV01",
			Value: &commonpb.Decimal{Coefficient: 0},
			Coverage: v1.InputCoverage{
				ExcludedCount: 2,
				Exclusions:    []v1.InputExclusion{{InstrumentID: "GOVT-10Y", Reason: "no_terms"}},
			},
		},
		"GrossExposure": {Name: "GrossExposure", Value: &commonpb.Decimal{Coefficient: 1000}},
	})
	srv := grpcsrv.New(fakeEngine{measures: func(v1.MeasuresRequest) (v1.MeasuresResponse, error) {
		return v1.MeasuresResponse{PortfolioID: "PF1", AsOf: asOf, Set: ms}, nil
	}}, "acme")

	resp, err := srv.Measures(context.Background(), &querypb.MeasuresRequest{PortfolioId: "PF1"})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*domainpb.RiskMeasure{}
	for _, m := range resp.GetSet().GetMeasures() {
		byName[m.GetName()] = m
	}

	cov := byName["DV01"].GetCoverage()
	if cov == nil {
		t.Fatalf("DV01.coverage absent — the caller receives a zero DV01 with no way to tell it " +
			"from a book holding no bonds, which is the whole of #527 undone at the process " +
			"boundary")
	}
	if cov.GetExcludedCount() != 2 || len(cov.GetExclusions()) != 1 ||
		cov.GetExclusions()[0].GetInstrumentId() != "GOVT-10Y" {
		t.Errorf("DV01.coverage=%+v, want excluded_count 2 with the GOVT-10Y sample", cov)
	}

	if cov := byName["GrossExposure"].GetCoverage(); cov != nil {
		t.Errorf("GrossExposure.coverage=%+v, want absent — it reads the portfolio directly and "+
			"has no provider that could decline, so reporting a clean coverage record would be a "+
			"claim the engine never made", cov)
	}
}

func TestEvaluateScenarioDecodesShocks(t *testing.T) {
	ms := domain.NewMeasureSet("PF1", asOf, map[v1.MeasureName]v1.Measure{})
	var gotShocks []v1.ScenarioShock
	srv := grpcsrv.New(fakeEngine{scenario: func(r v1.ScenarioRequest) (v1.ScenarioResponse, error) {
		gotShocks = r.Shocks
		return v1.ScenarioResponse{PortfolioID: "PF1", Projected: ms}, nil
	}}, "acme")

	_, err := srv.EvaluateScenario(context.Background(), &querypb.EvaluateScenarioRequest{
		PortfolioId: "PF1",
		Shocks: []*querypb.ScenarioShock{
			{Shock: &querypb.ScenarioShock_Price{Price: &querypb.PriceShock{
				InstrumentId: "AAPL", Pct: &commonpb.Decimal{Coefficient: -10, Exponent: -2}}}},
			{Shock: &querypb.ScenarioShock_ParallelShift{ParallelShift: &querypb.ParallelShift{
				Pct: &commonpb.Decimal{Coefficient: -5, Exponent: -2}}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(gotShocks) != 2 {
		t.Fatalf("want 2 shocks decoded, got %d", len(gotShocks))
	}
	if ps, ok := gotShocks[0].(v1.PriceShock); !ok || ps.InstrumentID != "AAPL" {
		t.Errorf("first shock not a PriceShock(AAPL): %#v", gotShocks[0])
	}
	if _, ok := gotShocks[1].(v1.ParallelShift); !ok {
		t.Errorf("second shock not a ParallelShift: %#v", gotShocks[1])
	}
}

func TestScenarioShockWithNoKindIsInvalidArgument(t *testing.T) {
	srv := grpcsrv.New(fakeEngine{}, "acme")
	_, err := srv.EvaluateScenario(context.Background(), &querypb.EvaluateScenarioRequest{
		PortfolioId: "PF1",
		Shocks:      []*querypb.ScenarioShock{{}}, // empty oneof
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("want InvalidArgument, got %v", err)
	}
}

// TestScenarioShockOutOfDomainIsInvalidArgument is #246.
//
// ScenarioShock.Pct was the one Decimal on this platform that reached arithmetic
// with no domain check anywhere in front of it: every BUS ingress calls
// dec.InDomainDeep, and this is a gRPC method argument, which the #95 arch guard
// does not look at because it keys on proto.Unmarshal(payload, …).
//
// exponent -2000000000 drove an unbounded pow10 loop two billion times per
// position per shock. The assertion is therefore BOTH halves — the right code AND
// promptly, because a correct-but-eventual InvalidArgument is still the outage.
// The engine must not be reached at all: a request refused after the compute
// started is a request that already spent the CPU.
func TestScenarioShockOutOfDomainIsInvalidArgument(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shock *querypb.ScenarioShock
	}{
		{"parallel shift", &querypb.ScenarioShock{Shock: &querypb.ScenarioShock_ParallelShift{
			ParallelShift: &querypb.ParallelShift{Pct: &commonpb.Decimal{Coefficient: 1, Exponent: -2_000_000_000}}}}},
		{"price shock", &querypb.ScenarioShock{Shock: &querypb.ScenarioShock_Price{
			Price: &querypb.PriceShock{InstrumentId: "AAPL", Pct: &commonpb.Decimal{Coefficient: 1, Exponent: 2_000_000_000}}}}},
		{"just outside the bound", &querypb.ScenarioShock{Shock: &querypb.ScenarioShock_ParallelShift{
			ParallelShift: &querypb.ParallelShift{Pct: &commonpb.Decimal{Coefficient: 1, Exponent: -65}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reached := false
			srv := grpcsrv.New(fakeEngine{scenario: func(v1.ScenarioRequest) (v1.ScenarioResponse, error) {
				reached = true
				return v1.ScenarioResponse{}, nil
			}}, "acme")

			done := make(chan error, 1)
			go func() {
				_, err := srv.EvaluateScenario(context.Background(), &querypb.EvaluateScenarioRequest{
					PortfolioId: "PF1",
					Shocks:      []*querypb.ScenarioShock{tc.shock},
				})
				done <- err
			}()
			select {
			case err := <-done:
				if status.Code(err) != codes.InvalidArgument {
					t.Fatalf("want InvalidArgument for an out-of-domain shock exponent, got %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("EvaluateScenario did not return within 5s — the domain check is gone and one " +
					"crafted shock stalls the risk engine while it still reports healthy")
			}
			if reached {
				t.Error("the Engine was called with an out-of-domain shock — the refusal must precede the compute")
			}
		})
	}
}

// TestInDomainShockStillEvaluates is the non-vacuity half: the bound must not
// refuse a real shock. -65 above and -8 here bracket it — anything a mark or a
// stress actually carries sits inside |exponent| ≤ 64 by thirty orders of
// magnitude.
func TestInDomainShockStillEvaluates(t *testing.T) {
	ms := domain.NewMeasureSet("PF1", asOf, map[v1.MeasureName]v1.Measure{})
	srv := grpcsrv.New(fakeEngine{scenario: func(v1.ScenarioRequest) (v1.ScenarioResponse, error) {
		return v1.ScenarioResponse{PortfolioID: "PF1", Projected: ms}, nil
	}}, "acme")
	_, err := srv.EvaluateScenario(context.Background(), &querypb.EvaluateScenarioRequest{
		PortfolioId: "PF1",
		Shocks: []*querypb.ScenarioShock{{Shock: &querypb.ScenarioShock_ParallelShift{
			ParallelShift: &querypb.ParallelShift{Pct: &commonpb.Decimal{Coefficient: -20_000_000, Exponent: -8}}}}},
	})
	if err != nil {
		t.Fatalf("an ordinary -20%% shock at exponent -8 was refused: %v", err)
	}
}

func TestHealthMapsMode(t *testing.T) {
	srv := grpcsrv.New(fakeEngine{health: func() (v1.Health, error) {
		return v1.Health{Mode: v1.ModeDegraded, AsOf: asOf, Staleness: 90 * time.Second}, nil
	}}, "acme")
	resp, err := srv.Health(context.Background(), &querypb.HealthRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetMode() != querypb.Mode_MODE_DEGRADED {
		t.Errorf("mode = %v", resp.GetMode())
	}
	if resp.GetStaleness().AsDuration() != 90*time.Second {
		t.Errorf("staleness = %v", resp.GetStaleness().AsDuration())
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"not_found", v1.ErrPortfolioNotFound, codes.NotFound},
		{"invalid", v1.ErrInvalidRequest, codes.InvalidArgument},
		{"other", context.DeadlineExceeded, codes.DeadlineExceeded},
		// #110: a shard refusal must NOT arrive as NotFound. One Service fans
		// out across every replica, so "no such portfolio" would be the answer
		// from every pod that does not own it — false, and non-deterministic
		// between identical calls.
		{"not_owned", v1.ErrPortfolioNotOwned, codes.FailedPrecondition},
		{"not_owned_wrapped", fmt.Errorf("%w: PF1 is held by replica risk-engine-2", v1.ErrPortfolioNotOwned), codes.FailedPrecondition},
		// #640: a scenario whose sector shocks cannot resolve. NOT Internal —
		// the default arm would bury "no instrument classifier is wired" under
		// the code reserved for bugs, where a caller learns to retry it. The
		// engine always wraps this sentinel with the reason and a bounded
		// sample, so the wrapped form is the only one that ships.
		{"scenario_unresolvable", v1.ErrScenarioUnresolvable, codes.FailedPrecondition},
		{"scenario_unresolvable_wrapped", fmt.Errorf("%w (0 of 1 resolved; reason(s): no_classifier)", v1.ErrScenarioUnresolvable), codes.FailedPrecondition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := grpcsrv.New(fakeEngine{exposure: func(v1.ExposureRequest) (v1.ExposureResponse, error) {
				return v1.ExposureResponse{}, tc.err
			}}, "acme")
			_, err := srv.Exposure(context.Background(), &querypb.ExposureRequest{PortfolioId: "PF1"})
			if status.Code(err) != tc.want {
				t.Errorf("err %v mapped to %v, want %v", tc.err, status.Code(err), tc.want)
			}
		})
	}
}

// TestExposureAsOfRoundTrip: a request as_of is decoded to the engine and a
// zero response as_of comes back as a nil timestamp (absent), not the epoch.
func TestExposureAsOfDecodeAndZeroElision(t *testing.T) {
	es := domain.NewExposureSet("PF1", time.Time{}, nil)
	var gotAsOf time.Time
	srv := grpcsrv.New(fakeEngine{exposure: func(r v1.ExposureRequest) (v1.ExposureResponse, error) {
		gotAsOf = r.AsOf
		return v1.ExposureResponse{PortfolioID: "PF1", Set: es}, nil // zero AsOf
	}}, "acme")
	req := &querypb.ExposureRequest{PortfolioId: "PF1", AsOf: timestamppb.New(asOf)}
	resp, err := srv.Exposure(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !gotAsOf.Equal(asOf) {
		t.Errorf("request as_of not decoded: %v", gotAsOf)
	}
	if resp.GetAsOf() != nil {
		t.Errorf("zero response as_of should elide to nil, got %v", resp.GetAsOf())
	}
}
