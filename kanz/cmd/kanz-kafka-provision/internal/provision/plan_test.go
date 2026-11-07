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

func TestExplicitRedpandaEncodingPreservesCanonicalIdentityAndExactACLs(t *testing.T) {
	p := testPlan(t)
	const peer = "spiffe://kanz.internal/ns/kanz-messaging/sa/redpanda"
	const wire = `User:spiffe:\/\/kanz.internal\/ns\/tenant-acme\/sa\/risk-engine`
	encoded, err := WithPrincipalEncoding(p, "redpanda-rule-escaped-uri", peer)
	if err != nil || encoded.Principal != p.Principal || encoded.BrokerPrincipal != wire || p.BrokerPrincipal != p.Principal {
		t.Fatal("canonical identity or wire encoding changed")
	}
	for i, acl := range encoded.ACLs {
		want := p.ACLs[i]
		if want.Principal != p.Principal {
			t.Fatal("input plan mutated")
		}
		want.Principal = wire
		if acl != want {
			t.Fatal("encoding changed an ACL permission or resource")
		}
	}
	api := &fakeAdmin{clusterID: p.ClusterID, topics: map[string]Topic{}}
	if r, err := Run(context.Background(), api, encoded, "apply"); err != nil || !r.Complete {
		t.Fatal("encoded readback incomplete")
	}
	if api.filteredPrincipal != wire {
		t.Fatal("ACL inventory did not use exact wire principal")
	}
	api.acls[0].Principal = p.Principal
	if _, err := Run(context.Background(), api, encoded, "verify"); !errors.Is(err, ErrConflict) {
		t.Fatal("mixed encoding accepted")
	}
	for _, pair := range [][2]string{{"", peer}, {"unknown", peer}, {"redpanda-rule-escaped-uri", "spiffe://kanz.internal/ns/kanz-messaging/sa/kafka"}} {
		if _, err := WithPrincipalEncoding(p, pair[0], pair[1]); !errors.Is(err, ErrInput) {
			t.Fatal("unbound encoding accepted")
		}
	}
	plain, err := WithPrincipalEncoding(p, "spiffe-uri", peer)
	if err != nil || plain.BrokerPrincipal != p.Principal {
		t.Fatal("default wire encoding changed")
	}
}
