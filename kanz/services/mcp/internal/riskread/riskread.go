// Package riskread is the MCP plane's read of governed risk state (#743).
//
// IT READS query.v1 AND PROJECTS NARROWLY. AGENTS.md requires the MCP plane be
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
	"github.com/eighred/kanz/internal/measureread"
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

// Measures reads the portfolio's risk measures.
//
// THE PROJECTION IS internal/measureread AND NOT A MAP OF FLOATS (#757). This
// used to return name→float64 built with dec.Float64Or(value, 0), which threw
// away the three things the engine computes to say whether a number can be
// trusted: the per-measure InputCoverage (#527), the response's quality flags,
// and the as-of stamp. On this hop that is worse than anywhere else, because the
// consumer is a language model that will state whatever it is handed as prose —
// and the fixed-income family runs over a contract-terms store with no
// production writer, so DV01 arrives as a zero computed over zero bonds.
//
// The owning tenant and source position are STILL dropped, and that part was
// always right: neither is something this plane decided to expose, and a
// projection that passes fields through "because they were there" is how a read
// plane becomes a data tap. What changed is that the integrity record is not one
// of them — it is the difference between a number and a claim.
func (c *Client) Measures(ctx context.Context, portfolioID string) (measureread.Set, error) {
	resp, err := c.query.Measures(ctx, &querypb.MeasuresRequest{PortfolioId: portfolioID})
	if err != nil {
		return measureread.Set{}, mapErr(err)
	}
	return measureread.ProjectMeasures(resp), nil
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
