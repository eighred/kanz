package query_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/lineage/internal/governance"
	"github.com/eighred/kanz/services/lineage/internal/graph"
	"github.com/eighred/kanz/services/lineage/internal/query"
)

type recordingRecorder struct {
	mu   sync.Mutex
	logs []*observationpb.DecisionLog
}

func (r *recordingRecorder) Record(_ context.Context, e *observationpb.DecisionLog) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, e)
	return nil
}
func (r *recordingRecorder) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.logs) }

var (
	piiDS   = graph.DatasetID{Namespace: "kanz.customer", Name: "PersonProfile"}
	derived = graph.DatasetID{Namespace: "kanz.risk", Name: "CustomerExposure"}
	steward = &auth.Principal{Subject: "s", Tenant: "acme", Roles: []string{"steward"}}
	viewer  = &auth.Principal{Subject: "u", Tenant: "acme", Roles: []string{"viewer"}}
)

// build a graph: PII PersonProfile (root) → public CustomerExposure (leaf).
func build(t *testing.T) (*query.Service, *recordingRecorder) {
	t.Helper()
	g := graph.NewMemory()
	now := time.Now()
	g.Observe("e1", piiDS, "customer", "customer.v1.PersonProfile:1", now, "")
	g.Observe("e2", derived, "risk", "risk.v1.CustomerExposure:1", now, "e1")

	classifier := governance.NewClassifier(&governance.Config{Datasets: []string{"PersonProfile"}})
	policy := &auth.Policy{Roles: map[string][]auth.Action{"steward": {governance.ActionPIIRead}}}
	rec := &recordingRecorder{}
	authz := auth.NewAuditedAuthorizer(auth.NewPolicyAuthorizer(policy), rec, "lineage", nil)
	return query.NewService(g, governance.NewGovernor(classifier, authz)), rec
}

// LIN-01e: an authorized steward gets the full upstream lineage, PII node intact.
func TestFullUpstreamForAuthorized(t *testing.T) {
	svc, _ := build(t)
	prov, err := svc.ForEvent(context.Background(), steward, "e2")
	if err != nil {
		t.Fatal(err)
	}
	if prov.Target.Dataset != derived {
		t.Errorf("target = %v, want CustomerExposure", prov.Target.Dataset)
	}
	if len(prov.Upstream) != 1 || prov.Upstream[0].Dataset != piiDS {
		t.Fatalf("upstream = %+v, want [PersonProfile]", prov.Upstream)
	}
	u := prov.Upstream[0]
	if u.Sensitivity != governance.SensitivityPII || u.Redacted || u.SchemaRef == "" {
		t.Errorf("authorized steward should see the PII node intact: %+v", u)
	}
}

// An unauthorized viewer still sees the topology (the leaf is public), but the
// upstream PII node is redacted — and the denied PII access is logged.
func TestUpstreamPIIRedactedForUnauthorized(t *testing.T) {
	svc, rec := build(t)
	prov, err := svc.ForEvent(context.Background(), viewer, "e2")
	if err != nil {
		t.Fatal(err)
	}
	if len(prov.Upstream) != 1 {
		t.Fatalf("upstream = %+v", prov.Upstream)
	}
	u := prov.Upstream[0]
	if !u.Redacted || u.SchemaRef != "" {
		t.Errorf("unauthorized viewer should get redacted PII node, got %+v", u)
	}
	if rec.count() == 0 {
		t.Error("PII access by viewer was not logged")
	}
}

