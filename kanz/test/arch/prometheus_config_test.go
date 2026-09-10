package arch

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPrometheusConsumersUseTheOwnedServiceNamespace(t *testing.T) {
	root := moduleRoot(t)
	serviceURL := regexp.MustCompile(`http://prometheus\.([a-z0-9-]+)\.svc:9090`)
	found := 0
	err := filepath.WalkDir(filepath.Join(root, "infra"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || (filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".sh") {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, match := range serviceURL.FindAllSubmatch(body, -1) {
			found++
			if string(match[1]) != "kanz-observability" {
				t.Errorf("%s addresses Prometheus in namespace %q; the Service is owned by kanz-observability", path, match[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Fatal("no in-cluster Prometheus consumer URL was found; the namespace check did not execute")
	}
}

// THE SCRAPE CONFIG MUST AGREE WITH THE ESTATE IT SCRAPES.
//
// prometheus.yaml is wiring, and every way it can be wrong is SILENT. Prometheus
// starts, serves a green /-/ready, and reports no error for any of these:
//
//   - a rule file whose name the rule_files glob does not match: zero rules
//     load, every SLO alert never fires, and `promtool check rules` still passes
//     because the FILE is valid — nothing checks that Prometheus reads it
//   - a relabel keyed on an annotation the manifests do not set: no pod is
//     kept, the target list is empty, and an empty target list looks identical
//     to a healthy one nobody has broken yet
//   - the rules volume marked optional: the mount succeeds empty and the
//     previous case happens with no ConfigMap at all
//
// None of these produce a DOWN target. They produce NO target, and absent
// targets appear on no dashboard and fire no alert. That is the failure this
// whole issue exists to remove, so it must not be reintroduced by the fix.
func TestPrometheusConfigMatchesTheEstate(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, "infra", "observability", "prometheus.yaml")

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	type volume struct {
		Name      string `yaml:"name"`
		ConfigMap struct {
			Name     string `yaml:"name"`
			Optional *bool  `yaml:"optional"`
		} `yaml:"configMap"`
	}
	type doc struct {
		Kind     string                `yaml:"kind"`
		Metadata struct{ Name string } `yaml:"metadata"`
		Data     map[string]string     `yaml:"data"`
		Spec     struct {
			Template struct {
				Spec struct {
					Volumes []volume `yaml:"volumes"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}

	var (
		promYML string
		volumes []volume
	)
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	for {
		var d doc
		derr := dec.Decode(&d)
		if errors.Is(derr, io.EOF) {
			break
		}
		if derr != nil {
			t.Fatalf("parse %s: %v", path, derr)
		}
		switch d.Kind {
		case "ConfigMap":
			if s, ok := d.Data["prometheus.yml"]; ok {
				promYML = s
			}
		case "Deployment":
			volumes = d.Spec.Template.Spec.Volumes
		}
	}
	if promYML == "" {
		t.Fatal("no ConfigMap in prometheus.yaml carries a prometheus.yml key — " +
			"the scanner is broken, or the config moved and this guard now checks nothing")
	}

	var cfg struct {
		RuleFiles     []string `yaml:"rule_files"`
		ScrapeConfigs []struct {
			JobName        string `yaml:"job_name"`
			RelabelConfigs []struct {
				SourceLabels []string `yaml:"source_labels"`
				Action       string   `yaml:"action"`
				TargetLabel  string   `yaml:"target_label"`
			} `yaml:"relabel_configs"`
		} `yaml:"scrape_configs"`
	}
	if err := yaml.Unmarshal([]byte(promYML), &cfg); err != nil {
		t.Fatalf("the embedded prometheus.yml is not valid YAML: %v", err)
	}

	// 1. EVERY RULE FILE ON DISK MUST MATCH THE GLOB THAT LOADS IT.
	sloDir := filepath.Join(root, "infra", "observability", "slo")
	entries, err := os.ReadDir(sloDir)
	if err != nil {
		t.Fatalf("read %s: %v", sloDir, err)
	}
	var ruleFiles []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(sloDir, e.Name()))
		if rerr != nil {
			continue
		}
		// A Prometheus rule file is the one with a top-level `groups:`. slo.yaml
		// (the SLO catalog) and slo_test.yaml (the promtool harness) are not.
		if strings.Contains(strings.ReplaceAll(string(b), "\r\n", "\n"), "\ngroups:") {
			ruleFiles = append(ruleFiles, e.Name())
		}
	}
	if len(ruleFiles) == 0 {
		t.Fatal("found zero rule files in infra/observability/slo — the scanner is broken, not the estate")
	}
	sort.Strings(ruleFiles)

	for _, rf := range ruleFiles {
		matched := false
		for _, glob := range cfg.RuleFiles {
			ok, merr := filepath.Match(filepath.Base(glob), rf)
			if merr != nil {
				t.Fatalf("rule_files entry %q is not a valid glob: %v", glob, merr)
			}
			if ok {
				matched = true
			}
		}
		if !matched {
			t.Errorf("infra/observability/slo/%s is a rule file that rule_files %v does not match\n\n"+
				"Prometheus would load zero rules from it and start clean. Every alert built on those "+
				"recordings would simply never fire, and nothing anywhere would say so.", rf, cfg.RuleFiles)
		}
	}

	// 2. THE DISCOVERY RELABEL MUST KEY ON THE ANNOTATIONS THE MANIFESTS SET.
	// These two strings are the contract between this config and the 21 manifests
	// that TestEveryDeployableServiceIsScrapable enforces. If either side is
	// renamed alone, the target list silently empties.
	const (
		scrapeLabel = "__meta_kubernetes_pod_annotation_prometheus_io_scrape"
		portLabel   = "__meta_kubernetes_pod_annotation_prometheus_io_port"
	)
	seen := map[string]bool{}
	for _, sc := range cfg.ScrapeConfigs {
		for _, rc := range sc.RelabelConfigs {
			for _, sl := range rc.SourceLabels {
				seen[sl] = true
			}
		}
	}
	for _, want := range []string{scrapeLabel, portLabel} {
		if !seen[want] {
			t.Errorf("no relabel rule in prometheus.yml reads %s\n\n"+
				"The deploy manifests carry prometheus.io/scrape and prometheus.io/port because this "+
				"config claims to read them. If it does not, no pod is kept and the target list is "+
				"empty — which looks exactly like a healthy estate nobody has broken yet.", want)
		}
	}

	const (
		rolloutMetaLabel   = "__meta_kubernetes_pod_label_rollouts_pod_template_hash"
		rolloutMetricLabel = "rollouts_pod_template_hash"
	)
	canaryLabelCopied := false
	for _, sc := range cfg.ScrapeConfigs {
		for _, rc := range sc.RelabelConfigs {
			if rc.TargetLabel != rolloutMetricLabel {
				continue
			}
			for _, source := range rc.SourceLabels {
				if source == rolloutMetaLabel {
					canaryLabelCopied = true
				}
			}
		}
	}
	if !canaryLabelCopied {
		t.Errorf("Prometheus does not copy %s to %s; risk canary queries cannot select the new ReplicaSet",
			rolloutMetaLabel, rolloutMetricLabel)
	}

	// 3. THE RULES MOUNT MUST NOT BE OPTIONAL.
	var found bool
	for _, v := range volumes {
		if v.ConfigMap.Name == "" {
			continue
		}
		if !strings.Contains(v.Name, "rule") {
			continue
		}
		found = true
		if v.ConfigMap.Optional != nil && *v.ConfigMap.Optional {
			t.Errorf("the %q volume mounts ConfigMap %q with optional: true\n\n"+
				"With optional, an absent ConfigMap mounts as an EMPTY directory: Prometheus starts, "+
				"loads no rules, and serves a green /-/ready. Without it the pod stays in "+
				"ContainerCreating and says which ConfigMap is missing. Fail loudly, never silently.",
				v.Name, v.ConfigMap.Name)
		}
	}
	if !found {
		t.Error("the Deployment mounts no ConfigMap volume whose name mentions rules — " +
			"either the rules are no longer mounted (so none load) or this guard has stopped " +
			"checking anything")
	}
}
