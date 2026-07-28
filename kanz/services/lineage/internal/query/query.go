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
// PII lineage (deny-by-default). ErrNotFound when the event/dataset is unknown.
var (
	ErrForbidden = errors.New("query: access to PII lineage denied")
	ErrNotFound  = errors.New("query: dataset not found")
)

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
	Target   Node   `json:"target"`
	Upstream []Node `json:"upstream"`
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
		return nil, ErrNotFound
	}
	d, sens := s.gov.CheckAccess(ctx, p, ds, info.SchemaRef)
	if !d.Allow {
		return nil, ErrForbidden
	}
	prov := &Provenance{Target: node(info, sens, false)}
	for _, up := range s.graph.Upstream(ds) {
		upInfo, _ := s.graph.Get(up)
		ud, usens := s.gov.CheckAccess(ctx, p, up, upInfo.SchemaRef)
		prov.Upstream = append(prov.Upstream, node(upInfo, usens, !ud.Allow))
	}
	return prov, nil
}

// ForEvent resolves the dataset an event produced, then its provenance.
func (s *Service) ForEvent(ctx context.Context, p *auth.Principal, eventID string) (*Provenance, error) {
	ds, ok := s.graph.DatasetOf(eventID)
	if !ok {
		return nil, ErrNotFound
	}
	return s.ForDataset(ctx, p, ds)
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
