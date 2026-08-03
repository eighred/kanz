package governance_test

import (
	"context"
	"sync"
	"testing"

	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/lineage/internal/governance"
	"github.com/eighred/kanz/services/lineage/internal/graph"
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

func (r *recordingRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.logs)
}

func newGovernor(t *testing.T) (*governance.Governor, *recordingRecorder) {
	t.Helper()
	classifier := governance.NewClassifier(&governance.Config{Datasets: []string{"PersonProfile"}})
	policy := &auth.Policy{Roles: map[string][]auth.Action{"steward": {governance.ActionPIIRead}}}
	rec := &recordingRecorder{}
	authz := auth.NewAuditedAuthorizer(auth.NewPolicyAuthorizer(policy), rec, "lineage", nil)
	return governance.NewGovernor(classifier, authz), rec
}

var (
	publicDS = graph.DatasetID{Namespace: "kanz.risk", Name: "ExposureSet"}
	piiDS    = graph.DatasetID{Namespace: "kanz.customer", Name: "PersonProfile"}

	steward = &auth.Principal{Subject: "s", Tenant: "acme", Roles: []string{"steward"}}
	viewer  = &auth.Principal{Subject: "u", Tenant: "acme", Roles: []string{"viewer"}}
)

func TestPublicDatasetAllowedWithoutAuthOrLog(t *testing.T) {
	g, rec := newGovernor(t)
	d, sens := g.CheckAccess(context.Background(), viewer, publicDS, "risk.v1.ExposureSet:1")
	if !d.Allow || sens != governance.SensitivityPublic {
		t.Errorf("public access = allow:%v sens:%v, want allow public", d.Allow, sens)
	}
	if rec.count() != 0 {
		t.Errorf("public access logged %d decisions, want 0 (only PII is governed/logged)", rec.count())
	}
}

// "Public" classifies the DATA, not the caller (#268). The non-PII branch used
// to return allow before it ever looked at the principal, so for as long as
// nothing on the upstream side reconstructed the gateway's headers, EVERY proxied
// read arrived with p == nil and was served the full provenance of every non-PII
// dataset in the graph — unauthenticated, and unscoped by tenant.
func TestNilPrincipalDeniedEvenOnAPublicDataset(t *testing.T) {
	g, _ := newGovernor(t)
	for name, ds := range map[string]graph.DatasetID{"public": publicDS, "pii": piiDS} {
		d, _ := g.CheckAccess(context.Background(), nil, ds, "risk.v1.ExposureSet:1")
		if d.Allow {
			t.Errorf("%s dataset served to a nil principal (%q) — nobody is not a caller with "+
				"no grants, and the two must not get the same answer", name, d.Reason)
		}
	}
}

func TestPIIAccessGovernedAndLogged(t *testing.T) {
	g, rec := newGovernor(t)
	ctx := context.Background()

	// Deny-by-default: a principal without the role is denied — and logged.
	d, sens := g.CheckAccess(ctx, viewer, piiDS, "")
	if d.Allow || sens != governance.SensitivityPII {
		t.Errorf("viewer PII access = allow:%v, want deny", d.Allow)
	}
	if rec.count() != 1 {
		t.Fatalf("PII deny logged %d, want 1", rec.count())
	}

	// Granted: the steward role carries lineage.pii.read — allowed, also logged.
	d, _ = g.CheckAccess(ctx, steward, piiDS, "")
	if !d.Allow {
		t.Errorf("steward PII access denied: %s", d.Reason)
	}
	if rec.count() != 2 {
		t.Errorf("PII allow not logged: count=%d, want 2", rec.count())
	}
}

func TestClassifyBySchemaRefSubstring(t *testing.T) {
	c := governance.NewClassifier(&governance.Config{SchemaRefSubstrings: []string{"pii.v1"}})
	if c.Classify(graph.DatasetID{Namespace: "kanz.x", Name: "Y"}, "pii.v1.Email:3") != governance.SensitivityPII {
		t.Error("schema-ref substring should classify PII")
	}
	if c.Classify(graph.DatasetID{Namespace: "kanz.x", Name: "Y"}, "risk.v1.Measures:1") != governance.SensitivityPublic {
		t.Error("non-matching ref should be public")
	}
}
