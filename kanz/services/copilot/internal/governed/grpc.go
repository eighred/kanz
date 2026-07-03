package governed

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	domainpb "github.com/kanz-eng/kanz-schemas-go/domain/v1"
	querypb "github.com/kanz-eng/kanz-schemas-go/query/v1"
)

// GRPCClient is the PRODUCTION governed.Client (WIRE-02b): it reads risk state
// over the query.v1 RiskQueryService — the API-01c governed path the copilot
// reaches the risk engine through (mTLS at the composition root) — and maps each
// response into a Reading. It replaces the in-memory StubClient at cmd/copilot.
//
// The authz seam it feeds is deny-by-default: OwnerTenant returns the owning
// tenant the tool registry checks the caller against BEFORE any read. WIRE-02a
// put that owning tenant on the query.v1 responses; a query.v1-only client could
// not enforce cross-tenant isolation without it. An empty owner_tenant (the
// engine has no ownership record) resolves to "" — the deny-by-default gate then
// denies, so a missing owner fails closed, never open.
type GRPCClient struct {
	client querypb.RiskQueryServiceClient
}

// NewGRPCClient builds a governed client over the generated RiskQueryService
// client (constructed from the mTLS ClientConn at the composition root).
func NewGRPCClient(client querypb.RiskQueryServiceClient) *GRPCClient {
	return &GRPCClient{client: client}
}

var _ Client = (*GRPCClient)(nil)

// OwnerTenant resolves the portfolio's owning tenant for the authorization gate.
// It reads Exposure and returns only the owner_tenant, discarding the payload —
// so the caller learns nothing but the tenant to authorize against. A NOT_FOUND
// is mapped to ErrUnknownPortfolio (tenant-indistinguishable, so existence is
// not a cross-tenant oracle).
func (c *GRPCClient) OwnerTenant(ctx context.Context, portfolioID string) (string, error) {
	resp, err := c.client.Exposure(ctx, &querypb.ExposureRequest{PortfolioId: portfolioID})
	if err != nil {
		return "", mapErr(err)
	}
	return resp.GetOwnerTenant(), nil
}

// Measures reads the named measures (empty ⇒ all) and maps them to a Reading.
func (c *GRPCClient) Measures(ctx context.Context, portfolioID string, names []string) (Reading, error) {
	resp, err := c.client.Measures(ctx, &querypb.MeasuresRequest{PortfolioId: portfolioID, Measures: names})
	if err != nil {
		return Reading{}, mapErr(err)
	}
	return Reading{
		PortfolioID:   portfolioID,
		Tenant:        resp.GetOwnerTenant(),
		Kind:          "measures",
		Values:        measureValues(resp.GetSet()),
		SourceEventID: citation(resp.GetSourcePosition()),
		AsOf:          asOf(resp.GetAsOf().AsTime()),
	}, nil
}

// Exposure reads the exposure aggregate and maps it to a Reading.
func (c *GRPCClient) Exposure(ctx context.Context, portfolioID string) (Reading, error) {
	resp, err := c.client.Exposure(ctx, &querypb.ExposureRequest{PortfolioId: portfolioID})
	if err != nil {
		return Reading{}, mapErr(err)
	}
	return Reading{
		PortfolioID:   portfolioID,
		Tenant:        resp.GetOwnerTenant(),
		Kind:          "exposure",
		Values:        exposureValues(resp.GetSet()),
		SourceEventID: citation(resp.GetSourcePosition()),
		AsOf:          asOf(resp.GetAsOf().AsTime()),
	}, nil
}

// EvaluateScenario applies a named parallel-shift scenario and maps the projected
// measures to a Reading. The scenario name is a signed decimal fraction (e.g.
// "-0.05" ⇒ everything drops 5%); an unparseable name is an error rather than a
// silent no-op. The scenario response carries no owning tenant (it is a pure
// function of already-authorized state), so Tenant is left empty — the gate has
// already run against OwnerTenant before this read.
func (c *GRPCClient) EvaluateScenario(ctx context.Context, portfolioID, scenario string) (Reading, error) {
	pct, err := parsePct(scenario)
	if err != nil {
		return Reading{}, err
	}
	resp, err := c.client.EvaluateScenario(ctx, &querypb.EvaluateScenarioRequest{
		PortfolioId: portfolioID,
		Shocks:      []*querypb.ScenarioShock{{Shock: &querypb.ScenarioShock_ParallelShift{ParallelShift: &querypb.ParallelShift{Pct: pct}}}},
	})
	if err != nil {
		return Reading{}, mapErr(err)
	}
	return Reading{
		PortfolioID: portfolioID,
		Kind:        "scenario",
		Values:      measureValues(resp.GetProjected()),
	}, nil
}

// --- mapping ------------------------------------------------------------------

// measureValues maps a RiskMeasureSet to name→value.
func measureValues(set *domainpb.RiskMeasureSet) map[string]float64 {
	out := map[string]float64{}
	for _, m := range set.GetMeasures() {
		out[m.GetName()] = decFloat(m.GetValue())
	}
	return out
}

// exposureValues maps an ExposureSet to "dimension/bucket"→net value. The
// composite key avoids collisions when the same bucket name appears under
// different dimensions.
func exposureValues(set *domainpb.ExposureSet) map[string]float64 {
	out := map[string]float64{}
	for _, e := range set.GetExposures() {
		key := e.GetDimension().String() + "/" + e.GetBucket()
		out[key] = decFloat(e.GetNet().GetAmount())
	}
	return out
}

// citation renders a LogPosition to the Reading's source-citation string:
// "topic@partition:offset". An unset position (the engine did not anchor the
// state to a durable-log coordinate) yields "" — the answer then carries no
// source citation rather than a fabricated one.
func citation(pos *commonpb.LogPosition) string {
	if pos == nil || pos.GetTopic() == "" {
		return ""
	}
	return fmt.Sprintf("%s@%d:%d", pos.GetTopic(), pos.GetPartition(), pos.GetOffset())
}

// parsePct decodes a scenario name (a signed decimal fraction) into a
// common.v1.Decimal shock. The value is carried exactly as coefficient×10^exp.
func parsePct(name string) (*commonpb.Decimal, error) {
	f, err := strconv.ParseFloat(name, 64)
	if err != nil {
		return nil, fmt.Errorf("governed: scenario %q is not a decimal fraction: %w", name, err)
	}
	// Six decimal places covers a percentage shock without float artefacts.
	const exp = -6
	coeff := int64(math.Round(f * math.Pow10(-exp)))
	return &commonpb.Decimal{Coefficient: coeff, Exponent: exp}, nil
}

func decFloat(d *commonpb.Decimal) float64 {
	if d == nil {
		return 0
	}
	return float64(d.GetCoefficient()) * math.Pow10(int(d.GetExponent()))
}

func asOf(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC()
}

// mapErr maps a gRPC NOT_FOUND to ErrUnknownPortfolio (so the copilot cannot use
// existence as a cross-tenant oracle); other errors propagate unchanged.
func mapErr(err error) error {
	if status.Code(err) == codes.NotFound {
		return ErrUnknownPortfolio
	}
	return err
}
