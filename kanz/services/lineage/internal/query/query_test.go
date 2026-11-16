package query_test

import (
	"bytes"
	"context"
	"encoding/json"
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
	g := newMemory(t)
	now := time.Now()
	observe(t, g, "e1", piiDS, "customer", "customer.v1.PersonProfile:1", now, "")
	observe(t, g, "e2", derived, "risk", "risk.v1.CustomerExposure:1", now, "e1")

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

	u, bound := svc.Explain(err)
	if !bound {
		t.Fatal("miss has no scoped coverage")
	}
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
	g := newMemory(t, graph.WithCompleteHistory("acme"))
	observe(t, g, "e1", piiDS, "customer", "customer.v1.PersonProfile:1", time.Now(), "")
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

func newMemory(t *testing.T, opts ...graph.Option) *graph.Memory {
	t.Helper()
	g, err := graph.NewMemory(graph.Limits{EventIndexMax: graph.DefaultEventIndexCapacity, DatasetMax: 128, EdgeMax: 512, StringBytesMax: 64 << 20}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return g
}
func observe(t *testing.T, g *graph.Memory, id string, ds graph.DatasetID, domain, schema string, at time.Time, cause string) graph.ObservationSnapshot {
	t.Helper()
	if schema == "" {
		schema = "fixture.v1." + ds.Name + ":1"
	}
	snapshot, err := g.Observe(graph.Observation{Owner: "acme", EventID: id, Dataset: ds, Domain: domain, SchemaRef: schema,
		At: at, ParentEventID: cause, Source: domain, CorrelationID: "corr-" + id})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func putOwned(t *testing.T, g *graph.Memory, owner graph.Owner, id string, ds graph.DatasetID, schema string) graph.Info {
	t.Helper()
	_, err := g.Observe(graph.Observation{Owner: owner, EventID: id, Dataset: ds, Domain: "risk", SchemaRef: schema,
		At: time.Unix(1, 0), Source: "risk-engine", CorrelationID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	info, ok := g.Get(owner, ds)
	if !ok {
		t.Fatal("owned fixture missing")
	}
	return info
}
func TestEqualRoleReadsUseStoredOwnerAndScopedCoverage(t *testing.T) {
	g := newMemory(t)
	// Deliberate adversarial storage identity collision; canonical live FACTs
	// have globally unique UUIDs. This tests the independent storage boundary.
	putOwned(t, g, "acme", "shared", derived, "risk.v1.Acme:1")
	putOwned(t, g, "other", "shared", derived, "risk.v1.Other:1")
	putOwned(t, g, "other", "foreign", graph.DatasetID{Namespace: "kanz.risk", Name: "Foreign"}, "risk.v1.Foreign:1")
	gov := governance.NewGovernor(governance.NewClassifier(nil), auth.NewPolicyAuthorizer(&auth.Policy{}))
	svc := query.NewService(g, gov)
	for _, tc := range []struct{ owner, schema string }{{"acme", "risk.v1.Acme:1"}, {"other", "risk.v1.Other:1"}} {
		p := &auth.Principal{Subject: "equal-user", Tenant: tc.owner, Roles: []string{"viewer"}}
		prov, err := svc.ForEvent(context.Background(), p, "shared")
		if err != nil || prov.Target.SchemaRef != tc.schema {
			t.Fatalf("scoped event read collapsed: %v", err)
		}
		catalog, err := svc.Catalog(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		for _, info := range catalog.Datasets {
			if string(info.Owner()) != tc.owner {
				t.Fatal("foreign catalog record disclosed")
			}
		}
	}
	_, foreign := svc.ForEvent(context.Background(), viewer, "foreign")
	_, absent := svc.ForEvent(context.Background(), viewer, "absent")
	fu, fok := svc.Explain(foreign)
	au, aok := svc.Explain(absent)
	if !fok || !aok {
		t.Fatal("miss lacks scoped coverage")
	}
	a, _ := json.Marshal(fu)
	b, _ := json.Marshal(au)
	if !errors.Is(foreign, query.ErrNotRetained) || !bytes.Equal(a, b) {
		t.Fatal("foreign existence differed from absent scoped history")
	}
	if fu.Coverage.Retained != 1 {
		t.Fatal("foreign counts escaped scoped coverage")
	}
}

type foreignGet struct {
	graph.Graph
	record graph.Info
}

func (g foreignGet) Get(graph.Owner, graph.DatasetID) (graph.Info, bool) { return g.record, true }

type foreignUpstream struct {
	graph.Graph
	record graph.Info
}

func (g foreignUpstream) Upstream(graph.Owner, graph.DatasetID) []graph.Info {
	return []graph.Info{g.record}
}

type foreignCatalog struct {
	graph.Graph
	record graph.Info
}

func (g foreignCatalog) Datasets(graph.Owner) []graph.Info { return []graph.Info{g.record} }
func TestQueryDoesNotTrustAResolverSubstitutedOwner(t *testing.T) {
	g := newMemory(t)
	putOwned(t, g, "acme", "a", derived, "risk.v1.Acme:1")
	foreign := putOwned(t, g, "other", "b", piiDS, "customer.v1.PersonProfile:1")
	gov := governance.NewGovernor(governance.NewClassifier(nil), auth.NewPolicyAuthorizer(&auth.Policy{Roles: map[string][]auth.Action{"admin": {"*"}}}))
	admin := &auth.Principal{Subject: "u", Tenant: "acme", Roles: []string{"admin"}}
	for _, record := range []graph.Info{foreign, {ID: derived, SchemaRef: "forged"}} {
		svc := query.NewService(foreignGet{Graph: g, record: record}, gov)
		if _, err := svc.ForDataset(context.Background(), admin, derived); !errors.Is(err, query.ErrNotRetained) {
			t.Fatalf("resolver owner substitution allowed: %v", err)
		}
	}
	svc := query.NewService(foreignUpstream{Graph: g, record: foreign}, gov)
	prov, err := svc.ForDataset(context.Background(), admin, derived)
	if err != nil || len(prov.Upstream) != 0 {
		t.Fatal("foreign upstream was disclosed as a redacted named node")
	}
	catalog, err := query.NewService(foreignCatalog{Graph: g, record: foreign}, gov).Catalog(context.Background(), admin)
	if err != nil || catalog.Count != 0 {
		t.Fatal("foreign catalog resolver output disclosed")
	}
}

func TestUnboundExplanationCannotFabricateZeroCoverage(t *testing.T) {
	svc, _ := build(t)
	if _, bound := svc.Explain(query.ErrNotRetained); bound {
		t.Fatal("untyped miss fabricated scoped zero coverage")
	}
}
