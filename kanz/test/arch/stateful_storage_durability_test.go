package arch

import (
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A STATEFUL WORKLOAD THAT WRITES TO A NON-CLAIMED PATH IS A ONE-SHOT SETUP STEP
// WEARING A STATEFULSET'S CLOTHES.
//
// This is the storage half of #92, and it is the half that is invisible from the
// thing people look at. A JetStream stream created `--storage=file` reads as
// durable in `nats stream info` no matter what is underneath it: on an emptyDir
// the file lands on the node's ephemeral disk and the whole spine is gone the
// moment the pod is rescheduled. Nothing in the stream config says so. The
// recurrence this issue records is exactly that shape — the rig lost JetStream
// state on a host reboot, nothing re-created the streams, and compliance and
// tv-sync reached ~495 restarts crash-looping on "stream not found".
//
// The re-provisioning half is already guarded
// (TestEveryGitOpsSyncedJobIsRerunnableOrNamed): a Job in a synced path must be a
// re-runnable hook. Nothing guarded the storage half, so a StatefulSet could lose
// its volumeClaimTemplate — or keep the claim and quietly stop mounting it — and
// every existing guard would stay green. deployability_test.go's volume check is
// the nearest neighbour and does not reach this: it compares
// spec.template.spec.volumes against volumeMounts, and a volumeClaimTemplate is
// not a pod volume.
//
// DEFAULT-DENY over every StatefulSet in the estate, because the defect is a
// class, not an instance. A StatefulSet is the kind Kubernetes offers for "this
// pod's identity and disk outlive the pod"; one that claims nothing has taken the
// stable name and left the disk behind, which is the failure mode that looks
// healthiest.
func TestEveryStatefulSetClaimsThePersistentStorageItMounts(t *testing.T) {
	sets := statefulSetsUnderInfra(t)
	if len(sets) == 0 {
		t.Fatal("found no StatefulSet under infra/ — the walk or the decode is broken, not the " +
			"estate. A guard that examines nothing passes for the wrong reason")
	}

	// Named exemptions, keyed "<path>#<name>". Empty on purpose: every StatefulSet
	// in this estate carries a claim today, so there is nothing to excuse. An entry
	// here is a written statement that this workload's data may be lost on
	// reschedule, and it must carry the issue that retires it.
	exempt := map[string]string{}
	used := map[string]bool{}

	for _, s := range sets {
		key := s.file + "#" + s.name
		if why, ok := exempt[key]; ok {
			used[key] = true
			t.Logf("%s: exempt from the persistent-claim rule — %s", key, why)
			continue
		}

		if len(s.claims) == 0 {
			t.Errorf("%s declares StatefulSet %q with no volumeClaimTemplates.\n\n"+
				"A StatefulSet without a claim keeps a stable pod NAME and nothing else: its data "+
				"lives on the node's ephemeral disk and is destroyed by any reschedule, while every "+
				"readiness probe and every `stream info`/`SHOW TABLES` still reports healthy. That "+
				"is the #92 failure mode — the state is gone and only the workloads that depend on "+
				"it say so, by crash-looping.\n\n"+
				"Either add a volumeClaimTemplate for the path it writes to, or add %q to this "+
				"guard's exempt map with the reason and the issue that retires it.", s.file, s.name, key)
			continue
		}

		for _, c := range s.claims {
			if !s.mounted[c] {
				t.Errorf("%s: StatefulSet %q declares volumeClaimTemplate %q that no container mounts.\n\n"+
					"An unmounted claim is the most convincing shape this defect takes: the PVC is "+
					"created, `kubectl get pvc` shows it Bound, and the process writes to the "+
					"container filesystem anyway. Mount it at the path the workload actually writes "+
					"to.", s.file, s.name, c)
			}
			if s.podVolumes[c] {
				t.Errorf("%s: StatefulSet %q declares BOTH a volumeClaimTemplate %q and a pod volume of "+
					"the same name.\n\n"+
					"The pod volume wins, so the claim is created, bound, billed — and unused. The "+
					"mount that looks persistent is whatever the pod volume is, usually an emptyDir. "+
					"Rename one of them.", s.file, s.name, c)
			}
		}
	}

	// Dead-entry check: an exemption that no longer names a real StatefulSet has
	// outlived its repair, and the next reader takes it as evidence the case was
	// considered.
	var dead []string
	for key := range exempt {
		if !used[key] {
			dead = append(dead, key)
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("these exemptions name no StatefulSet that exists:\n\n  %s\n\n"+
			"Remove them. A stale exemption is a standing permission nobody granted.",
			strings.Join(dead, "\n  "))
	}
}

// THE WHOLE CHAIN, OR THE GUARD PROVES NOTHING.
//
// "Streams survive a broker restart" (#92's Verified-when) needs four links, and
// each one is separately deletable by a change that looks harmless:
//
//	stream --storage=file  →  nats.conf store_dir  →  a volumeMount  →  a claim
//
// Break any one and the other three still read as correct. `--storage=file` on an
// emptyDir is durable-looking and ephemeral. A claim mounted at /data with
// store_dir pointing at /tmp writes past the disk it was given. This asserts the
// links join up, on the manifests production applies — the same files
// test/backing/up.sh and test/mtls/up.sh extract their bootstrap script from, so
// what CI runs and what this reads cannot drift.
//
// Empirically checked 2026-08-04 against the dev rig (`docker restart
// kanz-ci-nats`): all 15 streams and a marker message came back with the marker's
// original receive timestamp. That proves the mechanism, not the PVC binding — a
// container restart is not a reschedule onto a new node, which is precisely why
// the manifest links below need a guard rather than a memory of a green run.
func TestJetStreamStorageChainReachesThePersistentClaim(t *testing.T) {
	root := moduleRoot(t)
	natsFile := filepath.Join(root, "infra", "nats", "nats.yaml")
	bootstrapFile := filepath.Join(root, "infra", "nats", "bootstrap-job.yaml")

	// --- link 1: every stream is created on disk ---------------------------
	//
	// Comment-stripped, because bootstrap-job.yaml narrates its own flags at
	// length and a needle satisfied by prose is a dead needle — the mistake
	// nats_bootstrap_posture_test.go's stripYAMLComments was written to undo.
	script := stripYAMLComments(configMapValue(t, bootstrapFile, "nats-bootstrap", "bootstrap.sh"))
	joined := shellContinuation.ReplaceAllString(script, " ")

	adds := 0
	for _, line := range strings.Split(joined, "\n") {
		if !strings.Contains(line, "stream add") {
			continue
		}
		adds++
		if !strings.Contains(line, "--storage=file") {
			t.Errorf("bootstrap.sh creates a stream without --storage=file:\n\n  %s\n\n"+
				"JetStream's default is file storage, so an omission here is survivable — but it is "+
				"not stated, and the next edit that adds --storage=memory to 'make the tests faster' "+
				"reads as a tuning change and silently makes the spine a cache.",
				strings.TrimSpace(line))
		}
	}
	if adds == 0 {
		t.Fatal("found no `stream add` in bootstrap.sh — the extraction or the comment-stripping " +
			"broke, and this guard just passed by reading nothing. test/backing/up.sh parses the " +
			"same block, so it would break too")
	}
	if strings.Contains(joined, "--storage=memory") {
		t.Error("bootstrap.sh creates a stream with --storage=memory. Every FACT on that stream is " +
			"lost when the broker process exits — not on a node failure, on a routine restart. The " +
			"only memory-storage streams in this repository are scratch streams in " +
			"internal/bustest, which exist for the length of one test")
	}

	// --- link 2: the server writes where the config says --------------------
	conf := stripYAMLComments(configMapValue(t, natsFile, "nats-config", "nats.conf"))
	m := storeDirPattern.FindStringSubmatch(conf)
	if m == nil {
		t.Fatal("infra/nats/nats.yaml's nats.conf declares no jetstream store_dir. Without one " +
			"nats-server picks a temporary directory, so file-storage streams are written to a path " +
			"no volume backs and the 12Gi claim below holds nothing")
	}
	storeDir := strings.Trim(m[1], `"'`)

	// --- links 3 and 4: that path is a mounted claim ------------------------
	sets := statefulSetsUnderInfra(t)
	var natsSet *infraStatefulSet
	for i := range sets {
		if strings.HasSuffix(sets[i].file, "infra/nats/nats.yaml") && sets[i].name == "nats" {
			natsSet = &sets[i]
			break
		}
	}
	if natsSet == nil {
		t.Fatal("no StatefulSet named `nats` in infra/nats/nats.yaml — the broker was renamed or " +
			"changed kind, and this guard can no longer see the thing it protects")
	}

	volume, mountPath := "", ""
	for name, paths := range natsSet.mounts {
		for _, p := range paths {
			if (storeDir == p || strings.HasPrefix(storeDir, strings.TrimSuffix(p, "/")+"/")) &&
				len(p) > len(mountPath) {
				volume, mountPath = name, p
			}
		}
	}
	if volume == "" {
		t.Fatalf("nats.conf writes JetStream to %q, but the nats container mounts no volume at or "+
			"above that path (mounts: %v).\n\n"+
			"So the stream files land on the container's writable layer. Every stream and every "+
			"message on it is destroyed by a reschedule, and the 12Gi claim — if one is still "+
			"declared — sits empty next to it.", storeDir, sortedMountPaths(natsSet))
	}

	claimed := false
	for _, c := range natsSet.claims {
		if c == volume {
			claimed = true
		}
	}
	if !claimed {
		t.Errorf("nats.conf writes JetStream to %q, mounted from volume %q — but %q is a pod volume, "+
			"not a volumeClaimTemplate.\n\n"+
			"A pod volume dies with the pod. An emptyDir here is the exact recurrence #92 records: "+
			"the streams read as file-storage and healthy right up until the broker is rescheduled, "+
			"and then every stream-dependent service crash-loops on \"stream not found\" with "+
			"nothing in the broker's own state to explain it.", storeDir, volume, volume)
	}
}

var (
	// A trailing backslash joins a shell command across lines. Without folding
	// them, `nats ... stream add "$name" --defaults \` and the line carrying
	// --storage=file are two different strings and the check reads only the first.
	shellContinuation = regexp.MustCompile(`\\[ \t]*\n[ \t]*`)
	storeDirPattern   = regexp.MustCompile(`(?m)^\s*store_dir:\s*(\S+)`)
)

// infraStatefulSet is one StatefulSet reduced to the four facts this file asks
// about: what it claims, what it mounts and where, and what pod volumes could
// shadow a claim.
type infraStatefulSet struct {
	file       string   // repo-relative, forward slashes
	name       string   //
	claims     []string // spec.volumeClaimTemplates[].metadata.name
	mounted    map[string]bool
	mounts     map[string][]string // volume name -> mount paths
	podVolumes map[string]bool     // spec.template.spec.volumes[].name
}

type statefulSetDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers     []statefulSetContainer `yaml:"containers"`
				InitContainers []statefulSetContainer `yaml:"initContainers"`
				Volumes        []struct {
					Name string `yaml:"name"`
				} `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
		VolumeClaimTemplates []struct {
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		} `yaml:"volumeClaimTemplates"`
	} `yaml:"spec"`
}

type statefulSetContainer struct {
	VolumeMounts []struct {
		Name      string `yaml:"name"`
		MountPath string `yaml:"mountPath"`
	} `yaml:"volumeMounts"`
}

// statefulSetsUnderInfra decodes every StatefulSet under infra/. Structural, not
// grepped: `kind: StatefulSet` appears in comments in several of these files
// (pod_disruption_and_drain_test.go's own header talks about one), and no regex
// distinguishes a claim template's metadata.name from the workload's.
func statefulSetsUnderInfra(t *testing.T) []infraStatefulSet {
	t.Helper()
	root := moduleRoot(t)

	var out []infraStatefulSet
	for _, path := range infraYAMLFiles(t) {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatalf("relativise %s: %v", path, err)
		}
		rel = filepath.ToSlash(rel)

		dec := yaml.NewDecoder(strings.NewReader(readFile(t, path)))
		for {
			var doc statefulSetDoc
			derr := dec.Decode(&doc)
			if errors.Is(derr, io.EOF) {
				break
			}
			if derr != nil {
				// Not every file under infra/ is a manifest this decoder can read
				// (CRDs carry deeply nested schemas), and a parse failure there is not
				// evidence about storage. Skip the rest of the file rather than fail
				// the estate on it.
				break
			}
			if doc.Kind != "StatefulSet" {
				continue
			}

			s := infraStatefulSet{
				file:       rel,
				name:       doc.Metadata.Name,
				mounted:    map[string]bool{},
				mounts:     map[string][]string{},
				podVolumes: map[string]bool{},
			}
			for _, c := range doc.Spec.VolumeClaimTemplates {
				s.claims = append(s.claims, c.Metadata.Name)
			}
			for _, v := range doc.Spec.Template.Spec.Volumes {
				s.podVolumes[v.Name] = true
			}
			// initContainers count: a claim mounted only by an init container is
			// still mounted, and saying otherwise would fail a legitimate shape.
			all := append(append([]statefulSetContainer{},
				doc.Spec.Template.Spec.Containers...),
				doc.Spec.Template.Spec.InitContainers...)
			for _, c := range all {
				for _, vm := range c.VolumeMounts {
					s.mounted[vm.Name] = true
					s.mounts[vm.Name] = append(s.mounts[vm.Name], vm.MountPath)
				}
			}
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].name < out[j].name
	})
	return out
}

// configMapValue returns one key out of one named ConfigMap in a multi-document
// manifest. Reading the ConfigMap by name rather than slicing the file by line
// offsets means a document reordering cannot silently point this at the wrong
// script.
func configMapValue(t *testing.T, path, name, key string) string {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(readFile(t, path)))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Data map[string]string `yaml:"data"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("%s: parse as YAML: %v", path, err)
		}
		if doc.Kind != "ConfigMap" || doc.Metadata.Name != name {
			continue
		}
		v, ok := doc.Data[key]
		if !ok {
			t.Fatalf("%s: ConfigMap %q has no key %q — it was renamed, and every check built on it "+
				"would otherwise read an empty string and pass", path, name, key)
		}
		return v
	}
	t.Fatalf("%s: no ConfigMap named %q", path, name)
	return ""
}

// sortedMountPaths renders a StatefulSet's mounts for a failure message, so the
// person reading it can see what the container DOES mount next to what the config
// asked for.
func sortedMountPaths(s *infraStatefulSet) []string {
	var out []string
	for name, paths := range s.mounts {
		for _, p := range paths {
			out = append(out, name+" -> "+p)
		}
	}
	sort.Strings(out)
	return out
}
