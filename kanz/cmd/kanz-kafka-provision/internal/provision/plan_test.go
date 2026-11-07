package provision

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSupportedClusterIdentityGrammarKeepsExactEstate(t *testing.T) {
	for _, id := range []string{"cluster-a", "Ab_09-x", "redpanda.12345678-1234-1234-1234-123456789abc", "redpanda.safe_custom-1"} {
		p, err := NewPlan([]byte(testManifest), manifestDigest(testManifest), "acme", "User:spiffe://kanz.internal/ns/tenant-acme/sa/risk-engine", id)
		if err != nil || p.ClusterID != id {
			t.Fatal("supported exact identity refused")
		}
		api := &fakeAdmin{clusterID: id, topics: map[string]Topic{}}
		if _, err = Run(context.Background(), api, p, "plan"); err != nil {
			t.Fatal("exact observed estate refused")
		}
		api.clusterID = id + "-other"
		if _, err = Run(context.Background(), api, p, "apply"); !errors.Is(err, ErrConflict) || api.creates != 0 || api.grants != 0 {
			t.Fatal("different estate admitted")
		}
	}
	for _, id := range []string{"", "redpanda.", "redpanda.initializing", "redpanda.unknown", "redpanda.a.b", "other.cluster", "redpanda.x\n", "redpanda.$(id)", strings.Repeat("x", 129)} {
		if ValidClusterID(id) {
			t.Fatal("unsupported identity accepted")
		}
		p := testPlan(t)
		p.ClusterID = id
		api := &fakeAdmin{clusterID: id, topics: map[string]Topic{}}
		if _, err := Run(context.Background(), api, p, "apply"); !errors.Is(err, ErrInput) || api.metadataCalls != 0 {
			t.Fatal("invalid direct plan reached broker")
		}
	}
}

func TestPlanBindsBothExistingTenantPrincipalShapes(t *testing.T) {
	for _, principal := range []string{
		"User:spiffe://kanz.internal/ns/tenant-acme/sa/risk-engine",
		"User:spiffe://kanz.internal/ns/kanz-services/sa/archiver-acme",
	} {
		p, err := NewPlan([]byte(testManifest), manifestDigest(testManifest), "acme", principal, "cluster-a")
		if err != nil || len(p.ACLs) != 6 {
			t.Fatal("existing principal rejected")
		}
		for _, acl := range p.ACLs {
			if acl.Principal != principal || acl.ResourceName != "acme." {
				t.Fatal("ACL escapes tenant or principal")
			}
		}
	}
	for _, principal := range []string{
		"User:spiffe://kanz.internal/ns/tenant-other/sa/risk-engine",
		"User:spiffe://kanz.internal/ns/kanz-services/sa/archiver-other",
		"User:spiffe://kanz.internal/ns/kanz-services/sa/oms-acme",
		"User:spiffe://other.internal/ns/tenant-acme/sa/risk-engine",
		"User:*", "User:ANONYMOUS",
	} {
		if _, err := NewPlan([]byte(testManifest), manifestDigest(testManifest), "acme", principal, "cluster-a"); err == nil {
			t.Fatal("unbound principal accepted")
		}
	}
}
