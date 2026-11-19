package arch

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const operatorSyncPath = "kanz/infra/operator"

type operatorGitOpsDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string            `yaml:"name"`
		Namespace string            `yaml:"namespace"`
		Labels    map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
}

// The halt invocation must stay outside reconciliation, while its identity and
// every ordinary sibling remain inside it. A whole-tree exemption or a Job hook
// cannot express that boundary: either would lose identities or invoke a brake.
func operatorGitOpsProblems(sources []gitOpsSyncSource, files map[string]string, projectAllowed bool) []string {
	var problems []string
	matched := 0
	for _, source := range sources {
		for _, path := range source.paths {
			if path != operatorSyncPath && !strings.HasPrefix(operatorSyncPath, path+"/") {
				continue
			}
			matched++
			if path != operatorSyncPath || !source.recurse || source.includeRaw != "" ||
				source.templatePath != "{{.path}}" || source.destinationNamespace != "{{.namespace}}" {
				problems = append(problems, "operator-sync-path: require the complete recursive operator directory without an include filter")
			}
			if !source.excluded["halt-job.yaml"] {
				problems = append(problems, "halt-invocation-syncable: halt-job.yaml must be excluded by its exact name")
			}
			for name := range files {
				if name != "halt-job.yaml" && source.excluded[name] {
					problems = append(problems, "operator-identity-excluded: "+name)
				}
			}
		}
	}
	if matched != 1 {
		problems = append(problems, fmt.Sprintf("operator-sync-owner: expected one operator source, found %d", matched))
	}
	if !projectAllowed {
		problems = append(problems, "operator-project-denied: the operator component and AppProject must target kanz-operator")
	}

	identities := map[string]bool{}
	seen := map[string]bool{}
	namespaces, jobs := 0, 0
	for name, body := range files {
		if extension := strings.ToLower(filepath.Ext(name)); extension != ".yaml" && extension != ".yml" {
			continue
		}
		docs, err := decodeYAMLStream[operatorGitOpsDoc]([]byte(body))
		if err != nil {
			problems = append(problems, "operator-manifest-invalid: "+name)
			continue
		}
		for _, doc := range docs {
			if doc.Kind == "" {
				continue
			}
			key := doc.Kind + "/" + doc.Metadata.Namespace + "/" + doc.Metadata.Name
			if seen[key] {
				problems = append(problems, "operator-object-duplicate: "+key)
			}
			seen[key] = true
			if name == "halt-job.yaml" {
				jobs++
				if doc.Kind != "Job" || doc.Metadata.Name != "kanz-halt" || doc.Metadata.Namespace != "kanz-operator" {
					problems = append(problems, "halt-invocation-mixed: the excluded file must hold only the manual halt Job")
				}
				continue
			}
			switch doc.Kind {
			case "Namespace":
				namespaces++
				if doc.Metadata.Name != "kanz-operator" || doc.Metadata.Labels["kanz.internal/spiffe"] != "enabled" {
					problems = append(problems, "operator-spiffe-namespace: the reconciled namespace must retain SPIRE issuance")
				}
			case "ServiceAccount", "NetworkPolicy":
				if doc.Metadata.Namespace != "kanz-operator" {
					problems = append(problems, "operator-object-namespace: "+key)
				}
				identities[doc.Kind+"/"+doc.Metadata.Name] = true
			default:
				problems = append(problems, "operator-sync-kind: only identity resources belong in the automatic operator surface: "+key)
			}
		}
	}
	if namespaces != 1 || jobs != 1 {
		problems = append(problems, "operator-manifest-inventory: require one reconciled namespace and one excluded invocation")
	}
	for name := range operatorSVIDs {
		for _, key := range []string{"ServiceAccount/" + name, "NetworkPolicy/" + name + "-egress"} {
			if !identities[key] {
				problems = append(problems, "operator-identity-missing: "+key)
			}
		}
	}
	slices.Sort(problems)
	return problems
}

func operatorGitOpsInputs(t *testing.T) ([]gitOpsSyncSource, map[string]string, bool) {
	t.Helper()
	root := moduleRoot(t)
	sources := gitOpsSyncSources(t, root)
	files := map[string]string{}
	for name, path := range syncedFiles(t, root, gitOpsSyncSource{}, operatorSyncPath) {
		files[name] = readFile(t, path)
	}
	// A second, plain Application must not bypass the ApplicationSet exclusion.
	for _, app := range argoApplicationDocs(t, root) {
		path := app.Spec.Source.Path
		if path == operatorSyncPath || strings.HasPrefix(operatorSyncPath, path+"/") {
			sources = append(sources, gitOpsSyncSource{paths: []string{path}})
		}
	}
	operatorComponents := 0
	for _, component := range componentNamespaces(t, root) {
		if component.path == operatorSyncPath && component.component == "operator" && component.namespace == "kanz-operator" {
			operatorComponents++
		}
	}
	return sources, files, appProjectDestinationNamespaces(t, root)["kanz-operator"] && operatorComponents == 1
}

func TestOperatorIdentitiesSyncWithoutInvokingHalt(t *testing.T) {
	sources, files, allowed := operatorGitOpsInputs(t)
	if problems := operatorGitOpsProblems(sources, files, allowed); len(problems) != 0 {
		t.Fatalf("operator GitOps boundary:\n%s", strings.Join(problems, "\n"))
	}
}

func TestOperatorGitOpsGuardRejectsBoundaryMutations(t *testing.T) {
	baseline, original, allowed := operatorGitOpsInputs(t)
	cases := []struct {
		name string
		code string
		edit func([]gitOpsSyncSource, map[string]string) bool
	}{
		{"halt becomes syncable", "halt-invocation-syncable", func(sources []gitOpsSyncSource, _ map[string]string) bool {
			for i := range sources {
				delete(sources[i].excluded, "halt-job.yaml")
			}
			return allowed
		}},
		{"identity excluded", "operator-identity-excluded", func(sources []gitOpsSyncSource, _ map[string]string) bool {
			for i := range sources {
				sources[i].excluded["cli-identities.yaml"] = true
			}
			return allowed
		}},
		{"namespace hidden with invocation", "halt-invocation-mixed", func(_ []gitOpsSyncSource, files map[string]string) bool {
			files["halt-job.yaml"] += "\n---\n" + files["halt-identity.yaml"]
			delete(files, "halt-identity.yaml")
			return allowed
		}},
		{"identity deleted", "operator-identity-missing", func(_ []gitOpsSyncSource, files map[string]string) bool {
			delete(files, "cli-identities.yaml")
			return allowed
		}},
		{"extra automatic job", "operator-sync-kind", func(_ []gitOpsSyncSource, files map[string]string) bool {
			files["second-job.yaml"] = strings.Replace(files["halt-job.yaml"], "name: kanz-halt", "name: second-halt", 1)
			return allowed
		}},
		{"project denies namespace", "operator-project-denied", func(_ []gitOpsSyncSource, _ map[string]string) bool { return false }},
		{"include hides identities", "operator-sync-path", func(sources []gitOpsSyncSource, _ map[string]string) bool {
			for i := range sources {
				sources[i].includeRaw = "halt-identity.yaml"
			}
			return allowed
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			sources := slices.Clone(baseline)
			for i := range sources {
				sources[i].excluded = maps.Clone(sources[i].excluded)
			}
			files := maps.Clone(original)
			problems := operatorGitOpsProblems(sources, files, test.edit(sources, files))
			if !slices.ContainsFunc(problems, func(problem string) bool { return strings.HasPrefix(problem, test.code+":") }) {
				t.Fatalf("mutation escaped its intended control %q: %v", test.code, problems)
			}
		})
	}
}
