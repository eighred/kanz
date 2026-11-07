package provision

import "testing"

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
