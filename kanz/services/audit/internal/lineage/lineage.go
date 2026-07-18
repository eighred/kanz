// Package lineage reconstructs the causal history of any audited event
// (AUDIT-01c): given an event_id, it walks the envelope causation_id back to the
// root that triggered the cascade, and assembles the full correlation tree — the
// "what led to this, and what else did the same trigger cause" an investigator
// or regulator needs. It reads the AUDIT-01a store; the lineage substrate it
// walks (correlation_id / causation_id) is the chain EVT-17c stamps on every
// event.
package lineage

import (
	"context"
	"errors"

	"github.com/kanz-eng/kanz/services/audit/internal/audit"
)

// ErrNotFound is returned when the requested event_id is not in the audit log.
var ErrNotFound = errors.New("lineage: event not found in audit log")

// Node is one record in the correlation tree, with the records it directly
// caused as children (linked by causation_id).
type Node struct {
	Record   *audit.Record `json:"record"`
	Children []*Node       `json:"children,omitempty"`
}

// Lineage is the reconstructed causal context of a target event.
type Lineage struct {
	// Target is the requested event.
	Target *audit.Record `json:"target"`
	// Ancestry is the direct causal chain from the root trigger down to the
	// target (root first, target last) — each step is the event that directly
	// caused the next via causation_id.
	Ancestry []*audit.Record `json:"ancestry"`
	// Tree is the entire cascade that shares the target's correlation_id, rooted
	// at the correlation root — the full upstream-and-sibling lineage.
	Tree *Node `json:"tree"`
}

// Reconstruct builds the lineage for eventID. It is two lookups against the
// store — the causal ancestry (Get-walk up causation_id) and the correlation
// cascade (one Query) — so it stays well under the AUDIT-01e <1min budget even
// for large cascades (map-keyed, no scan).
func Reconstruct(ctx context.Context, store audit.Store, tenant, eventID string) (*Lineage, error) {
	target, ok, err := store.Get(ctx, eventID)
	if err != nil {
		return nil, err
	}
	// SCOPED, and structurally rather than at the handler: audit_log is not
	// RLS'd, so the store answers for every tenant. An event belonging to
	// someone else is ErrNotFound, never "forbidden" — on a cross-tenant
	// boundary, forbidden confirms the record exists.
	if !ok || target.TenantID != tenant {
		return nil, ErrNotFound
	}

	ancestry, err := walkAncestry(ctx, store, tenant, target)
	if err != nil {
		return nil, err
	}

	tree, err := buildTree(ctx, store, tenant, target.CorrelationID, target.EventID)
	if err != nil {
		return nil, err
	}

	return &Lineage{Target: target, Ancestry: ancestry, Tree: tree}, nil
}

// walkAncestry follows causation_id from target up to the root, returning the
// chain root-first. A missing or empty causation_id ends the walk (root reached,
// or the cause predates the audit log). A cycle (malformed lineage) is broken by
// the visited set rather than looping forever.
func walkAncestry(ctx context.Context, store audit.Store, tenant string, target *audit.Record) ([]*audit.Record, error) {
	var rev []*audit.Record
	visited := map[string]bool{}
	cur := target
	for cur != nil && !visited[cur.EventID] {
		visited[cur.EventID] = true
		rev = append(rev, cur)
		if cur.CausationID == "" {
			break
		}
		parent, ok, err := store.Get(ctx, cur.CausationID)
		if err != nil {
			return nil, err
		}
		// A parent in ANOTHER tenant terminates the walk exactly like a missing
		// one: the ancestry is best-effort to the edge of what this caller may
		// see, and a causation link that crosses tenants must not become a
		// read of the other side.
		if !ok || parent.TenantID != tenant {
			break // cause outside the audit window (or outside this tenant)
		}
		cur = parent
	}
	// reverse to root-first
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev, nil
}

// buildTree assembles the causation tree over every record sharing correlationID.
// Each record links to its causation parent within the set; records whose parent
// is absent (the correlation root, or causes outside the set) become tree roots.
// With one true root the result is a single tree; defensively, multiple roots are
// wrapped under a synthetic node so nothing is dropped.
func buildTree(ctx context.Context, store audit.Store, tenant, correlationID, _ string) (*Node, error) {
	recs, err := store.Query(ctx, audit.Filter{Correlation: correlationID, Tenant: tenant})
	if err != nil {
		return nil, err
	}
	nodes := make(map[string]*Node, len(recs))
	for _, r := range recs {
		nodes[r.EventID] = &Node{Record: r}
	}
	var roots []*Node
	for _, r := range recs {
		n := nodes[r.EventID]
		if parent, ok := nodes[r.CausationID]; ok && r.CausationID != "" {
			parent.Children = append(parent.Children, n)
		} else {
			roots = append(roots, n)
		}
	}
	switch len(roots) {
	case 0:
		return nil, nil
	case 1:
		return roots[0], nil
	default:
		return &Node{Children: roots}, nil // synthetic holder; no record dropped
	}
}
