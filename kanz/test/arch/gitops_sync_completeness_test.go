package arch

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// AN infra/ TREE IS EITHER DELIVERED BY ARGO CD OR SAYS WHY NOT (#625).
//
// # What went wrong without it
//
// infra/gitops/README.md opens with "Every in-cluster workload is delivered by
// Argo CD from git — no `kubectl apply` in anger." The ApplicationSet syncs FOUR
// paths. Thirteen trees under infra/ hold manifests. The gap was not argued
// anywhere; it was simply absent, and absence reads as "nothing to see".
//
// infra/observability is what that cost. Prometheus, its rules, Grafana and
// node-exporter are reachable by no applying path in this repository — not the
// ApplicationSet, not the rig, not CI. An alert rule added there is parsed,
// promtool-tested, proven-to-fire by TestEveryAlertRuleIsProvenToFire, reviewed,
// merged — and loaded by nothing. Thirty-two alerts sat in that state. The
// README even gave a reason ("shipped as their own PrometheusRule/ConfigMap
// wrapping"), and it named an artifact that does not exist: there is no
// PrometheusRule anywhere in this repository.
//
// That is the failure mode this guard exists for. Not a tree that is
// deliberately unsynced — several legitimately are — but a tree whose status
// nobody stated, where "decided against" and "never considered" look identical.
// CLAUDE.md's rule is the general form: "nothing configured" and "checked, and
// fine" must never look the same.
//
// # What it checks
//
// DEFAULT-DENY. Every directory directly under infra/ that contains at least one
// Kubernetes manifest must be covered by a sync path, or carry an entry in
// syncSurfaceExempt stating why and what would retire it.
//
// A tree with NO manifests is not required to be either. infra/edge,
// infra/catalog, infra/onboarding and infra/terraform hold zero today —
// requiring them to justify themselves would be noise, and the dead-entry arm
// below catches an exemption written for one.
func TestEveryInfraTreeIsSyncedOrNamedExempt(t *testing.T) {
	root := moduleRoot(t)
	synced := argoSyncedPaths(t, root)
	if len(synced) == 0 {
		t.Fatal("no sync paths discovered from infra/gitops — this guard is blind")
	}

	trees := infraTreesWithManifests(t, root)
	// NON-VACUITY. This repository has synced trees — deploy, nats, kafka,
	// security at minimum. A run that discovered no manifests at all would pass
	// however unreachable the estate had become, which is the shape of the defect
	// being guarded: a check reporting "fine" after checking nothing.
	if len(trees) < 8 {
		t.Fatalf("found only %d infra tree(s) with manifests (%v) — the scan is broken "+
			"and this guard proves nothing", len(trees), trees)
	}

	var problems []string
	covered := map[string]bool{}
	for _, tree := range trees {
		path := "kanz/infra/" + tree
		if synced[path] {
			covered[tree] = true
			if reason, exempt := syncSurfaceExempt[tree]; exempt {
				problems = append(problems, "exemption for infra/"+tree+" is DEAD: the tree IS a "+
					"sync path now, so the exemption only hides it from this guard. Delete it.\n      reason on file: "+reason)
			}
			continue
		}
		if _, exempt := syncSurfaceExempt[tree]; exempt {
			continue
		}
		problems = append(problems, "infra/"+tree+" holds Kubernetes manifests and is synced by NOTHING — "+
			"not the ApplicationSet, not bootstrap.yaml. Everything in it is merged, reviewed and "+
			"never applied. Add it as a component, or add a syncSurfaceExempt entry saying why not.")
	}

	// DEAD ENTRIES. An exemption for a tree that no longer holds manifests — or
	// never did — is stale permission, and the next reader takes it for a
	// decision rather than a leftover.
	present := map[string]bool{}
	for _, tree := range trees {
		present[tree] = true
	}
	for tree := range syncSurfaceExempt {
		if covered[tree] {
			continue // already reported above, with its reason quoted
		}
		if !present[tree] {
			problems = append(problems, "exemption for infra/"+tree+" is DEAD: that tree holds no "+
				"manifests (or no longer exists), so it needs no exemption")
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d problem(s) on the GitOps sync surface:\n\n  %s",
			len(problems), strings.Join(problems, "\n\n  "))
	}
}

