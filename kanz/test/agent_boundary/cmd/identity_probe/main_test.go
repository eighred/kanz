package main

import "testing"

func TestFixtureAddressRejectsAmbientAndPublicEndpoints(t *testing.T) {
	if !privateAddress("tls://10.96.0.2:4222") {
		t.Fatal("private owned service IP rejected")
	}
	for _, address := range []string{
		"nats://10.96.0.2:4222", "tls://127.0.0.1:4222", "tls://localhost:4222",
		"tls://8.8.8.8:4222", "tls://user:pass@10.96.0.2:4222", "tls://10.96.0.2:8222",
		"tls://10.96.0.2:4222/path", "tls://10.96.0.2:4222?token=fixture", "tls://10.96.0.2:4222#fragment",
	} {
		if privateAddress(address) {
			t.Errorf("accepted non-fixture address %q", address)
		}
	}
}

func TestFixtureRejectsUnexpectedIdentityOrModeBeforeWorkloadAPI(t *testing.T) {
	for _, item := range []struct{ mode, namespace, sa string }{
		{"bootstrap", "production", "copilot"}, {"bootstrap", "kanz-services", "copilot"},
		{"denied", "kanz-messaging", "nats-bootstrap"}, {"unregistered", "kanz-services", "copilot"},
		{"unknown", "kanz-services", "copilot"},
	} {
		out := result{Cases: map[string]bool{}}
		if err := run(item.mode, "tls://10.96.0.2:4222", item.namespace, item.sa, &out); err == nil || err.Error() != "unknown fixture identity" {
			t.Errorf("unexpected identity/mode reached the Workload API: %v", err)
		}
	}
}
