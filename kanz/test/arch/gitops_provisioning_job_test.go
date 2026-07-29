package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// EVERY PROVISIONING JOB IN A GITOPS-SYNCED PATH MUST BE RE-RUNNABLE, OR NAMED.
//
// A Kubernetes Job runs once. Argo CD will not re-run one that has already
// completed — the object still matches git, so a sync is a no-op — which means a
// plain Job in a synced path provisions the estate EXACTLY ONCE, forever. If what
// it created is later lost, GitOps reports Synced and Healthy over a cluster
// missing the thing the Job existed to create, and recovery is a command a human
// has to remember mid-incident.
//
// That is not hypothetical here. The NATS bootstrap Job creates every JetStream
// stream; when the rig lost JetStream state on a host reboot, nothing re-created
// them and compliance and tv-sync reached ~495 restarts crash-looping on "stream
// not found" (see the header of bootstrap-job-dev-plaintext.yaml). Issue #92.
//
// Marking a Job `argocd.argoproj.io/hook` makes it re-run on every sync. That is
// only safe when the Job is idempotent, which is a property of the script, not of
// this annotation — so the annotation is an ASSERTION by whoever adds it, and the
// exemption list below is where the alternative gets said out loud.
//
// DEFAULT-DENY. A Job that is neither hooked nor named fails. The prior state of
// this repository is exactly what that prevents: the sync path excluded
// `*-job.yaml`, which silently dropped bootstrap-job.yaml (the file that creates
// the spine) while admitting bootstrap-job-dev-plaintext.yaml, whose own header
// says never to apply it to a cluster with SPIRE. A name-shaped rule could not
// express the distinction, and nothing failed when it got it backwards.
func TestEveryGitOpsSyncedJobIsRerunnableOrNamed(t *testing.T) {
	root := moduleRoot(t)

	paths, excluded := applicationSetSyncConfig(t, root)
	if len(paths) == 0 {
		t.Fatal("parsed no component paths from infra/gitops/applicationset.yaml — the scanner is " +
			"broken, not the estate. A guard that examines nothing passes for the wrong reason")
	}

	seenExcluded := map[string]bool{}
	checked := 0

	for _, rel := range paths {
		dir := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(rel, "kanz/")))
		manifests, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
		if err != nil {
			t.Fatalf("glob %s: %v", dir, err)
		}
		for _, m := range manifests {
			base := filepath.Base(m)
			body, err := os.ReadFile(m)
			if err != nil {
				t.Fatalf("read %s: %v", m, err)
			}
			if !manifestDeclaresJob(t, string(body)) {
				continue
			}
			checked++

			if excluded[base] {
				// Named as not-synced. It is then applied by its own tooling, and
				// the one-shot property is somebody else's contract to keep.
				seenExcluded[base] = true
				continue
			}
			if jobHasArgoHook(t, string(body)) {
				continue
			}
			t.Errorf("%s declares a Job in GitOps-synced path %q, but it is neither a re-runnable "+
				"sync hook nor excluded from syncing\n\n"+
				"Argo CD does not re-run a completed Job, so whatever this provisions is created "+
				"once and never repaired. When it is lost the cluster keeps reporting Synced and "+
				"Healthy, and recovery becomes a command someone has to remember during an "+
				"incident.\n\n"+
				"Either add, if the Job is idempotent:\n"+
				"    argocd.argoproj.io/hook: PostSync\n"+
				"    argocd.argoproj.io/hook-delete-policy: BeforeHookCreation\n"+
				"or add %q to the ApplicationSet's directory.exclude list, which says in writing "+
				"that something else applies it.", base, rel, base)
		}
	}

	if checked == 0 {
		t.Error("found no Job manifests in any synced path — either the paths moved or the Job " +
			"detection broke. Both mean this guard stopped guarding")
	}

	// DEAD-ENTRY CHECK. An exclusion naming a file that no longer declares a Job
	// in a synced path is a claim nobody exercises: it reads as a considered
	// decision while protecting nothing, and it is the entry a future reader
	// trusts when adding the next one.
	for name := range excluded {
		if seenExcluded[name] {
			continue
		}
		if fileExistsInSyncedPaths(t, root, paths, name) {
			continue // present, but declares no Job — excluded for another reason
		}
		t.Errorf("ApplicationSet directory.exclude names %q, but no file by that name exists in any "+
			"synced component path\n\n"+
			"Either it was renamed or removed. An exclusion that outlives its subject makes the "+
			"list look considered while it is stale — delete the entry.", name)
	}
}