// EVERY COMPONENT'S NAMESPACE IS ONE THE AppProject ADMITS (#625).
//
// projects.yaml is a deny-by-default allowlist: an Application whose destination
// namespace is not in `spec.destinations` is REFUSED by Argo CD, with the
// component never syncing and the failure visible only in Argo's UI —
// "destination namespace ... is not permitted in project kanz".
//
// THIS IS NOT HYPOTHETICAL. When #625 was opened, destinations listed
// `observability` while every manifest in that tree declares
// `kanz-observability` (node-exporter.yaml, and apply-rules.sh's own -n).
// Nothing caught it because nothing had ever tried to sync that tree — the
// allowlist and the manifests had never been compared by anything.
//
// It is checked against the ELEMENTS rather than the rendered Applications
// because the elements are what a person edits: adding a component is a one-line
// list entry, and that line is where the namespace is chosen.
func TestEveryComponentNamespaceIsAllowedByTheAppProject(t *testing.T) {
	root := moduleRoot(t)
	allowed := appProjectDestinationNamespaces(t, root)
	if len(allowed) == 0 {
		t.Fatal("no destination namespaces parsed from infra/gitops/projects.yaml — this guard is blind")
	}

	used := componentNamespaces(t, root)
	if len(used) == 0 {
		t.Fatal("no component namespaces parsed from the ApplicationSets — this guard is blind")
	}

	var problems []string
	for _, cn := range used {
		if allowed[cn.namespace] {
			continue
		}
		problems = append(problems, cn.manifest+": component "+cn.component+" (path "+cn.path+
			") targets namespace "+cn.namespace+", which projects.yaml does not list in "+
			"spec.destinations. Argo CD REFUSES that Application — it never syncs, and the only "+
			"place that says so is Argo's own UI.")
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d component(s) target a namespace the AppProject denies:\n\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// syncSurfaceExempt names infra/ trees that hold manifests and are deliberately
// NOT delivered by Argo CD, with the reason.
//
// DEFAULT-DENY: an entry here is permission, and the dead-entry arm above
// removes it the moment the tree is synced or stops holding manifests. The
// reason is the point — a bare list would re-create the silence this guard was
// written to end.
var syncSurfaceExempt = map[string]string{
	"backstage": "NOT KUBERNETES. catalog-info.yaml is a Backstage software catalog " +
		"(backstage.io/v1alpha1 System/Component), ingested by pointing a Backstage instance at the " +
		"file via catalog.locations. Argo would try to apply a kind no cluster in this estate serves, " +
		"and the Application would sit permanently degraded on a CRD that will never exist.",

	"controllers": "BOOTSTRAP CONTROL PLANES, APPLIED BY PINNED INSTALLERS. A controller cannot " +
		"reconcile the custom resources that declare its own installation before its CRDs and process " +
		"exist. infra/controllers/argo-rollouts is checksum-verified and installed through " +
		"tools/Install-TestnetArgoRollouts.ps1; #1101 retires this exemption only after an independent " +
		"cluster bootstrap reconciler owns that dependency order.",

	"chaos": "EXPERIMENTS, RUN ON DEMAND (INFRA-01e). These are fault injections — az-kill.yaml " +
		"terminates an availability zone — driven by verify.sh during a game day. Under " +
		"automated{prune,selfHeal} a synced tree is applied continuously and re-applied when anyone " +
		"deletes it, which would run an AZ kill in a loop against the estate it is meant to test.",

	"dr": "PRIMARIES AND REPLICAS TARGET DIFFERENT CLUSTERS, and one sync path cannot express that: " +
		"cluster.yaml (the six CNPG primaries) and replica.yaml (their standbys) sit in one directory " +
		"and belong in two regions. infra/gitops/README.md states this. It is ALSO the estate's " +
		"durable state — the order book, the ledgers, the credentials — so putting it under " +
		"prune+selfHeal is a decision with an unrecoverable failure mode, not a list entry. " +
		"RETIRED BY #106, which restores a real DR cluster and the second Argo CD that reconciles it; " +
		"the env dimension in applicationset.yaml documents the same three preconditions.",

	"lakehouse": "A SINGLE-USER REFERENCE DEPLOYMENT, by its own header (LAKE-01d): Trino plus a " +
		"Jupyter pod for analysts, where \"a real estate runs JupyterHub for per-user pods\". " +
		"Syncing it would deploy the reference shape as though it were the production one.",

	"messaging": "REDIS IS BLOCKED ON A REAL ONE (EXEC-M22). redis.yaml is webhook-ingest's shared " +
		"nonce cache — the thing that lets the platform's entrance run more than one pod without " +
		"admitting a re-delivered alert twice. Deploying this single-pod manifest as the production " +
		"cache would put the replay defence on a store with no persistence and no failover, which is " +
		"worse than the pinned single replica it replaces. RETIRED BY #106 (live Redis).",

	"operator": "HOLDS A ONE-SHOT JOB THAT MUST NOT BE RE-CREATED. halt-job.yaml is kanz-halt's " +
		"manifest end to end, and the Job in it is the break-glass invocation itself — under " +
		"recurse+prune+selfHeal Argo would re-apply it, i.e. re-halt the platform, and re-apply it " +
		"again when anyone deleted it. The tree's OTHER manifest (cli-identities.yaml: ServiceAccount " +
		"and NetworkPolicy for the three shell-run CLIs) is ordinary and syncable, so this exemption " +
		"is about the Job, not the tree. RETIRING IT means naming halt-job.yaml in directory.exclude " +
		"and adding the tree — deliberately left out of #625 so the kill switch's delivery is changed " +
		"on its own, and not as a side effect of an observability fix.",

	"overlays": "EXPLICIT TESTNET PROMOTION BOUNDARIES, NOT A PRODUCTION APPLICATIONSET COMPONENT. " +
		"infra/overlays/testnet-tokyo binds one reviewed release to the cost-bounded Tokyo cluster and " +
		"is applied only after external dependency and DR gates pass. #1098 owns its server-side " +
		"apply, rollback, and eventual GitOps registration; auto-syncing it now would bypass those gates.",

	"tenancy": "APPLIED PER RUN BY ITS OWN TOOLING (MT-01f). onboard-job.yaml is a template — the " +
		"operator sets TENANT and the tenant.env vars and renames the Job per run — and tenantctl.sh " +
		"is a script the Job mounts as a ConfigMap, not a manifest. infra/gitops/README.md already " +
		"names templated/operational manifests as out of scope; this is that rule.",
}

// argoSyncedPaths returns every repo-relative path Argo CD delivers.
//
// BOTH KINDS, because the repository uses both and counting one would be a guard
// that flagged infra/gitops — the tree that carries the ApplicationSets — as
// unsynced. bootstrap.yaml is a plain Application (it is the single manual
// apply, and it syncs infra/gitops so Argo manages its own config from git);
// everything else is generated by ApplicationSets.
func argoSyncedPaths(t *testing.T, root string) map[string]bool {
	t.Helper()
	paths := map[string]bool{}
	for _, src := range gitOpsSyncSources(t, root) {
		for _, p := range src.paths {
			paths[p] = true
		}
	}
	for _, doc := range argoApplicationDocs(t, root) {
		if p := doc.Spec.Source.Path; p != "" && !strings.Contains(p, "{{") {
			paths[p] = true
		}
	}
	return paths
}

type argoApplicationDoc struct {
	Kind string `yaml:"kind"`
	Spec struct {
		Source struct {
			Path string `yaml:"path"`
		} `yaml:"source"`
	} `yaml:"spec"`
}

func argoApplicationDocs(t *testing.T, root string) []argoApplicationDoc {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, "infra", "gitops", "*.yaml"))
	if err != nil {
		t.Fatalf("glob infra/gitops: %v", err)
	}
	sort.Strings(files)

	var docs []argoApplicationDoc
	for _, f := range files {
		dec := yaml.NewDecoder(strings.NewReader(readFile(t, f)))
		for {
			var doc argoApplicationDoc
			err := dec.Decode(&doc)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("parse %s as YAML: %v", f, err)
			}
			if doc.Kind == "Application" {
				docs = append(docs, doc)
			}
		}
	}
	return docs
}

