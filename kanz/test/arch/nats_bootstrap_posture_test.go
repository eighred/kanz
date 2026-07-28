package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// sharedConfigMapMount matches the dev Job's volumes entry mounting the
// `nats-bootstrap` ConfigMap by name, anchored to the `configMap:` volume
// source specifically. A bare substring check for "name: nats-bootstrap" is
// satisfied by the file's own metadata label
// (app.kubernetes.io/name: nats-bootstrap) regardless of what the volume
// actually mounts — proven by renaming the mounted ConfigMap and observing
// the guard still pass. Anchoring to the line immediately after `configMap:`
// ties the check to the actual mount rather than to any "nats-bootstrap"
// string anywhere in the file.
var sharedConfigMapMount = regexp.MustCompile(`configMap:\s*\n\s*name:\s*nats-bootstrap\s*(\n|$)`)

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

	// Every needle below must test actual YAML configuration, not prose. Both
	// files carry extensive comments that legitimately narrate the very tokens
	// being asserted on (the dev file explains why NATS_CERT/NATS_KEY/NATS_CA
	// are absent; the prod file explains what spiffe-helper does) — so scanning
	// raw file text makes a comment indistinguishable from real config. This
	// guard used to test raw `dev` with an exemption clause for exactly one such
	// comment, and because that comment is always present, the exemption was
	// permanently satisfied and every dev-posture needle below was dead: see
	// stripYAMLComments. Stripped text is used everywhere a needle's own prose
	// could otherwise satisfy or defeat the check.
	prodConfig := stripYAMLComments(prod)
	devConfig := stripYAMLComments(dev)

	// --- the production Job must keep its identity -------------------------
	for _, want := range []struct{ needle, why string }{
		{"initContainers:", "the spiffe-helper init container is what materialises the SVID"},
		// `- name: spiffe-helper`, not bare `spiffe-helper`. The bare token also
		// appears in the ConfigMap resource name `nats-bootstrap-spiffe-helper`,
		// which survives comment-stripping and is NOT the init container. So the
		// bare needle stayed satisfied when the container itself was deleted —
		// green guard, no SVID, which is the one failure this file exists to
		// catch. Anchoring on the list-item declaration ties the assertion to the
		// container and nothing else.
		{"- name: spiffe-helper", "without the helper the nats CLI has no identity to present"},
		{"NATS_CERT", "the CLI reads its client cert from this env var"},
		{"NATS_KEY", "the CLI reads its client key from this env var"},
		{"NATS_CA", "the CLI verifies the broker against this bundle"},
	} {
		if !strings.Contains(prodConfig, want.needle) {
			t.Errorf("bootstrap-job.yaml no longer contains %q — %s.\n\n"+
				"If this was removed to get past an Init:0/1 hang on a cluster without SPIRE, "+
				"that is what bootstrap-job-dev-plaintext.yaml is for. Stripping mTLS from the "+
				"REAL Job removes the identity the broker maps to a __system__ user, and the "+
				"change reads like a bug fix.", want.needle, want.why)
		}
	}

	// --- the dev Job must NOT quietly become a second production Job -------
	//
	// Previously: `strings.Contains(dev, forbidden) && !strings.Contains(dev,
	// "# NATS_CERT / NATS_KEY / NATS_CA are deliberately ABSENT")`. That exemption
	// comment is always present in the dev manifest (it is the documentation of
	// this very posture), so the second half of that condition was permanently
	// false and NONE of NATS_CERT, NATS_KEY, NATS_CA, or spiffe-helper could ever
	// trip this check — proven by adding a real NATS_CERT env var to the dev file
	// and observing the test still pass. Scanning comment-stripped text removes
	// the need for an exemption clause at all: the comment that used to defeat
	// the check is gone before the check runs, so it can only fire on real config.
	for _, forbidden := range []string{"NATS_CERT", "NATS_KEY", "NATS_CA", "spiffe-helper"} {
		if strings.Contains(devConfig, forbidden) {
			t.Errorf("bootstrap-job-dev-plaintext.yaml contains %q. It is the PLAINTEXT posture: "+
				"the nats CLI selects TLS by the PRESENCE of those env vars, so a half-configured "+
				"file gives a confusing partial-TLS failure rather than a clean plaintext connect. "+
				"If this rig now has SPIRE, apply bootstrap-job.yaml instead. (This check was "+
				"previously disabled by a comment-exemption clause; if you are seeing this fail for "+
				"the first time, that is why — it is now effective.)", forbidden)
		}
	}

	// --- both must run the SAME script, not two copies ---------------------
	// This is the property that keeps a dev rig's stream topology identical to
	// production's. A dev file with its own inline script would pass every other
	// check here and still drift. Scanning stripped text, since this must test
	// whether the ConfigMap is actually mounted, not whether it is mentioned in
	// prose (the file's header comments talk about the shared ConfigMap at length).
	if !sharedConfigMapMount.MatchString(devConfig) {
		t.Error("bootstrap-job-dev-plaintext.yaml does not mount the shared `nats-bootstrap` " +
			"ConfigMap. Both Jobs must run the SAME bootstrap script: that is the only reason a " +
			"second manifest is acceptable, because it means there is one stream topology rather " +
			"than two that can drift apart. (A bare substring check here was previously satisfied " +
			"by the file's own app.kubernetes.io/name: nats-bootstrap label regardless of what the " +
			"volume actually mounts — this check is now anchored to the configMap: volume source.)")
	}
	if strings.Contains(devConfig, "ensure_stream ") {
		t.Error("bootstrap-job-dev-plaintext.yaml appears to carry its own copy of the bootstrap " +
			"script (it contains `ensure_stream `). It must mount the shared ConfigMap instead — " +
			"a duplicated topology is the defect infra/kafka/tenancy.yaml already demonstrated, " +
			"where a second copy silently drifted and was found by an outage.")
	}

	// --- the two must not collide -----------------------------------------
	// Scanning raw `dev` here is correct, not an oversight: "nats-bootstrap-dev-
	// plaintext" appears exactly once in the file (the Job's own metadata.name)
	// and nowhere in a comment, so there is nothing for stripping to change.
	if !strings.Contains(dev, "name: nats-bootstrap-dev-plaintext") {
		t.Error("the dev Job must be named nats-bootstrap-dev-plaintext, distinctly from the real " +
			"one, so it cannot replace it in a kubectl apply and is obvious in a `get jobs` listing")
	}
}

