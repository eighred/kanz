// Command kanz-pin-digests rewrites every production manifest image reference
// from a mutable tag to the immutable digest a release just published.
//
// WHY THIS EXISTS. release.yml pins everything AFTER the build to
// ${IMAGE}@${digest} — trivy, cosign and the SBOM all attest one specific image
// — and then nothing consumed that digest. Every manifest under infra/ said
// :latest, so the supply chain terminated at the registry: the signature was
// real and the cluster ran whatever :latest resolved to at pull time. Two
// consequences followed. :latest defaults imagePullPolicy to Always, so
// replicas rescheduled at different moments can run DIFFERENT code under one
// Deployment; and there is no previous digest to roll back to, which is the
// primitive the TUI's rollback is supposed to wrap.
//
// It is a separate, tested binary rather than a shell step inside the workflow
// because the workflow cannot be run here — a rewriter that mangles 39
// production manifests is not something to discover on a release.
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func main() {
	digestsDir := flag.String("digests", "", "directory of files named <service> containing sha256:<hex>")
	infraDir := flag.String("infra", "", "root of the manifest tree to rewrite")
	org := flag.String("org", "eighred", "registry org whose images are pinned")
	flag.Parse()
	if *digestsDir == "" || *infraDir == "" {
		fmt.Fprintln(os.Stderr, "usage: kanz-pin-digests -digests <dir> -infra <dir> [-org <org>]")
		os.Exit(2)
	}

	digests, err := loadDigests(*digestsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load digests: %v\n", err)
		os.Exit(1)
	}
	if len(digests) == 0 {
		// Fail loud: a release that pinned nothing has silently shipped :latest.
		fmt.Fprintf(os.Stderr, "no digests found in %s — refusing to report success on a no-op pin\n", *digestsDir)
		os.Exit(1)
	}

	res, err := Pin(os.DirFS(*infraDir), *infraDir, *org, digests)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pin: %v\n", err)
		os.Exit(1)
	}
	for _, line := range res.Report() {
		fmt.Println(line)
	}
	if res.Rewritten == 0 {
		fmt.Fprintln(os.Stderr, "no image reference was rewritten — the manifests do not match the expected shape")
		os.Exit(1)
	}
	// A RELEASE THAT LEAVES ONE OF OUR IMAGES ON A MUTABLE TAG FAILS (#771).
	//
	// Result.Unpinned has been reported since this tool was written and nothing
	// acted on it, which is the hole: the dead-exemption arm in
	// test/arch/supplychain_test.go fires only when an exemption OUTLIVES its
	// repair — the manifest is pinned and the line is still there. It cannot fire
	// the other way. If a release ran and this tool silently skipped a manifest,
	// the exemption covering it kept covering it forever, the guard stayed green,
	// and nothing anywhere said the image was still on a mutable tag. A temporary
	// exemption became permanent without anyone deciding it should.
	//
	// THE CHECK BELONGS HERE, WHERE THE ACTION IS, rather than in a guard that
	// infers it afterwards from git history. This runs at the exact moment the
	// promise ("the first tagged release publishes its digest and pin-digests
	// rewrites this") is either kept or broken, and it needs no release tag, no
	// join date and no shallow-clone caveat to say which.
	//
	// IT NEEDS NO EXEMPTION LIST, and that is measured rather than assumed. The
	// release matrix builds 30 services; infra/ references 30 eighred images
	// through an image:/value: key; the difference is EMPTY (2026-09-03). So every
	// image this rewriter can see is one this release published, and a service in
	// Unpinned means one of exactly two things — both defects:
	//
	//   - the service is missing from release.yml's build matrix, so no digest was
	//     published for an image the estate deploys; or
	//   - a manifest names a service that does not exist, so the reference is dead.
	//
	// The kustomize `images:` list in infra/gitops/preview-applicationset.yaml is
	// deliberately NOT reachable by this tool's pattern (it carries bare quoted
	// strings, not an image: key), so per-PR preview tags cannot reach this arm.
	// That is why the one standing entry in mutableTagExempt is unaffected.
	if len(res.Unpinned) > 0 {
		fmt.Fprintf(os.Stderr, "REFUSING THE RELEASE: %d service(s) referenced by a manifest were left on a "+
			"mutable tag because this release published no digest for them: %s\n\n"+
			"Either the service is missing from release.yml's build matrix — in which case the estate "+
			"deploys an image this release did not build — or a manifest names a service that no longer "+
			"exists. Both are defects. Reporting them and exiting 0 is how a temporary exemption in "+
			"test/arch/supplychain_test.go becomes permanent without anyone deciding it should (#771).\n",
			len(res.Unpinned), strings.Join(res.Unpinned, ", "))
		os.Exit(1)
	}
}