// infraTreesWithManifests returns the directories directly under infra/ that
// contain at least one Kubernetes manifest, at any depth.
//
// "Contains a manifest" is `kind:` at the start of a line in a .yaml — the same
// shape the rest of this package uses. A rule file under
// observability/alerts/ has no `kind:` and does not make a tree syncable on its
// own; prometheus.yaml, which mounts them, does.
func infraTreesWithManifests(t *testing.T, root string) []string {
	t.Helper()
	base := filepath.Join(root, "infra")
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatalf("read infra/: %v", err)
	}

	var trees []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		found := false
		err := filepath.WalkDir(filepath.Join(base, e.Name()), func(path string, d os.DirEntry, err error) error {
			if err != nil || found || d.IsDir() || !strings.HasSuffix(path, ".yaml") {
				return err
			}
			for _, line := range strings.Split(readFile(t, path), "\n") {
				if strings.HasPrefix(strings.TrimRight(line, "\r"), "kind:") {
					found = true
					return filepath.SkipAll
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk infra/%s: %v", e.Name(), err)
		}
		if found {
			trees = append(trees, e.Name())
		}
	}
	sort.Strings(trees)
	return trees
}

type componentNamespace struct {
	manifest  string
	component string
	path      string
	namespace string
}

// componentNamespaces reads the (component, path, namespace) triples straight
// out of every ApplicationSet's generator elements.
func componentNamespaces(t *testing.T, root string) []componentNamespace {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, "infra", "gitops", "*.yaml"))
	if err != nil {
		t.Fatalf("glob infra/gitops: %v", err)
	}
	sort.Strings(files)

	var out []componentNamespace
	for _, f := range files {
		rel := filepath.ToSlash(strings.TrimPrefix(f, root+string(filepath.Separator)))
		dec := yaml.NewDecoder(strings.NewReader(readFile(t, f)))
		for {
			var doc applicationSetDoc
			err := dec.Decode(&doc)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("parse %s as YAML: %v", rel, err)
			}
			if doc.Kind != "ApplicationSet" {
				continue
			}
			for _, g := range doc.Spec.Generators {
				for _, inner := range g.Matrix.Generators {
					out = append(out, elementNamespaces(rel, inner.List.Elements)...)
				}
				out = append(out, elementNamespaces(rel, g.List.Elements)...)
			}
		}
	}
	return out
}

