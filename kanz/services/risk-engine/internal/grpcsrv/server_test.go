package grpcsrv_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	domainpb "github.com/kanz-eng/kanz-schemas-go/domain/v1"
	querypb "github.com/kanz-eng/kanz-schemas-go/query/v1"

	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/domain"
	"github.com/kanz-eng/kanz/services/risk-engine/internal/grpcsrv"
)

// fakeEngine is a hand-rolled v1.Engine returning the concrete domain types
// the real engine produces, so the adapter's type-asserts + converters run.
type fakeEngine struct {
	exposure func(v1.ExposureRequest) (v1.ExposureResponse, error)
	measures func(v1.MeasuresRequest) (v1.MeasuresResponse, error)
	scenario func(v1.ScenarioRequest) (v1.ScenarioResponse, error)
	health   func() (v1.Health, error)
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
	}})

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
	}})

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

func TestEvaluateScenarioDecodesShocks(t *testing.T) {
	ms := domain.NewMeasureSet("PF1", asOf, map[v1.MeasureName]v1.Measure{})
	var gotShocks []v1.ScenarioShock
	srv := grpcsrv.New(fakeEngine{scenario: func(r v1.ScenarioRequest) (v1.ScenarioResponse, error) {
		gotShocks = r.Shocks
		return v1.ScenarioResponse{PortfolioID: "PF1", Projected: ms}, nil
	}})

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
	srv := grpcsrv.New(fakeEngine{})
	_, err := srv.EvaluateScenario(context.Background(), &querypb.EvaluateScenarioRequest{
		PortfolioId: "PF1",
		Shocks:      []*querypb.ScenarioShock{{}}, // empty oneof
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("want InvalidArgument, got %v", err)
	}
}

func TestHealthMapsMode(t *testing.T) {
	srv := grpcsrv.New(fakeEngine{health: func() (v1.Health, error) {
		return v1.Health{Mode: v1.ModeDegraded, AsOf: asOf, Staleness: 90 * time.Second}, nil
	}})
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := grpcsrv.New(fakeEngine{exposure: func(v1.ExposureRequest) (v1.ExposureResponse, error) {
				return v1.ExposureResponse{}, tc.err
			}})
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
	}})
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
