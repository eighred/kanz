package governed

import (
	"context"
	decutil "github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/measureread"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
)

// fakeQuery is a hand-rolled querypb.RiskQueryServiceClient for the mapping tests.
type fakeQuery struct {
	exposure func(*querypb.ExposureRequest) (*querypb.ExposureResponse, error)
	measures func(*querypb.MeasuresRequest) (*querypb.MeasuresResponse, error)
	scenario func(*querypb.EvaluateScenarioRequest) (*querypb.EvaluateScenarioResponse, error)
}

func (f fakeQuery) Exposure(_ context.Context, r *querypb.ExposureRequest, _ ...grpc.CallOption) (*querypb.ExposureResponse, error) {
	return f.exposure(r)
}
func (f fakeQuery) Measures(_ context.Context, r *querypb.MeasuresRequest, _ ...grpc.CallOption) (*querypb.MeasuresResponse, error) {
	return f.measures(r)
}
func (f fakeQuery) EvaluateScenario(_ context.Context, r *querypb.EvaluateScenarioRequest, _ ...grpc.CallOption) (*querypb.EvaluateScenarioResponse, error) {
	return f.scenario(r)
}

// Copilot does not list portfolios — its governed tools always name one. The
// method exists so the fake satisfies the generated client interface.
func (f fakeQuery) ListPortfolios(context.Context, *querypb.ListPortfoliosRequest, ...grpc.CallOption) (*querypb.ListPortfoliosResponse, error) {
	return nil, nil
}

func (f fakeQuery) Health(context.Context, *querypb.HealthRequest, ...grpc.CallOption) (*querypb.HealthResponse, error) {
	return &querypb.HealthResponse{}, nil
}

func dec(coeff int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coeff, Exponent: exp}
}

// OwnerTenant returns the response owner_tenant — the authz-gate input.
func TestGRPCOwnerTenant(t *testing.T) {
	c := NewGRPCClient(fakeQuery{exposure: func(r *querypb.ExposureRequest) (*querypb.ExposureResponse, error) {
		if r.GetPortfolioId() != "PF" {
			t.Errorf("portfolio not forwarded: %q", r.GetPortfolioId())
		}
		return &querypb.ExposureResponse{OwnerTenant: "acme"}, nil
	}})
	got, err := c.OwnerTenant(context.Background(), "PF")
	if err != nil {
		t.Fatalf("OwnerTenant: %v", err)
	}
	if got != "acme" {
		t.Fatalf("owner = %q, want acme", got)
	}
}

// A NOT_FOUND is mapped to ErrUnknownPortfolio (tenant-indistinguishable).
func TestGRPCOwnerTenantNotFound(t *testing.T) {
	c := NewGRPCClient(fakeQuery{exposure: func(*querypb.ExposureRequest) (*querypb.ExposureResponse, error) {
		return nil, status.Error(codes.NotFound, "unknown")
	}})
	if _, err := c.OwnerTenant(context.Background(), "PF"); err != ErrUnknownPortfolio {
		t.Fatalf("err = %v, want ErrUnknownPortfolio", err)
	}
}

// An empty owner_tenant resolves to "" — the deny-by-default gate then denies.
func TestGRPCOwnerTenantEmptyFailsClosed(t *testing.T) {
	c := NewGRPCClient(fakeQuery{exposure: func(*querypb.ExposureRequest) (*querypb.ExposureResponse, error) {
		return &querypb.ExposureResponse{}, nil // no owner
	}})
	got, err := c.OwnerTenant(context.Background(), "PF")
	if err != nil {
		t.Fatalf("OwnerTenant: %v", err)
	}
	if got != "" {
		t.Fatalf("owner = %q, want empty (fail-closed)", got)
	}
}

