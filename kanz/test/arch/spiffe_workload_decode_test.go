package arch

import (
	"errors"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// workloadDoc is the slice of a Deployment/StatefulSet/Rollout this guard needs:
// which app it is, which SPIFFE-ish env names it sets, and whether it mounts the
// SPIFFE CSI volume those names point at.
type workloadDoc struct {
	app            string
	spiffeEnvNames []string
	hasSpiffeCSI   bool
}

// rawWorkload mirrors only the manifest fields being inspected. Kept separate from
// networkPolicyDoc because that type models `spec.podSelector`/`egress`, which a
// workload does not have.
type rawWorkload struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Metadata struct {
				Labels map[string]string `yaml:"labels"`
			} `yaml:"metadata"`
			Spec struct {
				Containers []struct {
					Env []struct {
						Name string `yaml:"name"`
					} `yaml:"env"`
				} `yaml:"containers"`
				Volumes []struct {
					Name string `yaml:"name"`
					CSI  *struct {
						Driver string `yaml:"driver"`
					} `yaml:"csi"`
				} `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

// decodeWorkloadDocs extracts every workload in a multi-document manifest. Documents
// that are not workloads (Services, RBAC, NetworkPolicies) decode to a zero value and
// are skipped rather than erroring — a manifest legitimately mixes kinds.
func decodeWorkloadDocs(t *testing.T, body []byte, source string) []workloadDoc {
	t.Helper()
	var out []workloadDoc
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	for {
		var raw rawWorkload
		err := dec.Decode(&raw)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parse %s: %v", source, err)
		}
		switch raw.Kind {
		case "Deployment", "StatefulSet", "Rollout", "DaemonSet":
		default:
			continue
		}
		app := raw.Spec.Template.Metadata.Labels["app"]
		if app == "" {
			app = raw.Metadata.Name
		}
		doc := workloadDoc{app: app}
		for _, c := range raw.Spec.Template.Spec.Containers {
			for _, e := range c.Env {
				if strings.Contains(e.Name, "SPIFFE") {
					doc.spiffeEnvNames = append(doc.spiffeEnvNames, e.Name)
				}
			}
		}
		for _, v := range raw.Spec.Template.Spec.Volumes {
			if v.CSI != nil && v.CSI.Driver == "csi.spiffe.io" {
				doc.hasSpiffeCSI = true
			}
		}
		out = append(out, doc)
	}
	return out
}
