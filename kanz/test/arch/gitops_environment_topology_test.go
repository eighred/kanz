package arch

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TWO APPLICATIONS AUTO-PRUNING THE SAME OBJECTS IS NOT A REDUNDANT DEPLOY, IT
// IS A FIGHT.
//
// The app-of-apps is a matrix: every environment is crossed with every
// component. So an environment is only a real dimension if something in the
// generated Application actually DIFFERS per environment. The `dr` entry
// differed in exactly two places — the Application's name and a label — while
// `path`, `namespace` and `destination.server` were identical to `primary`'s
// (#153). `https://kubernetes.default.svc` is the cluster Argo CD runs in, so
// `dr` was the primary under a second name.
//
// The result was that every workload in kanz-services, kanz-messaging and the
// security tree was managed by two Applications, both with
// automated{prune: true, selfHeal: true}. Each was entitled to prune what the
// other had just reconciled, and the tracking annotation flips between them.
//
// This is the shape that is hard to see by reading: the file LOOKED like it
// described two environments, the header said each "carries its own destination
// cluster", and only resolving the URL shows both land in one place. A
// placeholder that is merely incomplete is inert; this one was active, and it
// was pointed at production's namespaces.
//
// The guard is therefore not "there must be one environment" — adding a genuine
// DR cluster must stay a one-line change. It is: two environments may not share
// a destination server while the template auto-syncs, because the matrix
// guarantees they then share every path and namespace too.
func TestNoTwoGitOpsEnvironmentsShareADestinationServer(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, "infra", "gitops", "applicationset.yaml")
	body := readFile(t, path)

	var doc struct {
		Spec struct {
			Generators []struct {
				Matrix struct {
					Generators []struct {
						List struct {
							Elements []map[string]string `yaml:"elements"`
						} `yaml:"list"`
					} `yaml:"generators"`
				} `yaml:"matrix"`
			} `yaml:"generators"`
			Template struct {
				Spec struct {
					Destination struct {
						Server    string `yaml:"server"`
						Namespace string `yaml:"namespace"`
					} `yaml:"destination"`
					SyncPolicy struct {
						Automated *struct {
							Prune    bool `yaml:"prune"`
							SelfHeal bool `yaml:"selfHeal"`
						} `yaml:"automated"`
					} `yaml:"syncPolicy"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("parse applicationset.yaml: %v", err)
	}

	// Environment elements are the matrix entries carrying a destination server.
	// Read from the file rather than restated here: a second copy of this list
	// would keep passing after someone adds an environment, which is the exact
	// moment this check needs to run.
	type env struct{ name, server string }
	var envs []env
	componentPaths := 0
	for _, g := range doc.Spec.Generators {
		for _, inner := range g.Matrix.Generators {
			for _, el := range inner.List.Elements {
				if s := el["server"]; s != "" {
					envs = append(envs, env{name: el["env"], server: s})
				}
				if el["path"] != "" {
					componentPaths++
				}
			}
		}
	}

	if len(envs) == 0 {
		t.Fatal("parsed no environment elements (none carrying a `server`) from " +
			"infra/gitops/applicationset.yaml — the scanner is looking at the wrong shape, " +
			"and would otherwise pass vacuously forever")
	}
	if componentPaths == 0 {
		t.Fatal("parsed no component elements (none carrying a `path`) from " +
			"infra/gitops/applicationset.yaml — without the component dimension this guard " +
			"cannot claim the environments collide, so it must not claim they are fine")
	}

	// Only meaningful while the template auto-syncs. Two Applications pointed at
	// one cluster are merely redundant if a human drives each sync; it is the
	// automated prune + selfHeal that turns the overlap into two controllers
	// reverting each other.
	auto := doc.Spec.Template.Spec.SyncPolicy.Automated
	if auto == nil {
		t.Skip("template does not set syncPolicy.automated — the collision this guards " +
			"requires two auto-pruning Applications; re-enable when automation returns")
	}

	// The matrix crosses envs with components, so a shared server means a shared
	// (path, namespace) too — UNLESS the destination namespace itself branches on
	// {{.env}}, which would separate them. Detect that rather than assuming it.
	nsBranchesOnEnv := strings.Contains(doc.Spec.Template.Spec.Destination.Namespace, "{{.env}}") ||
		strings.Contains(doc.Spec.Template.Spec.Destination.Namespace, "{{ .env }}")

	byServer := map[string][]string{}
	for _, e := range envs {
		byServer[e.server] = append(byServer[e.server], e.name)
	}

	var problems []string
	for server, names := range byServer {
		if len(names) < 2 || nsBranchesOnEnv {
			continue
		}
		sort.Strings(names)
		problems = append(problems, fmt.Sprintf(
			"environments %s all resolve to destination server %q\n\n"+
				"    The matrix crosses each of them with all %d component paths into the same\n"+
				"    namespace, and the template auto-syncs (prune=%t, selfHeal=%t). That is %d\n"+
				"    Applications per component managing one set of objects, each entitled to\n"+
				"    prune what the others reconcile.\n\n"+
				"    An environment is a real dimension only when something functional differs.\n"+
				"    A name and a label are not enough: this is how `dr` shipped as the primary\n"+
				"    under a second name (#153). Either give the environment a genuinely\n"+
				"    distinct, registered cluster, or remove it until that cluster exists rather\n"+
				"    than asserting a topology that is not there.",
			strings.Join(names, ", "), server, componentPaths, auto.Prune, auto.SelfHeal, len(names)))
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("GitOps environments collide on one cluster:\n\n  %s", strings.Join(problems, "\n\n  "))
	}
}
