package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A SERVICE'S MANIFEST MUST SET THE SPIFFE ENV VAR ITS CODE ACTUALLY READS.
//
// This is the defect family that cost the most on this branch, and every instance
// looked fine in review:
//
//   - compliance, tv-sync, webhook-ingest read SPIFFE_ENDPOINT_SOCKET and their
//     manifests set NOTHING and mounted no volume.
//   - archiver, the same, and it was the only one with neither.
//   - copilot reads COPILOT_SPIFFE_SOCKET; its manifest set SPIFFE_ENDPOINT_SOCKET.
//   - risk-engine reads RISK_ENGINE_SPIFFE_SOCKET; its manifest set the same
//     unprefixed spelling.
//
// The last two are the dangerous shape: the variable is PRESENT AND WRONG. A
// reviewer sees a SPIFFE line and a mounted csi.spiffe.io volume and ticks it off,
// while the process reads an empty string. Nothing fails at startup, because an
// absent socket is a legal configuration meaning "no mesh" — so the service simply
// runs without mTLS. Against the mTLS-only broker it cannot connect at all; a
// server-side listener would serve plaintext and say so only in a log line.
//
// Two spellings exist in this estate on purpose (api-gateway and friends prefix the
// name), and that is fine — what is not fine is a manifest and its binary disagreeing
// about which one. The guard derives BOTH sides rather than pinning a list: the env
// name comes from the service's own config package, the wiring from its own manifest.
// A new service is covered the day it is added.
func TestEveryServiceIsWiredToTheSpiffeSocketItReads(t *testing.T) {
	root := moduleRoot(t)
	reads := spiffeEnvByService(t, root)
	if len(reads) == 0 {
		t.Fatal("no service config was found to read a SPIFFE env var. Either the layout moved " +
			"or the pattern stopped matching — this guard is now asserting nothing.")
	}

	manifests := spiffeWiringByApp(t, root)
	checked := 0
	for _, svc := range sortedKeys(reads) {
		wiring, ok := manifests[svc]
		if !ok {
			// Not every service has a manifest in infra/deploy (some are libraries or
			// are deployed elsewhere). Silence here is correct, not a pass.
			continue
		}
		checked++
		want := reads[svc]
		if !anyOf(want, wiring.envNames) {
			t.Errorf("%s reads %v but its manifest sets %v.\n"+
				"    A SPIFFE variable that is PRESENT AND WRONG is worse than an absent one: the "+
				"process reads an empty socket, treats it as \"no mesh\", and runs WITHOUT mTLS — "+
				"so it cannot reach the mTLS-only broker, and any listener it serves is plaintext. "+
				"Nothing fails at startup and the manifest looks correct in review.",
				svc, want, orNone(wiring.envNames))
		}
		if !wiring.hasCSIVolume {
			t.Errorf("%s reads a SPIFFE socket but its manifest mounts no csi.spiffe.io volume — "+
				"the env var names a path that will not exist, which is the same outcome as not "+
				"setting it at all.", svc)
		}
	}
	if checked == 0 {
		t.Fatal("no service was matched to a manifest. The app-label or path convention changed " +
			"and this guard silently checked nothing.")
	}
}

type spiffeWiring struct {
	envNames     []string
	hasCSIVolume bool
}

var spiffeEnvRe = regexp.MustCompile(`"([A-Z_]*SPIFFE[A-Z_]*)"`)

// spiffeEnvByService reads each service's config package for the SPIFFE env name it
// looks up. Parsed from the source rather than listed here: a list would be a second
// place to update, and would drift exactly like the manifests it is meant to check.
func spiffeEnvByService(t *testing.T, root string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	servicesDir := filepath.Join(root, "services")
	entries, err := os.ReadDir(servicesDir)
	if err != nil {
		t.Fatalf("read %s: %v", servicesDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		cfgDir := filepath.Join(servicesDir, e.Name(), "internal", "config")
		files, err := os.ReadDir(cfgDir)
		if err != nil {
			continue // no config package
		}
		seen := map[string]bool{}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
				continue
			}
			body, err := os.ReadFile(filepath.Join(cfgDir, f.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", f.Name(), err)
			}
			for _, m := range spiffeEnvRe.FindAllStringSubmatch(string(body), -1) {
				seen[m[1]] = true
			}
		}
		if len(seen) > 0 {
			out[e.Name()] = sortedKeys(seen)
		}
	}
	return out
}

// spiffeWiringByApp reads infra/deploy for what each workload actually sets and
// mounts, keyed by its `app` label.
func spiffeWiringByApp(t *testing.T, root string) map[string]spiffeWiring {
	t.Helper()
	dir := filepath.Join(root, "infra", "deploy")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string]spiffeWiring{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, d := range decodeWorkloadDocs(t, body, e.Name()) {
			if d.app == "" {
				continue
			}
			w := out[d.app]
			w.envNames = append(w.envNames, d.spiffeEnvNames...)
			w.hasCSIVolume = w.hasCSIVolume || d.hasSpiffeCSI
			out[d.app] = w
		}
	}
	return out
}

func anyOf(want, got []string) bool {
	for _, w := range want {
		for _, g := range got {
			if w == g {
				return true
			}
		}
	}
	return false
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "NOTHING"
	}
	return strings.Join(s, ",")
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
