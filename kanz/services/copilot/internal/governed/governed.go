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

	"github.com/eighred/kanz/internal/measureread"
)

// ErrUnknownPortfolio is returned when the portfolio is not known to the read
// surface. It is deliberately indistinguishable across tenants so a cross-tenant
// probe cannot use existence as an oracle — authorization, not this error, is
// the tenant boundary.
var ErrUnknownPortfolio = errors.New("governed: unknown portfolio")

// Reading is a governed datum: named values for a portfolio, the tenant that
// owns it, and the source event id the values were computed from.
//
// IT CARRIES MEASURES AND NOT A MAP OF FLOATS (#757). This used to be
// map[string]float64 built with dec.Float64Or(value, 0), which discarded the
// per-measure InputCoverage (#527) and the response's quality flags — the two
// things the engine computes to say whether a number is a claim about the book
// or a zero that measured nothing. The copilot is the worst place to lose them:
// it hands these to a model that states them in prose, and a reader has no way
// to tell "DV01 is zero" from "DV01 was computed over no bonds".
//
// The projection is internal/measureread, SHARED WITH services/mcp. Both planes
// answer the same question — may this number be stated — and a second copy of
// that decision is how a fix stops spreading.
type Reading struct {
	PortfolioID   string
	Tenant        string
	Kind          string // "measures" | "exposure" | "scenario" | "performance"
	Measures      []measureread.Measure
	QualityFlags  []string
	SourceEventID string
	AsOf          time.Time
}

// StatedValues returns the values the model may cite — MEASURED ONLY.
//
// A withheld measure must never reach citedValues: the grounding gate
// (retrieval.CheckGrounding) treats that slice as "numbers a tool actually
// returned", so admitting a withheld one would license the model to state
// exactly the number this plane refused to state.
func (r Reading) StatedValues() []float64 {
	out := make([]float64, 0, len(r.Measures))
	for _, m := range r.Measures {
		if m.Status == measureread.StatusMeasured && m.Value != nil {
			out = append(out, *m.Value)
		}
	}
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

// Measured builds MEASURED measures from plain name→value pairs, ordered by
// name.
//
// It lives beside StubClient because it serves the same purpose: this package
// has always carried a dependency-free default for tests and local boot, and a
// fixture that had to hand-assemble measureread.Measure literals would tempt
// each caller into its own idea of what a sound measure looks like. It is
// deliberately unable to express a withheld one — a fixture that could would be
// a second place to decide what may be stated, and that decision has exactly one
// home (internal/measureread).
func Measured(values map[string]float64) []measureread.Measure {
	out := make([]measureread.Measure, 0, len(values))
	for name, v := range values {
		out = append(out, measureread.Measure{Name: name, Status: measureread.StatusMeasured, Value: &v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
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
	filtered := Reading{
		PortfolioID: r.PortfolioID, Tenant: r.Tenant, Kind: r.Kind,
		SourceEventID: r.SourceEventID, AsOf: r.AsOf, QualityFlags: r.QualityFlags,
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	for _, m := range r.Measures {
		if want[m.Name] {
			filtered.Measures = append(filtered.Measures, m)
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

// Coverage counts this reading's measures by whether the read plane will state a
// number for them (#973).
//
// IT IS DERIVED FROM Status AND NOT FROM Value BEING NIL. measureread already
// draws that line once — StatusUnavailable means "this plane will not state a
// number here", with Reason saying why — and re-deriving it from a nil pointer
// would be a second implementation of the same judgement, which is how the two
// drift and how a withheld measure starts being counted as present.
func (r Reading) Coverage() (measured, unavailable int) {
	for _, m := range r.Measures {
		if m.Status == measureread.StatusMeasured && m.Value != nil {
			measured++
			continue
		}
		unavailable++
	}
	return measured, unavailable
}
