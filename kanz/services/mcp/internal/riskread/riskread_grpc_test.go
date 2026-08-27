package riskread

// These run over a REAL gRPC connection on a bufconn listener, against a real
// query.v1 server — not a hand-built response struct handed to the projection.
//
// THE POINT IS THE WIRE (#757). The record this plane must not drop —
// domain.v1.InputCoverage, the response's quality flags, the as-of stamp — only
// matters if it survives protobuf marshalling into the client that reads it. A
// test that constructs a MeasuresResponse in memory and calls the projection
// directly proves the projection, and proves NOTHING about whether the fields
// arrive. The defect this fixes lived exactly there: on the hop between the wire
// and an agent.

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"

	"github.com/eighred/kanz/internal/agentgate"
	"github.com/eighred/kanz/internal/measureread"
)

// riskQueryServer is a risk engine, stood up over real gRPC.
type riskQueryServer struct {
	querypb.UnimplementedRiskQueryServiceServer
	measures *querypb.MeasuresResponse
	exposure *querypb.ExposureResponse
	err      error
}

func (s *riskQueryServer) Measures(_ context.Context, _ *querypb.MeasuresRequest) (*querypb.MeasuresResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.measures, nil
}

func (s *riskQueryServer) Exposure(_ context.Context, _ *querypb.ExposureRequest) (*querypb.ExposureResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.exposure, nil
}

func dialEngine(t *testing.T, srv querypb.RiskQueryServiceServer) *Client {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	querypb.RegisterRiskQueryServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return New(querypb.NewRiskQueryServiceClient(conn))
}

func dec(coeff int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coeff, Exponent: exp}
}