func elementNamespaces(manifest string, elements []map[string]string) []componentNamespace {
	var out []componentNamespace
	for _, el := range elements {
		ns := el["namespace"]
		if ns == "" || strings.Contains(ns, "{{") {
			continue // an environment element, or a template placeholder
		}
		out = append(out, componentNamespace{
			manifest:  manifest,
			component: el["component"],
			path:      el["path"],
			namespace: ns,
		})
	}
	return out
}

// appProjectDestinationNamespaces reads spec.destinations[].namespace from the
// kanz AppProject.
func appProjectDestinationNamespaces(t *testing.T, root string) map[string]bool {
	t.Helper()
	path := filepath.Join(root, "infra", "gitops", "projects.yaml")

	ns := map[string]bool{}
	dec := yaml.NewDecoder(strings.NewReader(readFile(t, path)))
	for {
		var doc struct {
			Kind string `yaml:"kind"`
			Spec struct {
				Destinations []struct {
					Namespace string `yaml:"namespace"`
				} `yaml:"destinations"`
			} `yaml:"spec"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parse infra/gitops/projects.yaml as YAML: %v", err)
		}
		if doc.Kind != "AppProject" {
			continue
		}
		for _, d := range doc.Spec.Destinations {
			if d.Namespace != "" {
				ns[d.Namespace] = true
			}
		}
	}
	return ns
}
