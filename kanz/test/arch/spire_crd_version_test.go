package arch

import (
	"path/filepath"
	"testing"
)

// registration.yaml declares a ClusterSPIFFEID custom resource. Until this
// task, no CustomResourceDefinition for that kind was ever installed on a
// cluster, so `kubectl apply -f registration.yaml` failed on a fresh rig with
// `no matches for kind "ClusterSPIFFEID"` — SPIRE could come up with zero
// registration entries and nobody applying the manifests in order would see
// anything but a stuck apply. kanz/infra/security/spire/crds/ vendors the
// fix: the CRD definitions spire-controller-manager ships in its own
// config/crd directory, fetched once at review time and committed rather
// than pulled at apply time, so the rig still rebuilds from the repository
// alone (tools/rig-apply.sh applies them, and waits for Established, before
// registration.yaml).
//
// A vendored CRD is only correct for the controller-manager binary version it
// was generated from — the schema the sidecar reconciles against and the CRD
// the API server enforces are versioned together upstream in the same repo
// and tag. This guard reads the spire-controller-manager image tag in
// spire-server.yaml as the source of truth and fails if the vendored CRD's
// recorded source tag disagrees, in EITHER direction: bumping the image
// without re-vendoring the CRD (or vice versa) is exactly the drift a rig
// cannot detect on its own, because the apply still succeeds — the schema is
// just stale.
func TestSPIREControllerManagerCRDVersionMatchesImage(t *testing.T) {
	root := moduleRoot(t)
	spireDir := filepath.Join(root, "infra", "security", "spire")

	server := readFile(t, filepath.Join(spireDir, "spire-server.yaml"))
	crd := readFile(t, filepath.Join(spireDir, "crds", "spire.spiffe.io_clusterspiffeids.yaml"))

	// Strip comments before reading the image tag: this must test the actual
	// deployed configuration, not prose that happens to mention a version.
	imageTag := mustFindOne(t, stripYAMLComments(server),
		`ghcr\.io/spiffe/spire-controller-manager:(\d+\.\d+\.\d+)`,
		"spire-server.yaml no longer pins the spire-controller-manager sidecar to a "+
			"MAJOR.MINOR.PATCH image tag")[1]

	// The vendored CRD's source tag is recorded only as a header comment —
	// there is no separate machine-readable manifest for one vendored file, by
	// design. This is the one needle in this guard that intentionally reads
	// the comment prose rather than stripped config, because the comment IS
	// where this fact lives.
	crdTag := mustFindOne(t, crd,
		`spire-controller-manager, tag v(\d+\.\d+\.\d+)`,
		"kanz/infra/security/spire/crds/spire.spiffe.io_clusterspiffeids.yaml no longer records its "+
			"source tag as '... spire-controller-manager, tag vX.Y.Z' in its header comment")[1]

	if imageTag != crdTag {
		t.Fatalf("spire-server.yaml pins the spire-controller-manager image to %s but the vendored "+
			"ClusterSPIFFEID CRD was generated from tag v%s. The CRD schema and the controller-manager "+
			"binary that reconciles against it are versioned together upstream — re-vendor "+
			"kanz/infra/security/spire/crds/spire.spiffe.io_clusterspiffeids.yaml from "+
			"https://github.com/spiffe/spire-controller-manager/tree/v%s/config/crd/bases and update "+
			"this file's header comment to match, or roll the image tag back to v%s.",
			imageTag, crdTag, imageTag, crdTag)
	}
}
