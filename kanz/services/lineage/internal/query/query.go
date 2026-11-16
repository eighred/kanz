// Package query answers the LIN-01d "where did this number come from": given an
// event or dataset, it returns the full upstream lineage over the graph,
// governed by LIN-01c. It complements the AUDIT-01c event-level causal chain —
// that walks event_ids in the audit log; this walks the datasets those events
// belong to, the view a data steward or regulator reasons in.
package query

import (
	"context"
	"errors"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/lineage/internal/governance"
	"github.com/eighred/kanz/services/lineage/internal/graph"
)

// ErrForbidden is returned when the principal may not read the queried dataset's
// PII lineage (deny-by-default).
//
// THE OTHER TWO ARE THE POINT OF #244. A miss has two meanings and they are not
// interchangeable:
//
//	ErrNotFound     — CHECKED, AND THERE IS NO RECORD. The graph has observed all
//	                  of history and evicted nothing, so this answer is a fact
//	                  about the estate.
//	ErrNotRetained  — NOT CHECKED. The graph's index is bounded and starts empty
//	                  on restart, so it cannot say whether this was ever observed.
//	                  A fact about this pod, and NOT evidence of absence.
//
// Returning ErrNotFound for the second is the whole failure this service had:
// "where did this number come from" answered "nowhere", with the confidence of
// an answer and the content of a shrug. Same disarm-vs-armed split the mandate
// registry draws with ErrMandateTenantUnresolved and Decision.Ungoverned
// (EXEC-M13/M14) — a state that means "nothing was consulted" must be
// representable, or it gets served as a verdict.
var (
	ErrForbidden   = errors.New("query: access to PII lineage denied")
	ErrNotFound    = errors.New("query: no such event or dataset")
	ErrNotRetained = errors.New("query: outside this index's retained provenance window — not an answer")
)

// Unresolved is the body served for a miss. It carries the graph's Coverage so a
// caller can see WHY the question went unanswered, and Status so the three
// outcomes are distinguishable in the payload as well as in the status code.
type Unresolved struct {
	Status   graph.Lookup   `json:"status"`
	Error    string         `json:"error"`
	Detail   string         `json:"detail"`
	Coverage graph.Coverage `json:"coverage"`
}

// Node is one dataset in a provenance answer.
type Node struct {
	Dataset     graph.DatasetID        `json:"dataset"`
	Domain      string                 `json:"domain,omitempty"`
	SchemaRef   string                 `json:"schema_ref,omitempty"`
	Sensitivity governance.Sensitivity `json:"sensitivity"`
	// Redacted marks an upstream PII dataset the requester is not authorized to
	// read — its place in the lineage is shown (topology is metadata) but it is
	// flagged so a UI does not present it as freely accessible. Every such check
	// is logged by the governor.
	Redacted bool `json:"redacted,omitempty"`
}

// Provenance is the queried dataset plus its full transitive upstream lineage.
type Provenance struct {
	// Status is always graph.LookupRetained — it is present so a caller can switch
	// on one field across a hit and both kinds of miss (see Unresolved).
	Status   graph.Lookup `json:"status"`
	Target   Node         `json:"target"`
	Upstream []Node       `json:"upstream"`
	// Coverage rides even a successful answer, because eviction can also have cost
	// the graph an EDGE: Coverage.UnlinkedCauses > 0 means some derived-from links
	// were never drawn, so Upstream may be short without anything here saying so.
	Coverage graph.Coverage `json:"coverage"`
}

// Service resolves provenance under governance.
type Service struct {
	graph graph.Graph
	gov   *governance.Governor
}

func NewService(g graph.Graph, gov *governance.Governor) *Service {
	return &Service{graph: g, gov: gov}
}

func callerScope(p *auth.Principal) (graph.Owner, error) {
	if p == nil || p.Tenant == "" {
		return "", ErrForbidden
	}
	// Selector only. Resource ownership is independently read from stored Info.
	return graph.Owner(p.Tenant), nil
}

