package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every ConfigMap a Deployment mounts must either be DEFINED in this repo or
// DECLARED operator-supplied.
//
// inference-deploy.yaml mounted `inference-models`, and nothing in the repo
// created it or said who should. A reviewer cannot tell an intentional
// operator-supplied input from a forgotten manifest, and the two fail very
// differently: the intentional one is a documented prerequisite, the forgotten
// one is a pod stuck in CreateContainerConfigError that never runs the process
// whose loud refusal the file advertises.
//
// This does not demand the ConfigMap be committed. Some genuinely must not be —
// `inference-models` carries model weights and validation evidence produced by
// the MLOPS-01g promotion workflow, and a placeholder in git would be a
// FABRICATED validation record satisfying the very gate that exists to refuse
// one. It demands only that the choice be STATED.

// operatorSuppliedConfigMaps are the ConfigMaps deliberately not in git. A name
// here must be accompanied, in the manifest that mounts it, by how to create it
// and what it contains.
var operatorSuppliedConfigMaps = map[string]string{
	"inference-models": "MLOPS-01g promotion output: model weights + recorded validation evidence. " +
		"Environment-specific, and a committed placeholder would be a fabricated validation record. " +
		"Creation command and schema are documented at the mount site in inference-deploy.yaml.",
}

var (
	configMapRefPattern  = regexp.MustCompile(`(?m)configMap:\s*\n\s*name:\s*([A-Za-z0-9._-]+)`)
	configMapKindPattern = regexp.MustCompile(`(?m)^kind:\s*ConfigMap\s*$`)
	metadataNamePattern  = regexp.MustCompile(`(?m)^\s{0,2}name:\s*([A-Za-z0-9._-]+)`)
)

// definedConfigMaps scans infra/ for ConfigMaps the repo actually creates.
func definedConfigMaps(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	infra := filepath.Join(root, "infra")
	err := filepath.Walk(infra, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		// Split on document boundaries so a `kind: ConfigMap` is paired with the
		// name in its OWN document, not one further down the file.
		for _, doc := range strings.Split(string(b), "\n---") {
			if !configMapKindPattern.MatchString(doc) {
				continue
			}
			if m := metadataNamePattern.FindStringSubmatch(doc); m != nil {
				out[m[1]] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", infra, err)
	}
	return out
}

func TestEveryMountedConfigMapIsDefinedOrDeclared(t *testing.T) {
	root := moduleRoot(t)
	defined := definedConfigMaps(t, root)

	// NON-VACUITY: this repo definitely creates ConfigMaps (nats, kafka, trino).
	// A walk that finds none is broken, and would make every reference below
	// look undefined for the wrong reason.
	if len(defined) == 0 {
		t.Fatal("found zero ConfigMap definitions under infra/ — the walk is broken, not the manifests")
	}

	deployDir := filepath.Join(root, "infra", "deploy")
	entries, err := os.ReadDir(deployDir)
	if err != nil {
		t.Fatalf("read %s: %v", deployDir, err)
	}

	var dangling []string
	seenRef := false
	for _, e := range entries {
		if e.IsDir() || (filepath.Ext(e.Name()) != ".yaml" && filepath.Ext(e.Name()) != ".yml") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(deployDir, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		for _, m := range configMapRefPattern.FindAllStringSubmatch(string(b), -1) {
			name := m[1]
			seenRef = true
			if defined[name] {
				continue
			}
			if _, ok := operatorSuppliedConfigMaps[name]; ok {
				continue
			}
			dangling = append(dangling, e.Name()+" → "+name)
		}
	}
	// NON-VACUITY: if the reference regex matched nothing, this test would pass
	// no matter how many dangling mounts existed.
	if !seenRef {
		t.Fatal("no `configMap: name:` reference found in infra/deploy — the reference parser is broken, " +
			"so this guard would pass whatever the manifests do")
	}

	if len(dangling) > 0 {
		sort.Strings(dangling)
		t.Fatalf("Deployments mount ConfigMaps that this repo neither defines nor declares "+
			"operator-supplied: %v\nA missing ConfigMap does NOT produce the loud in-process refusal a "+
			"manifest may advertise — the kubelet cannot mount it, so the pod sits in "+
			"CreateContainerConfigError and the process never runs. Either add the ConfigMap to infra/, "+
			"or add it to operatorSuppliedConfigMaps AND document at the mount site how to create it "+
			"and what it must contain.", dangling)
	}
}