// stripYAMLComments removes every `#` comment so a guard scans CONFIGURATION
// rather than prose. This file previously carried an exemption clause instead —
// "unless the file also contains the comment saying these are deliberately
// absent" — and because that comment is always present in the dev manifest, the
// exemption was permanently satisfied and ALL FOUR needles below were dead. A
// guard silenced by the very comment explaining what it guards is worse than no
// guard: it reports success. Strip the prose, keep the check.
//
// IT IS QUOTE-AWARE, and that is not decoration. A '#' inside a quoted scalar
// is CONTENT, not a comment — a PromQL label matcher, a colour, a URL fragment.
// The original implementation cut at the first '#' on the line regardless, so
// such a line was truncated and everything after the quote vanished from the
// scan. That direction fails OPEN: the guard stops seeing text it is supposed
// to be checking and reports success. TestEveryObservabilityMetricExistsInGo
// scans PromQL expressions with quoted matchers, where a metric name can sit
// after a quoted '#', so it needs the distinction; the NATS needles below are
// unaffected either way, because stripping less can only ever reveal more.
func stripYAMLComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		var inSingle, inDouble bool
		cut := -1
		for i, r := range line {
			switch r {
			case '\'':
				if !inDouble {
					inSingle = !inSingle
				}
			case '"':
				if !inSingle {
					inDouble = !inDouble
				}
			case '#':
				if !inSingle && !inDouble {
					cut = i
				}
			}
			if cut >= 0 {
				break
			}
		}
		if cut >= 0 {
			line = line[:cut]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
