// Package riskread is the MCP plane's read of governed risk state (#743).
//
// IT READS query.v1 AND PROJECTS NARROWLY. CLAUDE.md requires the MCP plane be
// "server-side filtered and projected" rather than handing an agent whatever a
// domain API returns, so this maps a MeasuresResponse down to name→value and
// discards everything else the response carries. What an agent cannot be given
// is decided here, once, rather than at each tool.
//
// IT IS NOT services/copilot/internal/governed, and the duplication is the
// point rather than an oversight. That client is another service's projection —
// it carries citations, as-of stamps and lineage seeds because a copilot answer
// must cite its source. This plane needs none of that, and importing it would
// both breach Go's internal rule and hand this surface fields nobody decided it
// should expose. The seam they share — "who owns this resource" — IS shared, in
// internal/agentgate, which is the part that must never be copied.
package riskread

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"

	"github.com/eighred/kanz/internal/agentgate"
	decutil "github.com/eighred/kanz/internal/dec"
)

// Client reads risk state over query.v1.
type Client struct {
	query querypb.RiskQueryServiceClient
}

// New builds the client over a generated query.v1 client, constructed from the
// mTLS ClientConn at the composition root.
func New(q querypb.RiskQueryServiceClient) *Client { return &Client{query: q} }

// OwnerTenant resolves the portfolio's owning tenant for the authorization gate,
// and returns NOTHING ELSE about it.
//
// It reads Exposure and discards the payload. That is deliberate and is the
// gate's contract: a caller about to be refused must not already have been
// handed the data, so the ownership question is answered without answering the
// data question.
//
// A NOT_FOUND becomes agentgate.ErrResourceNotVisible — the sentinel that means
// "not visible to you", covering both "does not exist" and "belongs to somebody
// else" with ONE answer, because a caller able to tell them apart can enumerate
// another tenant's portfolios by id.
func (c *Client) OwnerTenant(ctx context.Context, portfolioID string) (string, error) {
	resp, err := c.query.Exposure(ctx, &querypb.ExposureRequest{PortfolioId: portfolioID})
	if err != nil {
		return "", mapErr(err)
	}
	return resp.GetOwnerTenant(), nil
}

// Measures reads the portfolio's risk measures, projected to name→value.
//
// EVERY OTHER FIELD IS DROPPED HERE. The response carries an owning tenant, an
// as-of time, quality flags and a source position; none of them is something
// this plane decided to expose, and a projection that passed them through
// "because they were there" is how a read plane becomes a data tap.
func (c *Client) Measures(ctx context.Context, portfolioID string) (map[string]float64, error) {
	resp, err := c.query.Measures(ctx, &querypb.MeasuresRequest{PortfolioId: portfolioID})
	if err != nil {
		return nil, mapErr(err)
	}
	out := map[string]float64{}
	for _, m := range resp.GetSet().GetMeasures() {
		out[m.GetName()] = decutil.Float64Or(m.GetValue(), 0)
	}
	return out, nil
}

// mapErr maps a gRPC NOT_FOUND onto the gate's not-visible sentinel; every other
// error propagates unchanged, so a transient failure stays transient and the
// gate fails closed on it rather than reading it as an absent resource.
func mapErr(err error) error {
	if status.Code(err) == codes.NotFound {
		return agentgate.ErrResourceNotVisible
	}
	return err
}
