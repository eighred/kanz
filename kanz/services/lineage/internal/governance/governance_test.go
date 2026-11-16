package governance_test

import (
	"context"
	"sync"
	"testing"
	"time"

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
	d, sens := g.CheckAccess(context.Background(), viewer, ownedInfo(t, "acme", publicDS, "risk.v1.ExposureSet:1"))
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
		d, _ := g.CheckAccess(context.Background(), nil, ownedInfo(t, "acme", ds, "risk.v1.ExposureSet:1"))
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
	d, sens := g.CheckAccess(ctx, viewer, ownedInfo(t, "acme", piiDS, ""))
	if d.Allow || sens != governance.SensitivityPII {
		t.Errorf("viewer PII access = allow:%v, want deny", d.Allow)
	}
	if rec.count() != 1 {
		t.Fatalf("PII deny logged %d, want 1", rec.count())
	}

	// Granted: the steward role carries lineage.pii.read — allowed, also logged.
	d, _ = g.CheckAccess(ctx, steward, ownedInfo(t, "acme", piiDS, ""))
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

func ownedInfo(t *testing.T, owner graph.Owner, ds graph.DatasetID, schema string) graph.Info {
	t.Helper()
	if schema == "" {
		schema = "fixture.v1.Record:1"
	}
	g, err := graph.NewMemory(graph.Limits{EventIndexMax: 8, DatasetMax: 8, EdgeMax: 8, StringBytesMax: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Observe(graph.Observation{Owner: owner, EventID: "fixture", Dataset: ds, Domain: "fixture", SchemaRef: schema,
		At: time.Unix(1, 0), Source: "fixture", CorrelationID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	info, ok := g.Get(owner, ds)
	if !ok {
		t.Fatal("owned record missing")
	}
	return info
}

type allowEverything struct {
	calls   int
	request auth.Request
}

func (a *allowEverything) Authorize(_ context.Context, r auth.Request) auth.Decision {
	a.calls++
	a.request = r
	return auth.Decision{Allow: true}
}
func TestStoredOwnershipPrecedesPublicClassificationAndWildcardRoles(t *testing.T) {
	info := ownedInfo(t, "foreign", publicDS, "risk.v1.ExposureSet:1")
	for name, p := range map[string]*auth.Principal{
		"missing": nil, "empty-tenant": {Subject: "u", Roles: []string{"admin"}},
		"foreign-admin": {Subject: "u", Tenant: "acme", Roles: []string{"admin"}},
	} {
		t.Run(name, func(t *testing.T) {
			authz := &allowEverything{}
			// nil classifier discriminates ordering: an isolation denial must
			// return before attempting even the public/PII classification.
			gov := governance.NewGovernor(nil, authz)
			d, sens := gov.CheckAccess(context.Background(), p, info)
			if d.Allow || sens != governance.SensitivityUnknown || authz.calls != 0 {
				t.Fatal("ownership was bypassed by public/wildcard authority")
			}
		})
	}
	gov := governance.NewGovernor(nil, &allowEverything{})
	forged := graph.Info{ID: publicDS, SchemaRef: "risk.v1.ExposureSet:1"}
	d, _ := gov.CheckAccess(context.Background(), viewer, forged)
	if d.Allow || d.Code != auth.DenyResourceTenantUnresolved {
		t.Fatal("caller-constructed record fabricated ownership")
	}
}
func TestAuthorizerReceivesTheStoredResourceOwner(t *testing.T) {
	authz := &allowEverything{}
	gov := governance.NewGovernor(governance.NewClassifier(&governance.Config{Datasets: []string{"PersonProfile"}}), authz)
	info := ownedInfo(t, "acme", piiDS, "customer.v1.PersonProfile:1")
	if d, _ := gov.CheckAccess(context.Background(), steward, info); !d.Allow {
		t.Fatal("owned authorized read refused")
	}
	if authz.calls != 1 || authz.request.Resource.Tenant != string(info.Owner()) || authz.request.Resource.ID != piiDS.String() {
		t.Fatal("stored resource identity was not delegated")
	}
}
