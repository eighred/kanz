package arch

import (
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// postureLabel is the label a manifest carries to say it is a rig artifact and
// not a production one. rig_dev_posture_test.go already proves the right files
// carry it and that every Secret in them does. This file is what makes carrying
// it MEAN something: the label decides what GitOps refuses to sync.
const (
	postureLabel   = "kanz.eighred.com/posture"
	posturePropDev = "dev-only"
)

// A MANIFEST THAT SAYS "DEV-ONLY" MUST BE UNREACHABLE BY ARGO CD, NOT MERELY
// LABELLED.
//
// The app-of-apps syncs whole directories with directory.recurse and
// automated{prune, selfHeal}. Under that policy the DEFAULT for a file is
// "applied to every registered cluster, and reapplied if a human deletes it" —
// so a manifest whose own header says "do not apply this to a cluster that
// trades real money" is, absent an entry in directory.exclude, exactly as
// deployed as every production manifest beside it.
//
// That was the state this guard was written for (#224). kanz/infra/deploy held
// two files labelled dev-only — postgres-dev.yaml and rig-dev-secrets.yaml —
// and neither was excluded. The first stands up a postgres/postgres database in
// the TRADING namespace under the label app: postgres, which is precisely the
// selector allow-postgres-egress and allow-services-to-postgres use to grant
// every pod in that namespace access on 5432. The second commits the gateway's
// HS256 signing secret as a real Secret in the gateway's own namespace, and the
// gateway's config layer accepts a JWT secret alone as a complete auth
// configuration with nothing distinguishing a dev credential from a production
// one. selfHeal means deleting either one is temporary.
//
// The label had existed, and had been guarded, the whole time. It was read by
// nothing that mattered. This is the guard that reads it.
//
// GENERAL OVER APPLICATIONSETS AND OVER PATHS, deliberately. Fixing only
// kanz/infra/deploy in only applicationset.yaml closes one instance of the
// class: infra/gitops holds a second ApplicationSet (the per-PR preview
// generator) that syncs the same directory with the same prune + selfHeal, and
// the main one syncs four paths. Both dimensions are discovered from the files
// rather than restated here, so a new component path or a third ApplicationSet
// is covered on the day it is added rather than the day someone remembers.
func TestEveryDevOnlyManifestIsExcludedFromGitOpsSync(t *testing.T) {
	root := moduleRoot(t)
	sources := gitOpsSyncSources(t, root)

	devOnlyFound := 0
	manifestsScanned := 0

	for _, src := range sources {
		for _, p := range src.paths {
			for rel, abs := range syncedFiles(t, root, src, p) {
				if ext := strings.ToLower(filepath.Ext(rel)); ext != ".yaml" && ext != ".yml" {
					continue
				}
				manifestsScanned++
				if !manifestDeclaresDevOnlyPosture(t, rel, readFile(t, abs)) {
					continue
				}
				devOnlyFound++
				if src.excluded[rel] {
					continue
				}
				t.Errorf("%s/%s carries %s: %s but is NOT excluded from GitOps sync\n\n"+
					"    ApplicationSet: %s (%s)\n"+
					"    synced path:    %s, directory.recurse, automated{prune, selfHeal}\n"+
					"    exclude list:   %s\n\n"+
					"Under that sync policy the default for a file in this tree is applied to every "+
					"registered cluster and reapplied if anyone deletes it. A manifest labelled "+
					"%s exists because it must NOT be — a committed credential, a "+
					"password-in-the-clear database, a workload with no persistence. The label is "+
					"the whole of its protection and Argo CD does not read labels.\n\n"+
					"FIX: add %q to that ApplicationSet's directory.exclude list.\n\n"+
					"Do not reach for a glob. #92 is this repository's record of a name-shaped rule "+
					"getting it exactly backwards: `*-job.yaml` excluded bootstrap-job.yaml, which "+
					"creates every JetStream stream, and admitted bootstrap-job-dev-plaintext.yaml, "+
					"whose header says never to apply it to a cluster with SPIRE. A name shape "+
					"cannot express \"is this safe to sync\"; only the list can.",
					p, rel, postureLabel, posturePropDev,
					src.manifest, src.name, p, src.excludeRaw, posturePropDev, rel)
			}
		}
	}

	// NON-VACUITY. Every failure mode of this guard is silent: a moved directory,
	// a renamed label, an ApplicationSet that stops parsing. Each one leaves a
	// green test that inspected nothing, which is worse than no test because it
	// reports the estate is fine.
	if len(sources) == 0 {
		t.Fatal("found no ApplicationSet under infra/gitops — this guard would pass having " +
			"checked nothing")
	}
	if manifestsScanned == 0 {
		t.Fatal("scanned zero manifests across every synced path — the paths moved or the walk " +
			"broke; either way this guard stopped guarding")
	}
	if devOnlyFound == 0 {
		t.Fatalf("no manifest under any synced path carries %s: %s. That label is what this guard "+
			"reads, so a zero here means it is asserting nothing — either the rig's dev manifests "+
			"left the synced tree (delete this guard and say so) or the label was renamed (teach "+
			"this guard the new spelling).", postureLabel, posturePropDev)
	}
}

// AN EXCLUSION THAT OUTLIVES ITS FILE IS A CLAIM NOBODY EXERCISES.
//
// This is the OPPOSITE defect from the one above and it needs the opposite fix,
// so the two are separate tests rather than two arms of one: nothing here is
// being wrongly synced. The entry simply protects nothing while making the list
// read as considered — and a considered-looking list is what the next person
// copies when they add the next entry, which is how the preview generator ended
// up excluding three names, none of which named a file under the one path it
// syncs, one of them the `*-job.yaml` glob #92 forbids outright.
//
// Repair is DELETE THE ENTRY, never "add a file so the entry matches".
func TestNoGitOpsExclusionOutlivesItsFile(t *testing.T) {
	root := moduleRoot(t)
	sources := gitOpsSyncSources(t, root)

	entriesChecked := 0

	for _, src := range sources {
		present := map[string]bool{}
		for _, p := range src.paths {
			for rel := range syncedFiles(t, root, src, p) {
				present[rel] = true
			}
		}

		names := make([]string, 0, len(src.excluded))
		for name := range src.excluded {
			names = append(names, name)
		}
		sort.Strings(names)

		for _, name := range names {
			entriesChecked++
			if present[name] {
				continue
			}
			t.Errorf("%s (%s) excludes %q from GitOps sync, but no file at that path exists under "+
				"any path it syncs (%s)\n\n"+
				"This is a STALE EXCLUSION, not an unexcluded dev-only manifest — nothing is being "+
				"wrongly applied. The entry was left behind by a rename or a deletion and now "+
				"protects nothing, while making the list look like every name in it was decided on. "+
				"That is the list a future reader trusts when adding the next entry.\n\n"+
				"FIX: delete %q from directory.exclude. Do NOT create a file to satisfy it.",
				src.manifest, src.name, name, strings.Join(src.paths, ", "), name)
		}
	}

	if len(sources) == 0 {
		t.Fatal("found no ApplicationSet under infra/gitops — this guard would pass having " +
			"checked nothing")
	}
	if entriesChecked == 0 {
		t.Fatal("no ApplicationSet under infra/gitops declares a directory.exclude entry. Either " +
			"every exclusion was deleted — in which case the dev-only manifests are being synced " +
			"and TestEveryDevOnlyManifestIsExcludedFromGitOpsSync should be red — or the parser " +
			"stopped finding the field")
	}
}

// gitOpsSyncSource is one ApplicationSet's sync surface: what it applies, and
// what it refuses to. Modelled once and shared, because three guards
// (provisioning-Job re-runnability, dev-only exclusion, stale exclusion) all ask
// questions about the same two fields, and a second parser is a second answer
// that drifts.
type gitOpsSyncSource struct {
	manifest   string   // repo-relative path of the file declaring it, for error messages
	name       string   // metadata.name
	paths      []string // repo-root-relative sync paths, e.g. "kanz/infra/deploy"
	excludeRaw string   // directory.exclude verbatim, so a failure can quote the list
	// excluded holds slash-separated paths RELATIVE TO THE SYNC PATH. Argo CD's
	// own documentation for directory.exclude gives `{config.yaml,env-use2/*}` as
	// an example, which only means anything if the pattern is matched against the
	// path relative to the application's path rather than a bare basename.
	//
	// EVERY EXCLUSION IN THIS REPO IS CURRENTLY A TOP-LEVEL FILE, where the two
	// readings are identical — so nothing here is presently riding on that reading
	// being right. It starts to matter the first time a dev-only manifest lands in
	// a subdirectory, and infra/deploy already has one (tenants/<tenant>/). If a
	// nested exclusion is ever added and Argo CD does not honour it, the reading
	// above is the thing to re-check first.
	excluded map[string]bool
}

// gitOpsSyncSources discovers every ApplicationSet under infra/gitops rather
// than naming the two that exist today. Naming them is how the preview generator
// spent its life outside the #92 rule the main app-of-apps documents at length:
// the rule was enforced by a guard that opened one file by name.
func gitOpsSyncSources(t *testing.T, root string) []gitOpsSyncSource {
	t.Helper()

	dir := filepath.Join(root, "infra", "gitops")
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	sort.Strings(files)

	var sources []gitOpsSyncSource
	for _, f := range files {
		rel := filepath.ToSlash(strings.TrimPrefix(f, root+string(filepath.Separator)))
		body := readFile(t, f)

		dec := yaml.NewDecoder(strings.NewReader(body))
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
			sources = append(sources, gitOpsSyncSource{
				manifest:   rel,
				name:       doc.Metadata.Name,
				paths:      doc.syncPaths(),
				excludeRaw: doc.Spec.Template.Spec.Source.Directory.Exclude,
				excluded:   parseExcludeList(t, rel, doc.Spec.Template.Spec.Source.Directory.Exclude),
			})
		}
	}
	return sources
}

type applicationSetDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Generators []struct {
			Matrix struct {
				Generators []struct {
					List struct {
						Elements []map[string]string `yaml:"elements"`
					} `yaml:"list"`
				} `yaml:"generators"`
			} `yaml:"matrix"`
			List struct {
				Elements []map[string]string `yaml:"elements"`
			} `yaml:"list"`
		} `yaml:"generators"`
		Template struct {
			Spec struct {
				Source struct {
					Path      string `yaml:"path"`
					Directory struct {
						Recurse bool   `yaml:"recurse"`
						Exclude string `yaml:"exclude"`
					} `yaml:"directory"`
				} `yaml:"source"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

// syncPaths returns every repo-relative directory this ApplicationSet syncs.
// Two shapes are in use and both are real: the app-of-apps templates
// source.path from a generator list element, and the preview generator hardcodes
// a literal. A literal containing `{{` is a template placeholder resolved by the
// elements, not a path — counting it would produce a directory that does not
// exist and a guard that fatals on the wrong thing.
func (d applicationSetDoc) syncPaths() []string {
	seen := map[string]bool{}
	var paths []string
	add := func(p string) {
		if p == "" || strings.Contains(p, "{{") || seen[p] {
			return
		}
		seen[p] = true
		paths = append(paths, p)
	}
	for _, g := range d.Spec.Generators {
		for _, inner := range g.Matrix.Generators {
			for _, el := range inner.List.Elements {
				add(el["path"])
			}
		}
		for _, el := range g.List.Elements {
			add(el["path"])
		}
	}
	add(d.Spec.Template.Spec.Source.Path)
	sort.Strings(paths)
	return paths
}

// parseExcludeList reads Argo CD's brace-list glob — "{a.yaml,b.sh}" — into the
// set of literal paths it names.
//
// LITERAL ONLY, and the rejection below is the point rather than an edge case.
// A glob selects by NAME SHAPE, and shape does not correlate with whether a
// manifest is safe to sync: `*-job.yaml` excluded bootstrap-job.yaml, the file
// that creates every JetStream stream, while admitting
// bootstrap-job-dev-plaintext.yaml, whose header says never to apply it to a
// cluster with SPIRE (#92). Rejecting patterns also keeps this parser HONEST for
// its callers — a substring or shape match would let a name mentioned in a
// comment, or a near-miss, read as a decision that was never made.
func parseExcludeList(t *testing.T, manifest, raw string) map[string]bool {
	t.Helper()
	excluded := map[string]bool{}
	for _, e := range strings.Split(strings.Trim(strings.TrimSpace(raw), "{}"), ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if strings.ContainsAny(e, "*?[") {
			t.Errorf("%s: directory.exclude contains the pattern %q\n\n"+
				"Exclusions must be literal paths. A glob selects by name shape, and the shape does "+
				"not correlate with whether a manifest is safe to sync: `*-job.yaml` excluded "+
				"bootstrap-job.yaml (which creates every JetStream stream) while admitting "+
				"bootstrap-job-dev-plaintext.yaml (whose header says never to apply it to a cluster "+
				"with SPIRE). Name the files.", manifest, e)
			continue
		}
		excluded[filepath.ToSlash(e)] = true
	}
	return excluded
}

// syncedFiles returns every file Argo CD would consider under one sync path,
// keyed by its slash-separated path RELATIVE to that sync path — the same key
// directory.exclude is matched against.
//
// It walks recursively because the template sets directory.recurse: per-tenant
// OMS compute already lives at tenants/<tenant>/oms-<tenant>.yaml, so a
// top-level-only listing would miss a whole synced subtree.
func syncedFiles(t *testing.T, root string, src gitOpsSyncSource, syncPath string) map[string]string {
	t.Helper()

	// Sync paths are written repo-root-relative ("kanz/infra/deploy") while these
	// guards run inside the module ("<root>/infra/deploy").
	trimmed := strings.TrimPrefix(syncPath, "kanz/")
	if trimmed == syncPath {
		t.Fatalf("%s (%s) syncs path %q, which is not under the Go module's kanz/ tree. This guard "+
			"resolves sync paths relative to the module root; teach it the new layout rather than "+
			"letting it silently examine nothing.", src.manifest, src.name, syncPath)
	}
	dir := filepath.Join(root, filepath.FromSlash(trimmed))

	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, p)
		if relErr != nil {
			return relErr
		}
		files[filepath.ToSlash(rel)] = p
		return nil
	})
	if err != nil {
		t.Fatalf("%s (%s) syncs %q but walking %s failed: %v — an unreadable sync path makes every "+
			"question about it answer \"nothing found\"", src.manifest, src.name, syncPath, dir, err)
	}
	if len(files) == 0 {
		t.Fatalf("%s (%s) syncs %q but %s contains no files. A guard that walks an empty directory "+
			"passes for the wrong reason.", src.manifest, src.name, syncPath, dir)
	}
	return files
}

// manifestDeclaresDevOnlyPosture reports whether any document in a manifest
// carries the posture label on a `metadata.labels` map.
//
// PARSED, NOT GREPPED, and the distinction has already cost this repository two
// dead guards: the label's own spelling appears in prose throughout this tree
// (this file included), so a strings.Contains would fire on a comment and, worse,
// would report a file as protected because someone wrote the label's name in a
// sentence.
//
// Scoped to `metadata.labels` rather than any map containing the key, because a
// NetworkPolicy's podSelector.matchLabels naming posture: dev-only would be
// SELECTING dev-only pods, not declaring itself one — that manifest belongs in
// the sync, and demanding its exclusion would be the guard arguing for a hole.
// It descends into nested pod templates, where a Deployment or Rollout can carry
// the label on its template's metadata and nowhere else.
func manifestDeclaresDevOnlyPosture(t *testing.T, rel, body string) bool {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(body))
	for {
		var doc any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return false
		}
		if err != nil {
			t.Errorf("%s: parse as YAML: %v\n\nArgo CD applies every manifest in a synced path; one "+
				"it cannot parse is a sync failure, and it is one this guard cannot read a posture "+
				"out of either.", rel, err)
			return false
		}
		if nodeDeclaresDevOnlyPosture(doc) {
			return true
		}
	}
}

func nodeDeclaresDevOnlyPosture(n any) bool {
	switch v := n.(type) {
	case map[string]any:
		if md, ok := v["metadata"].(map[string]any); ok {
			if labels, ok := md["labels"].(map[string]any); ok {
				if s, ok := labels[postureLabel].(string); ok && s == posturePropDev {
					return true
				}
			}
		}
		for _, child := range v {
			if nodeDeclaresDevOnlyPosture(child) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if nodeDeclaresDevOnlyPosture(child) {
				return true
			}
		}
	}
	return false
}