// loadDigests reads <dir>/<service> files whose contents are sha256:<hex>.
func loadDigests(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	digestRe := regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		d := strings.TrimSpace(string(b))
		if !digestRe.MatchString(d) {
			return nil, fmt.Errorf("%s: %q is not a sha256 digest", e.Name(), d)
		}
		out[e.Name()] = d
	}
	return out, nil
}

// Result reports what a Pin run changed.
type Result struct {
	Rewritten int
	Files     []string
	Unpinned  []string // services referenced by a manifest but absent from the digest set
}

// Report renders the result deterministically, for a workflow log and a PR body.
func (r Result) Report() []string {
	out := []string{fmt.Sprintf("pinned %d image reference(s) across %d file(s)", r.Rewritten, len(r.Files))}
	sort.Strings(r.Files)
	for _, f := range r.Files {
		out = append(out, "  "+f)
	}
	if len(r.Unpinned) > 0 {
		sort.Strings(r.Unpinned)
		out = append(out, fmt.Sprintf("LEFT ON A MUTABLE TAG (%d) — no digest was published for these:", len(r.Unpinned)))
		for _, s := range r.Unpinned {
			out = append(out, "  "+s)
		}
	}
	return out
}

// Pin rewrites `image: <registry>/<org>/<service>:<tag>` to `...@<digest>` for
// every service present in digests. A reference already pinned to a digest is
// rewritten too, so re-running a release moves it forward rather than skipping it.
func Pin(fsys fs.FS, root, org string, digests map[string]string) (Result, error) {
	// Captures: 1 the `image:` key with its leading spacing, 2 registry/org/service,
	// 3 the service name. The trailing group consumes the existing :tag or @digest.
	//
	// The optional `- ` accepts the list-item form (`- image: ...`). Every manifest
	// in infra/ today uses the plain form, but both are valid YAML and a rewriter
	// that silently skipped one shape would leave a mutable tag behind while
	// reporting success — the failure mode this tool exists to end.
	//
	// `value:` is matched as well as `image:`, and that is not incidental. The
	// operator launches provisioning Jobs with the image named by its
	// PROVISIONER_IMAGE env var (operator-deploy.yaml), which is a `value:`, not an
	// `image:`. Pinning only `image:` keys would leave the one image the platform
	// launches AT RUNTIME on a mutable tag — the least visible place for it to
	// drift. The match still demands a full <registry>/<org>/<service> shape, so an
	// unrelated `value:` cannot be caught by it.
	ref := regexp.MustCompile(`(?m)^(\s*(?:-\s+)?(?:image|value):\s*)([A-Za-z0-9.\-]+/` + regexp.QuoteMeta(org) + `/([A-Za-z0-9._-]+))(?::[^\s]+|@sha256:[0-9a-f]{64})`)

	var res Result
	unpinned := map[string]bool{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || (!strings.HasSuffix(p, ".yaml") && !strings.HasSuffix(p, ".yml")) {
			return nil
		}
		full := filepath.Join(root, filepath.FromSlash(p))
		body, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		changed := 0
		out := ref.ReplaceAllStringFunc(string(body), func(m string) string {
			g := ref.FindStringSubmatch(m)
			svc := g[3]
			digest, ok := digests[svc]
			if !ok {
				unpinned[svc] = true
				return m
			}
			changed++
			return g[1] + g[2] + "@" + digest
		})
		if changed > 0 {
			if err := os.WriteFile(full, []byte(out), 0o644); err != nil {
				return err
			}
			res.Rewritten += changed
			res.Files = append(res.Files, filepath.ToSlash(p))
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	for s := range unpinned {
		res.Unpinned = append(res.Unpinned, s)
	}
	return res, nil
}
