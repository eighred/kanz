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

// ForDataset returns ds's governed provenance. Access to the QUERIED dataset is
// deny-by-default: a PII target the principal can't read returns ErrForbidden
// (and the denial is logged). Upstream PII nodes the principal can't read are
// included but redacted.
func (s *Service) ForDataset(ctx context.Context, p *auth.Principal, ds graph.DatasetID) (*Provenance, error) {
	info, ok := s.graph.Get(ds)
	if !ok {
		// The dataset table is never evicted — its cardinality is the estate's
		// schema count — so a miss here is not an eviction. It is still not
		// conclusive: a restarted pod holds no datasets either, and the durable
		// consumer does not replay the history that built them.
		return nil, s.miss()
	}
	d, sens := s.gov.CheckAccess(ctx, p, ds, info.SchemaRef)
	if !d.Allow {
		return nil, ErrForbidden
	}
	prov := &Provenance{
		Status:   graph.LookupRetained,
		Target:   node(info, sens, false),
		Coverage: s.graph.Coverage(),
	}
	for _, up := range s.graph.Upstream(ds) {
		upInfo, _ := s.graph.Get(up)
		ud, usens := s.gov.CheckAccess(ctx, p, up, upInfo.SchemaRef)
		prov.Upstream = append(prov.Upstream, node(upInfo, usens, !ud.Allow))
	}
	return prov, nil
}

// ForEvent resolves the dataset an event produced, then its provenance.
func (s *Service) ForEvent(ctx context.Context, p *auth.Principal, eventID string) (*Provenance, error) {
	ds, look := s.graph.DatasetOf(eventID)
	switch look {
	case graph.LookupRetained:
		return s.ForDataset(ctx, p, ds)
	case graph.LookupNotObserved:
		return nil, ErrNotFound
	default:
		return nil, ErrNotRetained
	}
}

// miss maps a lookup failure to the error that says what the failure MEANS.
func (s *Service) miss() error {
	if s.graph.Coverage().Complete {
		return ErrNotFound
	}
	return ErrNotRetained
}

// Coverage exposes the graph's coverage so a transport can render it alongside a
// refusal. Read through the graph rather than cached, so the number a caller is
// handed cannot drift from the index it describes.
func (s *Service) Coverage() graph.Coverage { return s.graph.Coverage() }

// Explain builds the body for a miss, stating what the miss does and does not
// establish. The two texts are deliberately not interchangeable: one is a
// finding, the other is a refusal to make one.
func (s *Service) Explain(status graph.Lookup) Unresolved {
	u := Unresolved{Status: status, Coverage: s.graph.Coverage()}
	if status == graph.LookupNotObserved {
		u.Error = "not found"
		u.Detail = "this index has observed the estate's full history and evicted nothing, " +
			"so this identifier was never observed"
		return u
	}
	u.Error = "not retained"
	u.Detail = "this index is bounded and does not cover the whole history: it retains " +
		"the most recent events only, and a restart resumes at the last acked event rather " +
		"than replaying. THIS IS NOT A FINDING THAT THE IDENTIFIER IS UNKNOWN TO THE ESTATE — " +
		"it is this service declining to answer. See coverage for what it does cover."
	return u
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