type lookupError struct {
	status   graph.Lookup
	coverage graph.Coverage
}

func (e *lookupError) Error() string { return e.Unwrap().Error() }
func (e *lookupError) Unwrap() error {
	if e.status == graph.LookupNotObserved {
		return ErrNotFound
	}
	return ErrNotRetained
}
func (s *Service) miss(owner graph.Owner) error {
	coverage := s.graph.Coverage(owner)
	status := graph.LookupUnknown
	if coverage.Complete {
		status = graph.LookupNotObserved
	}
	return &lookupError{status: status, coverage: coverage}
}
func (s *Service) ForDataset(ctx context.Context, p *auth.Principal, ds graph.DatasetID) (*Provenance, error) {
	owner, err := callerScope(p)
	if err != nil {
		return nil, err
	}
	info, ok := s.graph.Get(owner, ds)
	if !ok {
		return nil, s.miss(owner)
	}
	d, sens := s.gov.CheckAccess(ctx, p, info)
	if !d.Allow {
		if d.Code.IsIsolation() {
			return nil, s.miss(owner)
		}
		return nil, ErrForbidden
	}
	prov := &Provenance{Status: graph.LookupRetained, Target: node(info, sens, false), Coverage: s.graph.Coverage(owner)}
	for _, up := range s.graph.Upstream(owner, ds) {
		d, sens := s.gov.CheckAccess(ctx, p, up)
		if !d.Allow && d.Code.IsIsolation() {
			continue
		}
		prov.Upstream = append(prov.Upstream, node(up, sens, !d.Allow))
	}
	return prov, nil
}
func (s *Service) ForEvent(ctx context.Context, p *auth.Principal, eventID string) (*Provenance, error) {
	owner, err := callerScope(p)
	if err != nil {
		return nil, err
	}
	ds, look := s.graph.DatasetOf(owner, eventID)
	if look != graph.LookupRetained {
		return nil, s.miss(owner)
	}
	return s.ForDataset(ctx, p, ds)
}

type Catalog struct {
	Count    int            `json:"count"`
	Datasets []graph.Info   `json:"datasets"`
	Coverage graph.Coverage `json:"coverage"`
}

// Catalog preserves the existing same-owner metadata policy. Ownership is
// enforced before listing; PII provenance keeps its separate existing gate.
func (s *Service) Catalog(_ context.Context, p *auth.Principal) (Catalog, error) {
	owner, err := callerScope(p)
	if err != nil {
		return Catalog{}, err
	}
	items := make([]graph.Info, 0)
	for _, info := range s.graph.Datasets(owner) {
		if s.gov.CheckOwner(p, info).Allow {
			items = append(items, info)
		}
	}
	return Catalog{Count: len(items), Datasets: items, Coverage: s.graph.Coverage(owner)}, nil
}

// Explain uses the scoped coverage captured with the miss, never a global
// fallback or a second graph read at the HTTP error boundary.
func (s *Service) Explain(err error) (Unresolved, bool) {
	var miss *lookupError
	if !errors.As(err, &miss) {
		return Unresolved{}, false
	}
	u := Unresolved{Status: miss.status, Coverage: miss.coverage}
	if miss.status == graph.LookupNotObserved {
		u.Error = "not found"
		u.Detail = "this explicitly declared owner-scoped history was completely observed and evicted nothing; this identifier was never observed in that scope"
		return u, true
	}
	u.Error = "not retained"
	u.Detail = "this bounded owner-scoped index does not establish absence: history may precede this process, be unretained, or never have been observed in this scope"
	return u, true
}

func node(info graph.Info, sens governance.Sensitivity, redacted bool) Node {
	n := Node{
		Dataset:     info.ID,
		Domain:      info.Domain,
		SchemaRef:   info.SchemaRef,
		Sensitivity: sens,
		Redacted:    redacted,
	}
	if redacted {
		// Don't leak a PII dataset's schema ref to an unauthorized requester.
		n.SchemaRef = ""
	}
	return n
}