// Querying a PII dataset directly is deny-by-default for the unauthorized — and
// the denial is logged.
func TestPIITargetForbiddenAndLogged(t *testing.T) {
	svc, rec := build(t)
	_, err := svc.ForDataset(context.Background(), viewer, piiDS)
	if !errors.Is(err, query.ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
	if rec.count() == 0 {
		t.Error("forbidden PII target access was not logged")
	}

	// The steward may query it.
	prov, err := svc.ForDataset(context.Background(), steward, piiDS)
	if err != nil {
		t.Fatalf("steward denied PII target: %v", err)
	}
	if prov.Target.Sensitivity != governance.SensitivityPII {
		t.Errorf("target sensitivity = %v, want pii", prov.Target.Sensitivity)
	}
}

// #244: THIS TEST USED TO ASSERT THE DEFECT.
//
// It required ErrNotFound for an event the bounded, restart-emptied index simply
// does not hold — which is the service asserting that no such event exists on a
// spine it has read a few minutes of. The graph built above is not declared to
// cover history, so the only honest answer is "I cannot say".
func TestUnknownEventOnABoundedIndexIsNotAFinding(t *testing.T) {
	svc, _ := build(t)
	_, err := svc.ForEvent(context.Background(), steward, "nope")
	if !errors.Is(err, query.ErrNotRetained) {
		t.Fatalf("want ErrNotRetained, got %v", err)
	}
	if errors.Is(err, query.ErrNotFound) {
		t.Error("ErrNotRetained must not satisfy errors.Is(ErrNotFound) — that is the collapse")
	}

	u := svc.Explain(graph.LookupUnknown)
	if u.Status != graph.LookupUnknown {
		t.Errorf("Explain status = %q", u.Status)
	}
	if u.Coverage.Capacity <= 0 || u.Coverage.Complete {
		t.Errorf("the refusal must carry usable coverage, got %+v", u.Coverage)
	}
}

// The conclusive answer is still reachable, and it is a different error, so a
// caller that must distinguish "no such trade" from "ask the warehouse" can.
func TestACompleteGraphStillAnswersNotFound(t *testing.T) {
	g := graph.NewMemory(graph.WithCompleteHistory())
	g.Observe("e1", piiDS, "customer", "customer.v1.PersonProfile:1", time.Now(), "")
	svc := query.NewService(g, governance.NewGovernor(
		governance.NewClassifier(nil),
		auth.NewPolicyAuthorizer(&auth.Policy{})))

	if _, err := svc.ForEvent(context.Background(), steward, "nope"); !errors.Is(err, query.ErrNotFound) {
		t.Errorf("want ErrNotFound on a complete graph, got %v", err)
	}
	if _, err := svc.ForDataset(context.Background(), steward, graph.DatasetID{
		Namespace: "kanz.nowhere", Name: "Nothing",
	}); !errors.Is(err, query.ErrNotFound) {
		t.Errorf("dataset miss on a complete graph: want ErrNotFound, got %v", err)
	}
}

// A dataset miss gets the same treatment. The dataset table is never evicted,
// but a restarted pod holds none of it either, so the miss is still not a fact
// about the estate.
func TestUnknownDatasetOnABoundedIndexIsNotAFinding(t *testing.T) {
	svc, _ := build(t)
	_, err := svc.ForDataset(context.Background(), steward, graph.DatasetID{
		Namespace: "kanz.nowhere", Name: "Nothing",
	})
	if !errors.Is(err, query.ErrNotRetained) {
		t.Errorf("want ErrNotRetained, got %v", err)
	}
}

// A successful answer carries the coverage too, because an eviction can shorten
// the upstream list without emptying it.
func TestASuccessfulAnswerDeclaresItsCoverage(t *testing.T) {
	svc, _ := build(t)
	prov, err := svc.ForEvent(context.Background(), steward, "e2")
	if err != nil {
		t.Fatal(err)
	}
	if prov.Status != graph.LookupRetained {
		t.Errorf("status = %q, want %q", prov.Status, graph.LookupRetained)
	}
	if prov.Coverage.Capacity != graph.DefaultEventIndexCapacity {
		t.Errorf("coverage.capacity = %d, want the default cap", prov.Coverage.Capacity)
	}
	if prov.Coverage.Since.IsZero() {
		t.Error("coverage.since is zero — a caller cannot tell how far back this answer reaches")
	}
}