// applicationSetSyncConfig reads the component paths and the exclude list from
// the ApplicationSet itself rather than restating them here. A second copy of
// this list is the failure it exists to prevent: it would keep passing after
// someone adds a component, which is the moment a new unhooked Job appears.
func applicationSetSyncConfig(t *testing.T, root string) (paths []string, excluded map[string]bool) {
	t.Helper()
	body := readFile(t, filepath.Join(root, "infra", "gitops", "applicationset.yaml"))

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
					Source struct {
						Directory struct {
							Exclude string `yaml:"exclude"`
						} `yaml:"directory"`
					} `yaml:"source"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("parse applicationset.yaml: %v", err)
	}

	for _, g := range doc.Spec.Generators {
		for _, inner := range g.Matrix.Generators {
			for _, el := range inner.List.Elements {
				if p := el["path"]; p != "" {
					paths = append(paths, p)
				}
			}
		}
	}

	// exclude is a brace-list glob: "{a.yaml,b.sh,c.yaml}". Only literal names are
	// treated as exclusions — a wildcard entry is rejected outright, because a
	// pattern is what got this wrong before: `*-job.yaml` dropped the bootstrap
	// that creates the spine and admitted the dev-plaintext one that must never
	// run against SPIRE. Excluding by shape cannot express "safe to sync".
	excluded = map[string]bool{}
	raw := strings.Trim(strings.TrimSpace(doc.Spec.Template.Spec.Source.Directory.Exclude), "{}")
	for _, e := range strings.Split(raw, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if strings.ContainsAny(e, "*?[") {
			t.Errorf("ApplicationSet directory.exclude contains the pattern %q\n\n"+
				"Exclusions must be literal filenames. A glob selects by NAME SHAPE, and the "+
				"shape does not correlate with whether a manifest is safe to sync: `*-job.yaml` "+
				"excluded bootstrap-job.yaml (which creates every JetStream stream) while "+
				"admitting bootstrap-job-dev-plaintext.yaml (whose header says never to apply it "+
				"to a cluster with SPIRE). Name the files.", e)
			continue
		}
		excluded[e] = true
	}
	return paths, excluded
}

// manifestDeclaresJob reports whether any document in a multi-document manifest
// is a batch Job. Parsed structurally rather than grepped: `kind: Job` also
// appears inside comments and inside CronJob templates, and a guard that fires on
// prose is a guard people learn to route around.
func manifestDeclaresJob(t *testing.T, body string) bool {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(body))
	for {
		var d struct {
			Kind string `yaml:"kind"`
		}
		if err := dec.Decode(&d); err != nil {
			return false
		}
		if d.Kind == "Job" {
			return true
		}
	}
}

// jobHasArgoHook reports whether every Job document in the manifest carries the
// hook annotation. EVERY, not any: a file holding two Jobs where only one is
// hooked leaves the other exactly as un-rerunnable as before.
func jobHasArgoHook(t *testing.T, body string) bool {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(body))
	jobs, hooked := 0, 0
	for {
		var d struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"metadata"`
		}
		if err := dec.Decode(&d); err != nil {
			break
		}
		if d.Kind != "Job" {
			continue
		}
		jobs++
		if d.Metadata.Annotations["argocd.argoproj.io/hook"] != "" {
			hooked++
		}
	}
	return jobs > 0 && jobs == hooked
}

func fileExistsInSyncedPaths(t *testing.T, root string, paths []string, name string) bool {
	t.Helper()
	for _, rel := range paths {
		dir := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(rel, "kanz/")))
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}
