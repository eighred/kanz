package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A PROMETHEUS RULE FILE MUST BE REACHABLE BY EXACTLY ONE LIST (#230).
//
// `rule_files` in prometheus.yaml is /etc/prometheus/rules/*.rules.yaml — a MOUNT
// path. It matches whatever the ConfigMap happens to contain, so it cannot detect
// an omission: a ConfigMap holding two of three rule files looks exactly like a
// complete one, and Prometheus comes up green having loaded neither the third
// file's alerts nor any complaint about it.
//
// What decided the ConfigMap's contents was a COMMENT — two hand-written
// --from-file lines above the volume. And CI validated a separately-written glob,
// `cp kanz/infra/observability/slo/*.rules.yaml`. Two lists, both partial, neither
// aware of the other. An agent wrote 16 operational alert rules under
// infra/observability/alerts/ and they would have been in neither: unvalidated by
// promtool and absent from the cluster, while the repository looked like it had
// gained alerting. That is precisely the failure #230 exists to fix.
//
// So there is now one list — apply-rules.sh derives it from the tree — and this
// guard exists to stop a second one growing back. It checks three joints:
//
//	A. CI stages via `apply-rules.sh --list`, not its own glob.
//	B. No workflow copies rule files by a literal path glob (a second list).
//	C. The ConfigMap name the script writes is the one the manifest mounts.
//
// WHAT THIS DOES NOT CHECK. That the script's `find` is correct — it is executed
// by CI and by whoever deploys, and a broken find fails loudly there. This guard
// is about there being ONE derivation, not about that derivation's internals.
func TestPrometheusRulesHaveExactlyOneSourceOfTruth(t *testing.T) {
	repoRoot := filepath.Dir(moduleRoot(t))
	obsDir := filepath.Join(moduleRoot(t), "infra", "observability")

	// NON-VACUITY (1/3): the rule files must actually be there. If the tree moved,
	// every assertion below would be about nothing.
	var ruleFiles []string
	err := filepath.Walk(obsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(info.Name(), ".rules.yaml") {
			rel, _ := filepath.Rel(repoRoot, path)
			ruleFiles = append(ruleFiles, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", obsDir, err)
	}
	if len(ruleFiles) < 2 {
		t.Fatalf("found only %d *.rules.yaml under infra/observability — expected at least 2 "+
			"(slo.recording.rules.yaml and slo.alerts.rules.yaml). The scanner is broken or the "+
			"rules moved; either way this guard is asserting nothing", len(ruleFiles))
	}
	sort.Strings(ruleFiles)

	// NON-VACUITY (2/3): the single source of truth must exist.
	scriptPath := filepath.Join(obsDir, "apply-rules.sh")
	scriptBody, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read apply-rules.sh: %v — it is the one list of which rule files deploy; "+
			"without it prometheus.yaml's rule_files glob matches the mount and can never "+
			"report an omission (#230)", err)
	}
	script := string(scriptBody)

	// C. The manifest and the script must name the SAME ConfigMap. Renaming one and
	// not the other leaves the pod in ContainerCreating — loud, but only at deploy
	// time on a cluster that does not exist yet (#98), so it would sit undetected.
	cmName := regexp.MustCompile(`(?m)^configmap="([a-z0-9-]+)"`).FindStringSubmatch(script)
	if cmName == nil {
		t.Fatalf("apply-rules.sh does not declare `configmap=\"...\"` at the start of a line — " +
			"this guard reads it to check the manifest mounts what the script writes. If the " +
			"script's shape changed, update this guard rather than dropping the check")
	}
	manifest, err := os.ReadFile(filepath.Join(obsDir, "prometheus.yaml"))
	if err != nil {
		t.Fatalf("read prometheus.yaml: %v", err)
	}
	mountedCM := regexp.MustCompile(`configMap:\s*\{\s*name:\s*([a-z0-9-]+)\s*\}`).
		FindAllStringSubmatch(string(manifest), -1)
	var mounts []string
	for _, m := range mountedCM {
		mounts = append(mounts, m[1])
	}
	if len(mounts) == 0 {
		t.Fatalf("prometheus.yaml mounts no ConfigMap by `configMap: { name: ... }` — the " +
			"matcher is broken, not the manifest")
	}
	if !contains(mounts, cmName[1]) {
		t.Fatalf("apply-rules.sh writes ConfigMap %q but prometheus.yaml mounts %v.\n\n"+
			"The rules volume is deliberately NOT optional, so a name mismatch leaves the pod "+
			"in ContainerCreating rather than starting with zero rules. That is the right "+
			"failure, but it happens at deploy time on a cluster nobody has yet (#98) — so it "+
			"would be found by an outage, not by a test.", cmName[1], mounts)
	}

	// A + B. Exactly one list: CI must derive from the script, and no workflow may
	// carry its own rule-file glob.
	//
	// The literal-glob pattern deliberately matches ANY path ending in a
	// *.rules.yaml glob, not just the slo/ one that used to be here — the whole
	// point is that the next directory somebody adds must not get its own line.
	literalGlob := regexp.MustCompile(`[\w./-]*\*\.rules\.yaml`)
	derives := regexp.MustCompile(`apply-rules\.sh\s+--list`)

	var stagedByScript bool
	var offenders []string
	for _, path := range workflowFiles(t, repoRoot) {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		rel, _ := filepath.Rel(repoRoot, path)
		for i, raw := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
			line := strings.TrimSpace(raw)
			// Comments discuss the old glob to explain why it is gone — including the
			// note above the staging step. Matching prose would flag a workflow for
			// documenting itself, the same trap ci_test_parallelism_test.go avoids.
			if strings.HasPrefix(line, "#") {
				continue
			}
			if derives.MatchString(line) {
				stagedByScript = true
			}
			// `rule_files:` inside the embedded prometheus.yml is the MOUNT glob and is
			// correct; it is not a second repo-path list.
			//
			// The exemption is keyed on THE MATCHED GLOB, not on the line containing
			// the mount path anywhere. Testing the line was this guard's own first bug:
			// `cp kanz/infra/observability/slo/*.rules.yaml /etc/prometheus/rules/` —
			// the exact regression this arm exists to catch — names the mount as its
			// DESTINATION and was therefore skipped. Caught by mutation, which is the
			// only reason it is not still here.
			for _, m := range literalGlob.FindAllString(line, -1) {
				if strings.HasPrefix(m, "/etc/prometheus/rules/") {
					continue
				}
				offenders = append(offenders,
					filepath.ToSlash(rel)+":"+strconv.Itoa(i+1)+"\n      "+line)
				break
			}
		}
	}

	// NON-VACUITY (3/3).
	if !stagedByScript {
		t.Fatalf("no workflow stages rule files via `apply-rules.sh --list`.\n\n" +
			"CI must validate the SAME set the cluster loads. When it had its own glob " +
			"(`cp kanz/infra/observability/slo/*.rules.yaml`) a rule file outside slo/ was " +
			"deployed unchecked while the promtool step stayed green — 16 such rules were " +
			"written and would have fired never (#230).")
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("these workflow lines glob rule files by path instead of using "+
			"`apply-rules.sh --list`:\n\n  %s\n\nThat is a SECOND list of what deploys, and the "+
			"two drift silently: prometheus.yaml's rule_files matches the mount, so a file "+
			"missing from one list produces a green Prometheus with absent alerts rather than "+
			"an error.", strings.Join(offenders, "\n\n  "))
	}

	t.Logf("%d rule file(s) under infra/observability, one derivation (apply-rules.sh), "+
		"mounted as ConfigMap %q", len(ruleFiles), cmName[1])
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// EVERY ALERT MUST HAVE A TEST THAT MAKES IT FIRE (#230).
//
// `promtool check rules` proves an expression PARSES. It does not evaluate it,
// so it cannot tell a working alert from one that will never produce a sample —
// and this repository has already shipped the second kind at scale. #62 deleted
// ten data-quality rules that parsed, deployed, and could not fire, because the
// series they compared against had no producer; the layer read as healthy for
// months precisely BECAUSE nothing in it worked (alerts/README.md).
//
// #63's guard (observability_metrics_test.go) closes the specific case where the
// metric name does not exist in Go. It cannot close the general one: an alert
// over real metrics can still be unfireable through a label selector that
// matches nothing, an `and` between two vectors whose label sets never join, a
// unit error, or an aggregation that drops the label the join needs. Every one
// of those parses. The only thing that distinguishes a working alert from a
// decorative one is running it against a series, which is what promtool test
// rules does and what this guard makes non-optional.
//
// A FIRING CASE, NOT MERELY A MENTION. An alert named only inside an
// `exp_alerts: []` assertion is proven to stay QUIET, which is the opposite
// claim. Coverage here means at least one alert_rule_test entry for that name
// with a non-empty exp_alerts.
func TestEveryAlertRuleIsProvenToFire(t *testing.T) {
	obsDir := filepath.Join(moduleRoot(t), "infra", "observability")

	declared := map[string]string{} // alert name -> file it is declared in
	proven := map[string]bool{}     // alert name -> some harness makes it fire
	var harnesses []string

	err := filepath.Walk(obsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(obsDir, path)
		rel = filepath.ToSlash(rel)

		switch {
		case strings.HasSuffix(info.Name(), ".rules.yaml"):
			var doc struct {
				Groups []struct {
					Rules []struct {
						Alert string `yaml:"alert"`
					} `yaml:"rules"`
				} `yaml:"groups"`
			}
			if uerr := yaml.Unmarshal([]byte(readFile(t, path)), &doc); uerr != nil {
				t.Fatalf("parse %s: %v", rel, uerr)
			}
			for _, g := range doc.Groups {
				for _, r := range g.Rules {
					if r.Alert != "" {
						declared[r.Alert] = rel
					}
				}
			}
		case strings.HasSuffix(info.Name(), "_test.yaml"):
			harnesses = append(harnesses, rel)
			var doc struct {
				Tests []struct {
					AlertRuleTests []struct {
						Alertname string                   `yaml:"alertname"`
						ExpAlerts []map[string]interface{} `yaml:"exp_alerts"`
					} `yaml:"alert_rule_test"`
				} `yaml:"tests"`
			}
			if uerr := yaml.Unmarshal([]byte(readFile(t, path)), &doc); uerr != nil {
				t.Fatalf("parse %s: %v", rel, uerr)
			}
			for _, tc := range doc.Tests {
				for _, art := range tc.AlertRuleTests {
					// The non-empty check is the whole point — see the doc comment.
					if art.Alertname != "" && len(art.ExpAlerts) > 0 {
						proven[art.Alertname] = true
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", obsDir, err)
	}

	// NON-VACUITY, both halves. A parser that stopped finding alerts, or a walk
	// that stopped finding harnesses, would make every assertion below trivially
	// true — and this guard's whole subject is checks that pass while asserting
	// nothing.
	if len(declared) == 0 {
		t.Fatal("found zero `- alert:` declarations under infra/observability — the rules " +
			"parser is broken, not the rules")
	}
	if len(harnesses) == 0 {
		t.Fatal("found zero *_test.yaml harnesses under infra/observability — the walk is " +
			"broken, or every alert in the estate is now unproven")
	}

	var unproven []string
	for name, file := range declared {
		if !proven[name] {
			unproven = append(unproven, name+" ("+file+")")
		}
	}
	if len(unproven) > 0 {
		sort.Strings(unproven)
		t.Errorf("%d alert(s) have no promtool test that makes them FIRE:\n\n  %s\n\n"+
			"Add an alert_rule_test with a non-empty exp_alerts to a *_test.yaml beside the "+
			"rules file. A rule that parses but cannot fire is indistinguishable from a healthy "+
			"platform: the expression returns an empty vector, the alert never appears, and the "+
			"absence reads as quiet. That is how ten data-quality rules survived for months "+
			"(#62), and it is what #230 exists to stop repeating.\n\n"+
			"Naming the alert only in an `exp_alerts: []` case does NOT count — that proves it "+
			"stays silent, which is the opposite claim.",
			len(unproven), strings.Join(unproven, "\n  "))
	}

	t.Logf("%d alert(s) across %d harness(es), all with a firing case", len(declared), len(harnesses))
}

// A promtool HARNESS MUST BE RUN BY DISCOVERY, NOT BY NAME (#230).
//
// Sibling of TestPrometheusRulesHaveExactlyOneSourceOfTruth above, one layer
// out: that guard is about which rule files DEPLOY, this one is about which
// test harnesses EXECUTE. Both failures look the same from a green pipeline —
// a file in the repository that nothing reads.
//
// It has happened here. #61 found slo_test.yaml had never been executed by
// anything: every reference to promtool in the repository was a comment telling
// a human to run it. The step that fixed it named `slo_test.yaml` with
// working-directory pinned to slo/, which is the same list-written-by-hand
// shape, and a second harness written beside a new rules file would have sat
// unrun exactly as the first one did.
func TestPromtoolHarnessesAreRunByDiscovery(t *testing.T) {
	repoRoot := filepath.Dir(moduleRoot(t))
	obsDir := filepath.Join(moduleRoot(t), "infra", "observability")

	var harnessNames []string
	err := filepath.Walk(obsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(info.Name(), "_test.yaml") {
			harnessNames = append(harnessNames, info.Name())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", obsDir, err)
	}
	// NON-VACUITY (1/2).
	if len(harnessNames) == 0 {
		t.Fatal("found no *_test.yaml under infra/observability — nothing to check, which " +
			"means this guard is asserting nothing")
	}
	sort.Strings(harnessNames)

	discovers := regexp.MustCompile(`-name\s+'\*_test\.yaml'|-name\s+"\*_test\.yaml"`)

	var discovered bool
	var offenders []string
	for _, path := range workflowFiles(t, repoRoot) {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		rel, _ := filepath.Rel(repoRoot, path)
		for i, raw := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
			line := strings.TrimSpace(raw)
			// Comments explain why the enumeration is gone; matching prose would
			// flag a workflow for documenting itself. Same exemption, and same
			// reason, as the guard above.
			if strings.HasPrefix(line, "#") {
				continue
			}
			if discovers.MatchString(line) {
				discovered = true
			}
			for _, name := range harnessNames {
				if strings.Contains(line, name) {
					offenders = append(offenders,
						filepath.ToSlash(rel)+":"+strconv.Itoa(i+1)+"\n      "+line)
					break
				}
			}
		}
	}

	// NON-VACUITY (2/2).
	if !discovered {
		t.Fatalf("no workflow discovers promtool harnesses with `find ... -name '*_test.yaml'`.\n\n" +
			"Every harness in the tree must be executed. When CI named slo_test.yaml directly, a " +
			"second harness added anywhere else would have been a test file nobody ran — a claim " +
			"of coverage with nothing behind it, which is the defect #61 fixed (#230).")
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("these workflow lines name a promtool harness by filename instead of "+
			"discovering it:\n\n  %s\n\nA named list omits the next harness silently: it sits in "+
			"the repository looking like coverage and is never executed.", strings.Join(offenders, "\n\n  "))
	}

	t.Logf("%d promtool harness(es) under infra/observability, all executed by discovery: %s",
		len(harnessNames), strings.Join(harnessNames, ", "))
}