// Measures maps the set to values and carries the tenant + citation seed.
func TestGRPCMeasures(t *testing.T) {
	c := NewGRPCClient(fakeQuery{measures: func(r *querypb.MeasuresRequest) (*querypb.MeasuresResponse, error) {
		return &querypb.MeasuresResponse{
			PortfolioId: "PF",
			OwnerTenant: "acme",
			AsOf:        timestamppb.New(mustTime("2026-07-03T00:00:00Z")),
			Set: &domainpb.RiskMeasureSet{Measures: []*domainpb.RiskMeasure{
				{Name: "VaR99", Value: dec(125000, -2)}, // 1250.00
			}},
			SourcePosition: &commonpb.LogPosition{Topic: "risk.state", Partition: 2, Offset: 42},
		}, nil
	}})
	r, err := c.Measures(context.Background(), "PF", []string{"VaR99"})
	if err != nil {
		t.Fatalf("Measures: %v", err)
	}
	if r.Tenant != "acme" || r.Kind != "measures" {
		t.Fatalf("reading meta wrong: %+v", r)
	}
	if v, ok := statedValue(r, "VaR99"); !ok || v < 1249.99 || v > 1250.01 {
		t.Fatalf("VaR99 = %v (stated=%v), want 1250", v, ok)
	}
	if r.SourceEventID != "risk.state@2:42" {
		t.Fatalf("citation = %q, want risk.state@2:42", r.SourceEventID)
	}
}

// Exposure maps net money per dimension/bucket.
func TestGRPCExposure(t *testing.T) {
	c := NewGRPCClient(fakeQuery{exposure: func(*querypb.ExposureRequest) (*querypb.ExposureResponse, error) {
		return &querypb.ExposureResponse{
			OwnerTenant: "acme",
			Set: &domainpb.ExposureSet{Exposures: []*domainpb.ExposureState{
				{Dimension: domainpb.ExposureDimension_EXPOSURE_DIMENSION_CURRENCY, Bucket: "USD",
					Net: &commonpb.Money{Amount: dec(500000, 0), CurrencyCode: "USD"}},
			}},
		}, nil
	}})
	r, err := c.Exposure(context.Background(), "PF")
	if err != nil {
		t.Fatalf("Exposure: %v", err)
	}
	if r.Tenant != "acme" {
		t.Fatalf("tenant = %q", r.Tenant)
	}
	if v, ok := statedValue(r, "EXPOSURE_DIMENSION_CURRENCY/USD"); !ok || v != 500000 {
		t.Fatalf("net = %v (stated=%v), want 500000; measures=%+v", v, ok, r.Measures)
	}
}

// EvaluateScenario parses the scenario fraction into a parallel shift and maps
// the projected measures.
func TestGRPCEvaluateScenario(t *testing.T) {
	var gotPct *commonpb.Decimal
	c := NewGRPCClient(fakeQuery{scenario: func(r *querypb.EvaluateScenarioRequest) (*querypb.EvaluateScenarioResponse, error) {
		if len(r.GetShocks()) == 1 {
			gotPct = r.GetShocks()[0].GetParallelShift().GetPct()
		}
		return &querypb.EvaluateScenarioResponse{
			Projected: &domainpb.RiskMeasureSet{Measures: []*domainpb.RiskMeasure{{Name: "PnL", Value: dec(-2500, 0)}}},
		}, nil
	}})
	r, err := c.EvaluateScenario(context.Background(), "PF", "-0.05")
	if err != nil {
		t.Fatalf("EvaluateScenario: %v", err)
	}
	pct, pctOK := decutil.Float64(gotPct)
	if !pctOK || pct > -0.049 || pct < -0.051 {
		t.Fatalf("shock pct = %v (convertible=%v), want ~-0.05", pct, pctOK)
	}
	if v, ok := statedValue(r, "PnL"); !ok || v != -2500 {
		t.Fatalf("PnL = %v (stated=%v), want -2500", v, ok)
	}
}

// A non-decimal scenario name is an error, not a silent no-op.
func TestGRPCEvaluateScenarioRejectsBadName(t *testing.T) {
	c := NewGRPCClient(fakeQuery{})
	if _, err := c.EvaluateScenario(context.Background(), "PF", "crash"); err == nil {
		t.Fatal("expected error for a non-decimal scenario name")
	}
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// statedValue reads one MEASURED value out of a Reading. It returns ok=false for
// a withheld measure as well as an absent one, because a test asserting on a
// number must fail either way: a value this plane refused to state is not a
// value (#757).
func statedValue(r Reading, name string) (float64, bool) {
	for _, m := range r.Measures {
		if m.Name == name {
			return derefOrZero(m.Value), m.Status == measureread.StatusMeasured && m.Value != nil
		}
	}
	return 0, false
}

func derefOrZero(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}