func measureOf(t *testing.T, set measureread.Set, name string) measureread.Measure {
	t.Helper()
	for _, m := range set.Measures {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("measure %q never arrived (%d measures) — a measure that cannot be stated must still "+
		"be reported, never dropped", name, len(set.Measures))
	return measureread.Measure{}
}

// THE PRODUCTION SHAPE, OVER THE WIRE: the fixed-income family registered over a
// contract-terms store with no production writer. DV01 is a real zero the engine
// computed over zero bonds, and it must not arrive as a number.
func TestOverGRPC_AMeasureComputedOverNothingArrivesWithoutANumber(t *testing.T) {
	c := dialEngine(t, &riskQueryServer{measures: &querypb.MeasuresResponse{
		PortfolioId: "PF-1",
		OwnerTenant: "t1",
		AsOf:        timestamppb.New(mustUTC(t, "2026-08-27T09:00:00Z")),
		Set: &domainpb.RiskMeasureSet{PortfolioId: "PF-1", Measures: []*domainpb.RiskMeasure{
			{
				Name:  "DV01",
				Value: dec(0, 0),
				Coverage: &domainpb.InputCoverage{
					Contributed:   0,
					ExcludedCount: 12,
					Exclusions:    []*domainpb.InputExclusion{{InstrumentId: "BOND-1", Reason: "no_terms"}},
				},
			},
			{Name: "GrossExposure", Value: dec(500, 0), UncertaintyAbs: dec(25, -1)},
		}},
		QualityFlags: []querypb.QualityFlag{querypb.QualityFlag_QUALITY_FLAG_INPUTS_UNRESOLVED},
	}})

	set, err := c.Measures(context.Background(), "PF-1")
	if err != nil {
		t.Fatalf("Measures over gRPC: %v", err)
	}

	dv01 := measureOf(t, set, "DV01")
	if dv01.Status != measureread.StatusUnavailable || dv01.Value != nil {
		t.Fatalf("DV01 = %+v after a real round-trip, want withheld — this is the exact production "+
			"shape, and a number here is what an agent restates as \"no interest-rate risk\"", dv01)
	}
	// THE COVERAGE SURVIVED MARSHALLING. If domain.v1.InputCoverage did not
	// cross the wire, the projection would see nil coverage and state the zero —
	// and the unit test, which builds the response in memory, could never tell.
	if !dv01.Coverage.Reported || dv01.Coverage.Excluded != 12 {
		t.Errorf("DV01 coverage = %+v after the wire, want reported with excluded=12", dv01.Coverage)
	}
	if len(dv01.Coverage.ExclusionReasons) != 1 || dv01.Coverage.ExclusionReasons[0] != "no_terms" {
		t.Errorf("exclusion reasons = %v, want [no_terms] — the reason is what names the empty store",
			dv01.Coverage.ExclusionReasons)
	}

	sound := measureOf(t, set, "GrossExposure")
	if sound.Status != measureread.StatusMeasured || sound.Value == nil || *sound.Value != 500 {
		t.Errorf("GrossExposure = %+v, want 500 measured — refusing the sound measure beside the "+
			"degraded one is the all-or-nothing #509 removed", sound)
	}
	if sound.UncertaintyAbs == nil || *sound.UncertaintyAbs != 2.5 {
		t.Errorf("uncertainty = %v, want 2.5 across the wire", sound.UncertaintyAbs)
	}
	if len(set.QualityFlags) != 1 || set.QualityFlags[0] != "INPUTS_UNRESOLVED" {
		t.Errorf("quality flags = %v after the wire, want [INPUTS_UNRESOLVED]", set.QualityFlags)
	}
	if set.AsOf == nil {
		t.Error("as_of did not survive the wire — without it an agent cannot tell current state " +
			"from state folded hours ago")
	}
}

// CURRENCY_EXCLUDED CROSSES THE WIRE AS A REFUSAL, not as a flag beside a number.
func TestOverGRPC_CurrencyExcludedWithholdsEveryValue(t *testing.T) {
	c := dialEngine(t, &riskQueryServer{measures: &querypb.MeasuresResponse{
		PortfolioId: "PF-1",
		Set: &domainpb.RiskMeasureSet{Measures: []*domainpb.RiskMeasure{
			{Name: "GrossExposure", Value: dec(500, 0)},
			{Name: "VaR99", Value: dec(125, 0)},
		}},
		QualityFlags: []querypb.QualityFlag{querypb.QualityFlag_QUALITY_FLAG_CURRENCY_EXCLUDED},
	}})

	set, err := c.Measures(context.Background(), "PF-1")
	if err != nil {
		t.Fatalf("Measures over gRPC: %v", err)
	}
	for _, name := range []string{"GrossExposure", "VaR99"} {
		if m := measureOf(t, set, name); m.Status != measureread.StatusUnavailable || m.Value != nil {
			t.Errorf("%s = %+v, want withheld — the engine has no FX layer, so the response covered "+
				"a currency subset and every base-currency number on it moves in an unknown direction",
				name, m)
		}
	}
}

// NON-VACUITY, over the wire: a clean response is served in full. Without this,
// every withholding assertion above is satisfied by a plane that refuses
// everything.
func TestOverGRPC_ACleanResponseIsServedInFull(t *testing.T) {
	c := dialEngine(t, &riskQueryServer{measures: &querypb.MeasuresResponse{
		PortfolioId: "PF-1",
		Set: &domainpb.RiskMeasureSet{Measures: []*domainpb.RiskMeasure{
			{Name: "VaR99", Value: dec(125000, -2), Coverage: &domainpb.InputCoverage{Contributed: 500}},
		}},
	}})

	set, err := c.Measures(context.Background(), "PF-1")
	if err != nil {
		t.Fatalf("Measures over gRPC: %v", err)
	}
	m := measureOf(t, set, "VaR99")
	if m.Status != measureread.StatusMeasured || m.Value == nil || *m.Value != 1250 {
		t.Fatalf("VaR99 = %+v, want 1250 measured across the wire", m)
	}
	// Coverage that REPORTS and excludes nothing is the "everything applicable
	// was covered" case, and it must not read the same as absent coverage.
	if !m.Coverage.Reported || m.Coverage.Contributed != 500 || m.Coverage.Excluded != 0 {
		t.Errorf("coverage = %+v, want reported contributed=500 excluded=0", m.Coverage)
	}
}

// A NOT_FOUND BECOMES THE GATE'S NOT-VISIBLE SENTINEL, over a real status wire.
// Any other error stays an error, so a transient failure fails closed rather
// than reading as an absent portfolio.
func TestOverGRPC_NotFoundMapsToNotVisibleAndOtherErrorsDoNot(t *testing.T) {
	notFound := dialEngine(t, &riskQueryServer{err: status.Error(codes.NotFound, "no such portfolio")})
	if _, err := notFound.Measures(context.Background(), "PF-1"); err != agentgate.ErrResourceNotVisible {
		t.Errorf("NOT_FOUND over the wire = %v, want ErrResourceNotVisible — a caller able to tell "+
			"\"absent\" from \"another tenant's\" can enumerate by id", err)
	}
	flaky := dialEngine(t, &riskQueryServer{err: status.Error(codes.Unavailable, "engine restarting")})
	if _, err := flaky.Measures(context.Background(), "PF-1"); err == nil || err == agentgate.ErrResourceNotVisible {
		t.Errorf("UNAVAILABLE over the wire = %v, want a plain error — reading an outage as an "+
			"absent resource turns availability into an authorization answer", err)
	}
}

// OwnerTenant reads ownership and RETURNS NOTHING ELSE, across the wire.
func TestOverGRPC_OwnerTenantResolvesWithoutHandingBackData(t *testing.T) {
	c := dialEngine(t, &riskQueryServer{exposure: &querypb.ExposureResponse{
		PortfolioId: "PF-1",
		OwnerTenant: "t1",
		Set: &domainpb.ExposureSet{Exposures: []*domainpb.ExposureState{
			{Dimension: domainpb.ExposureDimension_EXPOSURE_DIMENSION_ASSET_CLASS, Bucket: "EQUITY",
				Net: &commonpb.Money{Amount: dec(500, 0), CurrencyCode: "USD"}},
		}},
	}})

	tenant, err := c.OwnerTenant(context.Background(), "PF-1")
	if err != nil {
		t.Fatalf("OwnerTenant over gRPC: %v", err)
	}
	if tenant != "t1" {
		t.Fatalf("owner tenant = %q, want t1", tenant)
	}
}

func mustUTC(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v.UTC()
}
