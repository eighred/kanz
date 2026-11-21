package tenantgen

import (
	"bytes"
	"testing"
)

func TestRegulatoryProducerRequiresExplicitSelection(t *testing.T) {
	for _, service := range Services {
		if service.Name == "regulatory" {
			t.Fatal("filing source candidate became a default tenant workload")
		}
	}
	service, ok := ServiceByName("regulatory")
	if !ok || service.TenantEnv != "REGULATORY_TENANT" || service.KafkaProducer {
		t.Fatal("explicit filing producer contract unavailable or changed")
	}
	base, err := ReadBases("../..", service)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := Render(base, service, "acme")
	if err != nil {
		t.Fatal(err)
	}
	sa, err := ServiceAccountName(rendered)
	if err != nil || sa != "regulatory-acme" || service.ComputeSPIFFEID("acme") != "spiffe://kanz.internal/ns/kanz-services/sa/regulatory-acme" {
		t.Fatalf("rendered identity mismatch: %s %v", sa, err)
	}
	if !bytes.Contains(rendered, []byte("REGULATORY_TENANT")) || !bytes.Contains(rendered, []byte("value: acme")) || bytes.Contains(rendered, []byte("value: __system__")) {
		t.Fatal("explicit render retained platform filing scope")
	}
	if _, ok := ServiceByName("regulatory-unknown"); ok {
		t.Fatal("unknown optional role accepted")
	}
}
