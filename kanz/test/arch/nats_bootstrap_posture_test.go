package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The NATS bootstrap exists in two postures, and the dangerous direction is not
// the one people expect.
//
//	bootstrap-job.yaml                 mTLS. spiffe-helper init container writes
//	                                   an SVID; the nats CLI presents it and the
//	                                   broker's verify_and_map maps it to a
//	                                   __system__ user (SEC-M3b).
//	bootstrap-job-dev-plaintext.yaml   no init container, no certs. For a rig
//	                                   with no SPIRE, where the mTLS Job blocks
//	                                   forever at Init:0/1 and no stream is ever
//	                                   created.
//
// The risk is NOT that someone applies the dev file to production — it is named
// to make that obvious, and a production broker requiring client certs would
// reject a plaintext CLI anyway. The risk is the quiet one: somebody hits the
// Init:0/1 hang, finds the dev file "works", and edits the REAL Job to match —
// stripping the identity that makes the spine multi-tenant-safe, in a commit
// that looks like a bug fix.
//
// So this guard protects the production posture, and separately pins the one
// property that makes having a dev variant safe at all: both Jobs mount the
// SAME script ConfigMap. The moment the dev file carries its own copy of the
// bootstrap script, the two topologies can drift — and infra/kafka/tenancy.yaml
// spent a day proving that a duplicated table drifts silently and is discovered
// by an outage.
func TestNATSBootstrapKeepsItsMTLSPosture(t *testing.T) {
	root := moduleRoot(t)
	prod := readFile(t, filepath.Join(root, "infra", "nats", "bootstrap-job.yaml"))
	dev := readFile(t, filepath.Join(root, "infra", "nats", "bootstrap-job-dev-plaintext.yaml"))

	// --- the production Job must keep its identity -------------------------
	for _, want := range []struct{ needle, why string }{
		{"initContainers:", "the spiffe-helper init container is what materialises the SVID"},
		{"spiffe-helper", "without the helper the nats CLI has no identity to present"},
		{"NATS_CERT", "the CLI reads its client cert from this env var"},
		{"NATS_KEY", "the CLI reads its client key from this env var"},
		{"NATS_CA", "the CLI verifies the broker against this bundle"},
	} {
		if !strings.Contains(prod, want.needle) {
			t.Errorf("bootstrap-job.yaml no longer contains %q — %s.\n\n"+
				"If this was removed to get past an Init:0/1 hang on a cluster without SPIRE, "+
				"that is what bootstrap-job-dev-plaintext.yaml is for. Stripping mTLS from the "+
				"REAL Job removes the identity the broker maps to a __system__ user, and the "+
				"change reads like a bug fix.", want.needle, want.why)
		}
	}

	// --- the dev Job must NOT quietly become a second production Job -------
	for _, forbidden := range []string{"NATS_CERT", "NATS_KEY", "NATS_CA", "spiffe-helper"} {
		if strings.Contains(dev, forbidden) && !strings.Contains(dev, "# NATS_CERT / NATS_KEY / NATS_CA are deliberately ABSENT") {
			t.Errorf("bootstrap-job-dev-plaintext.yaml contains %q. It is the PLAINTEXT posture: "+
				"the nats CLI selects TLS by the PRESENCE of those env vars, so a half-configured "+
				"file gives a confusing partial-TLS failure rather than a clean plaintext connect. "+
				"If this rig now has SPIRE, apply bootstrap-job.yaml instead.", forbidden)
		}
	}

	// --- both must run the SAME script, not two copies ---------------------
	// This is the property that keeps a dev rig's stream topology identical to
	// production's. A dev file with its own inline script would pass every other
	// check here and still drift.
	if !strings.Contains(dev, "configMap:") || !strings.Contains(dev, "name: nats-bootstrap") {
		t.Error("bootstrap-job-dev-plaintext.yaml does not mount the shared `nats-bootstrap` " +
			"ConfigMap. Both Jobs must run the SAME bootstrap script: that is the only reason a " +
			"second manifest is acceptable, because it means there is one stream topology rather " +
			"than two that can drift apart.")
	}
	if strings.Contains(dev, "ensure_stream ") {
		t.Error("bootstrap-job-dev-plaintext.yaml appears to carry its own copy of the bootstrap " +
			"script (it contains `ensure_stream `). It must mount the shared ConfigMap instead — " +
			"a duplicated topology is the defect infra/kafka/tenancy.yaml already demonstrated, " +
			"where a second copy silently drifted and was found by an outage.")
	}

	// --- the two must not collide -----------------------------------------
	if !strings.Contains(dev, "name: nats-bootstrap-dev-plaintext") {
		t.Error("the dev Job must be named nats-bootstrap-dev-plaintext, distinctly from the real " +
			"one, so it cannot replace it in a kubectl apply and is obvious in a `get jobs` listing")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
