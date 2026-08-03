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

	// Every ApplicationSet, not just the app-of-apps: a one-shot Job under the
	// preview generator's path is exactly as un-rerunnable as one under the main
	// app's. The sync surface is modelled once in gitops_sync_surface_test.go, so
	// there is one parser of directory.exclude rather than one per guard.
	sources := gitOpsSyncSources(t, root)
	if len(sources) == 0 {
		t.Fatal("found no ApplicationSet under infra/gitops — the scanner is broken, not the " +
			"estate. A guard that examines nothing passes for the wrong reason")
	}

	checked := 0

	for _, src := range sources {
		if len(src.paths) == 0 {
			t.Fatalf("%s (%s) declares no sync path this guard can resolve — it would then be "+
				"exempt from every question below without saying so", src.manifest, src.name)
		}
		for _, p := range src.paths {
			for rel, abs := range syncedFiles(t, root, src, p) {
				if ext := strings.ToLower(filepath.Ext(rel)); ext != ".yaml" && ext != ".yml" {
					continue
				}
				body, err := os.ReadFile(abs)
				if err != nil {
					t.Fatalf("read %s: %v", abs, err)
				}
				if !manifestDeclaresJob(t, string(body)) {
					continue
				}
				checked++

				if src.excluded[rel] {
					// Named as not-synced. It is then applied by its own tooling, and
					// the one-shot property is somebody else's contract to keep.
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
					"or add %q to %s's directory.exclude list, which says in writing that something "+
					"else applies it.", rel, p, rel, src.manifest)
			}
		}
	}

	if checked == 0 {
		t.Error("found no Job manifests in any synced path — either the paths moved or the Job " +
			"detection broke. Both mean this guard stopped guarding")
	}

	// The dead-entry check that used to live here now covers every ApplicationSet
	// rather than this one guard's view of the app-of-apps: see
	// TestNoGitOpsExclusionOutlivesItsFile in gitops_sync_surface_test.go. Its
	// question — does this exclusion still name a file? — was never specific to
	// Jobs, and two copies of it would drift.
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
