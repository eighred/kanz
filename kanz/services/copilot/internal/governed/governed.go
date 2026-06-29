// Package governed is the copilot's read surface over the platform's GOVERNED
// APIs (COPILOT-01b): the model touches the query.v1 RiskQueryService, scenario
// evaluation, and performance through this seam — never a raw store. Every
// Reading carries the owning tenant (for the AUTH-01 isolation gate) and the
// source event id that produced the value (the AUDIT-01 lineage citation seed),
// so a copilot answer can always cite "where did this number come from".
//
// The PRODUCTION client dials the api-gateway / risk-engine over mTLS (the
// API-01c governed path) and maps query.v1 responses into Readings; the default
// StubClient serves in-memory fixtures for tests and local boot — the same
// seam-with-a-dependency-free-default stance the rest of the platform takes.
package governed

import (
	"context"
	"errors"
	"sort"
	"time"
)

// ErrUnknownPortfolio is returned when the portfolio is not known to the read
// surface. It is deliberately indistinguishable across tenants so a cross-tenant
// probe cannot use existence as an oracle — authorization, not this error, is
// the tenant boundary.
var ErrUnknownPortfolio = errors.New("governed: unknown portfolio")

// Reading is a governed datum: named numeric values for a portfolio, the tenant
// that owns it, and the source event id the value was computed from.
type Reading struct {
	PortfolioID   string
	Tenant        string
	Kind          string // "measures" | "exposure" | "scenario" | "performance"
	Values        map[string]float64
	SourceEventID string
	AsOf          time.Time
}

// OrderedNames returns the value names sorted, for deterministic rendering.
func (r Reading) OrderedNames() []string {
	out := make([]string, 0, len(r.Values))
	for k := range r.Values {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Client is the governed read surface. OwnerTenant resolves a portfolio's owning
// tenant for the authorization gate WITHOUT returning any data (so a denied
// caller learns nothing); the read methods return the data once authorized.
type Client interface {
	OwnerTenant(ctx context.Context, portfolioID string) (string, error)
	Measures(ctx context.Context, portfolioID string, names []string) (Reading, error)
	Exposure(ctx context.Context, portfolioID string) (Reading, error)
	EvaluateScenario(ctx context.Context, portfolioID, scenario string) (Reading, error)
}

// StubClient is the in-memory default Client. Each portfolio maps to a set of
// readings keyed by kind.
type StubClient struct {
	owner    map[string]string             // portfolio -> tenant
	readings map[string]map[string]Reading // portfolio -> kind -> reading
}

// NewStubClient returns an empty stub.
func NewStubClient() *StubClient {
	return &StubClient{owner: map[string]string{}, readings: map[string]map[string]Reading{}}
}

// Put records a reading (and its portfolio→tenant ownership) for the stub.
func (c *StubClient) Put(r Reading) {
	c.owner[r.PortfolioID] = r.Tenant
	if c.readings[r.PortfolioID] == nil {
		c.readings[r.PortfolioID] = map[string]Reading{}
	}
	c.readings[r.PortfolioID][r.Kind] = r
}

// OwnerTenant returns the portfolio's owning tenant.
func (c *StubClient) OwnerTenant(_ context.Context, portfolioID string) (string, error) {
	t, ok := c.owner[portfolioID]
	if !ok {
		return "", ErrUnknownPortfolio
	}
	return t, nil
}

func (c *StubClient) read(portfolioID, kind string) (Reading, error) {
	byKind, ok := c.readings[portfolioID]
	if !ok {
		return Reading{}, ErrUnknownPortfolio
	}
	r, ok := byKind[kind]
	if !ok {
		return Reading{}, ErrUnknownPortfolio
	}
	return r, nil
}

// Measures returns the named measures (empty names ⇒ all).
func (c *StubClient) Measures(_ context.Context, portfolioID string, names []string) (Reading, error) {
	r, err := c.read(portfolioID, "measures")
	if err != nil {
		return Reading{}, err
	}
	if len(names) == 0 {
		return r, nil
	}
	filtered := Reading{PortfolioID: r.PortfolioID, Tenant: r.Tenant, Kind: r.Kind, SourceEventID: r.SourceEventID, AsOf: r.AsOf, Values: map[string]float64{}}
	for _, n := range names {
		if v, ok := r.Values[n]; ok {
			filtered.Values[n] = v
		}
	}
	return filtered, nil
}

// Exposure returns the exposure reading.
func (c *StubClient) Exposure(_ context.Context, portfolioID string) (Reading, error) {
	return c.read(portfolioID, "exposure")
}

// EvaluateScenario returns the scenario reading (the scenario name selects the
// pre-computed projection in the stub).
func (c *StubClient) EvaluateScenario(_ context.Context, portfolioID, _ string) (Reading, error) {
	return c.read(portfolioID, "scenario")
}
