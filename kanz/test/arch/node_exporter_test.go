package arch

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestNodeExporterDaemonSet asserts the node-exporter manifest is a valid DaemonSet
// that tolerates every node (so a newly-joined node is actually scraped). A malformed
// or too-narrowly-tolerated manifest would silently skip nodes.
func TestNodeExporterDaemonSet(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "infra", "observability", "node-exporter.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	type doc struct {
		Kind string `yaml:"kind"`
		Spec struct {
			Template struct {
				Spec struct {
					Tolerations []struct {
						Operator string `yaml:"operator"`
					} `yaml:"tolerations"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	var found bool
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	for {
		var d doc
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if d.Kind != "DaemonSet" {
			continue
		}
		found = true
		var tolerated bool
		for _, tol := range d.Spec.Template.Spec.Tolerations {
			if tol.Operator == "Exists" {
				tolerated = true
			}
		}
		if !tolerated {
			t.Errorf("node-exporter DaemonSet must tolerate every node (a toleration with operator: Exists), else new/tainted nodes are not scraped")
		}
	}
	if !found {
		t.Fatalf("no DaemonSet found in %s", path)
	}
}
