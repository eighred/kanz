package arch

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #1413: freeze the previously deployed bytes while publishing source support.
// This deliberately strict transition guard prevents a partial pod-template or
// image promotion. Remove it AND the named exception together when a verified
// image and socket configuration are promoted; normal SPIFFE wiring checks must
// cover that final state. These hashes prove staging only, not deployability.
const lakeSinkBeforeIdentitySHA = "be1afd1dd77fb22b79297360e9b6ed58473b83f12d217b489d03ba31e5b971fc"
const lakeSinkIdentityPatchSHA = "1700a25214a603a2db4e4c4b1d702cd9765cf84250162882d594e7eeaf703c7d"

func checkLakeSinkStagingBytes(active, patch []byte) error {
	for _, entry := range []struct {
		name string
		data []byte
		want string
	}{
		{"active deployment", active, lakeSinkBeforeIdentitySHA},
		{"staged identity patch", patch, lakeSinkIdentityPatchSHA},
	} {
		normalized := strings.ReplaceAll(string(entry.data), "\r\n", "\n")
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(normalized))); got != entry.want {
			return fmt.Errorf("lake-sink %s changed during source-first staging (#1413): promote verified image+config together and retire the staging exception", entry.name)
		}
	}
	return nil
}

func assertLakeSinkIdentityStaged(t *testing.T, root string, want []string, current spiffeWiring) {
	t.Helper()
	if err := checkLakeSinkStagingWiring(want, current); err != nil {
		t.Fatal(err)
	}
	active, err := os.ReadFile(filepath.Join(root, "infra/deploy/lake-sink-deploy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	patchPath := filepath.Join(root, "services/lake-sink/broker-identity-patch.yaml")
	patch, err := os.ReadFile(patchPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkLakeSinkStagingBytes(active, patch); err != nil {
		t.Fatal(err)
	}
	docs := decodeWorkloadDocs(t, patch, patchPath)
	if len(docs) != 1 || docs[0].app != "lake-sink" || !anyOf(want, docs[0].spiffeEnvNames) || !docs[0].hasSpiffeCSI {
		t.Fatal("staged lake-sink patch does not supply the configured SPIFFE environment and CSI volume")
	}
}

func checkLakeSinkStagingWiring(want []string, current spiffeWiring) error {
	if len(want) != 1 || want[0] != "SPIFFE_ENDPOINT_SOCKET" || len(current.envNames) != 0 || current.hasCSIVolume {
		return fmt.Errorf("lake-sink staging exception no longer matches the config reader and unwired active deployment; retire it with the verified image/config promotion (#1413)")
	}
	return nil
}

func TestLakeSinkIdentityStagingRequiresItsConfigReader(t *testing.T) {
	if err := checkLakeSinkStagingWiring([]string{"SPIFFE_ENDPOINT_SOCKET"}, spiffeWiring{}); err != nil {
		t.Fatal(err)
	}
	if err := checkLakeSinkStagingWiring(nil, spiffeWiring{}); err == nil {
		t.Fatal("deleted config read left a dead staging exception")
	}
}

func TestLakeSinkIdentityStagingRefusesPartialPromotion(t *testing.T) {
	root := moduleRoot(t)
	active, err := os.ReadFile(filepath.Join(root, "infra/deploy/lake-sink-deploy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	patch, err := os.ReadFile(filepath.Join(root, "services/lake-sink/broker-identity-patch.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkLakeSinkStagingBytes(active, patch); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		active, patch []byte
	}{
		{"image only", []byte(strings.Replace(string(active), "sha256:054cc", "sha256:154cc", 1)), patch},
		{"pod config only", []byte(strings.Replace(string(active), "LAKE_SINK_LISTEN", "SPIFFE_ENDPOINT_SOCKET", 1)), patch},
		{"staged socket mismatch", active, []byte(strings.Replace(string(patch), "SPIFFE_ENDPOINT_SOCKET", "WRONG_SOCKET", 1))},
		{"staged CSI removed", active, []byte(strings.Replace(string(patch), "csi.spiffe.io", "wrong.driver", 1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkLakeSinkStagingBytes(tc.active, tc.patch); err == nil {
				t.Fatal("partial or mismatched promotion passed")
			}
		})
	}
}
