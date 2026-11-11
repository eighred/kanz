package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const d1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
const d2 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPinRewritesTagsToDigests(t *testing.T) {
	infra := t.TempDir()
	p := write(t, infra, "deploy/oms.yaml", "spec:\n  containers:\n    - image: ghcr.io/eighred/oms:latest\n")
	res, err := Pin(os.DirFS(infra), infra, "eighred", map[string]string{"oms": d1})
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if res.Rewritten != 1 {
		t.Fatalf("Rewritten = %d, want 1", res.Rewritten)
	}
	got, _ := os.ReadFile(p)
	if want := "image: ghcr.io/eighred/oms@" + d1; !strings.Contains(string(got), want) {
		t.Errorf("file = %q, want it to contain %q", got, want)
	}
	if strings.Contains(string(got), ":latest") {
		t.Errorf("a mutable tag survived the rewrite: %q", got)
	}
}

// A service with no published digest must be REPORTED, not silently skipped —
// silence is how 23 services once had no signed image and nobody noticed.
func TestPinReportsServicesWithNoDigest(t *testing.T) {
	infra := t.TempDir()
	write(t, infra, "deploy/two.yaml",
		"a:\n  - image: ghcr.io/eighred/oms:latest\nb:\n  - image: ghcr.io/eighred/audit:latest\n")
	res, err := Pin(os.DirFS(infra), infra, "eighred", map[string]string{"oms": d1})
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if res.Rewritten != 1 {
		t.Fatalf("Rewritten = %d, want 1", res.Rewritten)
	}
	if len(res.Unpinned) != 1 || res.Unpinned[0] != "audit" {
		t.Fatalf("Unpinned = %v, want [audit]", res.Unpinned)
	}
	body, _ := os.ReadFile(filepath.Join(infra, "deploy/two.yaml"))
	if !strings.Contains(string(body), "ghcr.io/eighred/audit:latest") {
		t.Error("audit should have been left untouched, not half-rewritten")
	}
}

// Re-releasing must MOVE an existing pin forward. If it skipped already-pinned
// references, the second release would ship new images that nothing deploys.
func TestPinMovesAnExistingDigestForward(t *testing.T) {
	infra := t.TempDir()
	write(t, infra, "deploy/oms.yaml", "image: ghcr.io/eighred/oms@"+d1+"\n")
	res, err := Pin(os.DirFS(infra), infra, "eighred", map[string]string{"oms": d2})
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if res.Rewritten != 1 {
		t.Fatalf("Rewritten = %d, want 1", res.Rewritten)
	}
	body, _ := os.ReadFile(filepath.Join(infra, "deploy/oms.yaml"))
	if !strings.Contains(string(body), d2) || strings.Contains(string(body), d1) {
		t.Errorf("digest was not moved forward: %q", body)
	}
}

// Third-party images must never be touched: spiffe and gitleaks are consumed,
// not built here, and no digest is ever published for them.
func TestPinLeavesThirdPartyImagesAlone(t *testing.T) {
	infra := t.TempDir()
	p := write(t, infra, "spire.yaml",
		"a:\n  - image: ghcr.io/spiffe/spire-agent:1.9.6\nb:\n  - image: ghcr.io/eighred/oms:latest\n")
	res, err := Pin(os.DirFS(infra), infra, "eighred", map[string]string{"oms": d1, "spire-agent": d2})
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if res.Rewritten != 1 {
		t.Fatalf("Rewritten = %d, want 1 (only the eighred image)", res.Rewritten)
	}
	body, _ := os.ReadFile(p)
	if !strings.Contains(string(body), "ghcr.io/spiffe/spire-agent:1.9.6") {
		t.Errorf("a third-party image was rewritten: %q", body)
	}
}

// A prefix collision must not pin the wrong service: "oms" must not match
// "oms-acme".
func TestPinDoesNotMatchAServicePrefix(t *testing.T) {
	infra := t.TempDir()
	p := write(t, infra, "acme.yaml", "image: ghcr.io/eighred/oms-acme:latest\n")
	res, err := Pin(os.DirFS(infra), infra, "eighred", map[string]string{"oms": d1})
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if res.Rewritten != 0 {
		t.Fatalf("Rewritten = %d, want 0 — 'oms' must not match 'oms-acme'", res.Rewritten)
	}
	body, _ := os.ReadFile(p)
	if !strings.Contains(string(body), "oms-acme:latest") {
		t.Errorf("oms-acme was rewritten by the oms digest: %q", body)
	}
	if len(res.Unpinned) != 1 || res.Unpinned[0] != "oms-acme" {
		t.Errorf("Unpinned = %v, want [oms-acme]", res.Unpinned)
	}
}

// The plain `image:` form is what every manifest in infra/ actually uses; the
// list-item form above is covered because both are valid YAML. Keeping a test
// per shape means a regex change cannot quietly drop one.
func TestPinHandlesThePlainImageForm(t *testing.T) {
	infra := t.TempDir()
	p := write(t, infra, "deploy/oms-deploy.yaml",
		"    spec:\n      containers:\n        - name: oms\n          image: ghcr.io/eighred/oms:latest\n")
	res, err := Pin(os.DirFS(infra), infra, "eighred", map[string]string{"oms": d1})
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if res.Rewritten != 1 {
		t.Fatalf("Rewritten = %d, want 1 — this is the shape production manifests use", res.Rewritten)
	}
	body, _ := os.ReadFile(p)
	if !strings.Contains(string(body), "image: ghcr.io/eighred/oms@"+d1) {
		t.Errorf("file = %q", body)
	}
}

// The operator launches provisioning Jobs with the image named by its
// PROVISIONER_IMAGE env var — a `value:`, not an `image:`. Pinning only `image:`
// would leave the one image the platform launches at RUNTIME on a mutable tag.
func TestPinHandlesTheEnvValueForm(t *testing.T) {
	infra := t.TempDir()
	p := write(t, infra, "deploy/operator-deploy.yaml",
		"            - name: PROVISIONER_IMAGE\n              value: ghcr.io/eighred/kanz-provisioner:latest\n")
	res, err := Pin(os.DirFS(infra), infra, "eighred", map[string]string{"kanz-provisioner": d1})
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if res.Rewritten != 1 {
		t.Fatalf("Rewritten = %d, want 1 — the operator's runtime image must be pinned too", res.Rewritten)
	}
	body, _ := os.ReadFile(p)
	if !strings.Contains(string(body), "value: ghcr.io/eighred/kanz-provisioner@"+d1) {
		t.Errorf("file = %q", body)
	}
}

// An unrelated `value:` must not be mistaken for an image reference.
func TestPinIgnoresUnrelatedValues(t *testing.T) {
	infra := t.TempDir()
	p := write(t, infra, "cm.yaml",
		"data:\n  value: some-plain-config\n  other: ghcr.io/eighred/oms\n")
	res, err := Pin(os.DirFS(infra), infra, "eighred", map[string]string{"oms": d1})
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if res.Rewritten != 0 {
		t.Fatalf("Rewritten = %d, want 0 — neither line is an image reference with a tag", res.Rewritten)
	}
	body, _ := os.ReadFile(p)
	if !strings.Contains(string(body), "value: some-plain-config") {
		t.Errorf("an unrelated value was rewritten: %q", body)
	}
}
