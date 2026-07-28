package arch

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// THE SSH PLANE IS CANCELLED. This test is that decision, executable.
//
// KANZ_BRAIN.md (2026-07-16, lead): "THE ESTATE IS KUBERNETES-MANAGED. SSH-pushed
// hosts are CANCELLED." The sovereign blueprint still describes, at length, an
// operator TUI pushing pre-compiled binaries to host vectors over SSH pipelines.
// That blueprint is the document a new engineer reads first, and it does not know
// it was overruled — so the cancelled plane arrives as an import, from someone
// implementing a spec in good faith.
//
// The same file names the failure mode this guards: "A decision whose stated
// rationale names a thing that no longer exists is not a decision any more — it is
// a fossil, and the next engineer will read it as a requirement." A prose
// cancellation cannot fail a build. This can.
//
// WHY THIS IS NOT govulncheck'S JOB. security.yml runs govulncheck, which is
// reachability-aware and fails on a vulnerable symbol actually being called. The
// two answer different questions and neither subsumes the other:
//
//   - govulncheck asks "is a known CVE reachable today?" — a moving target keyed to
//     what has been published. It goes green the day a CVE is patched, while the
//     architectural decision still stands.
//   - this asks "did the cancelled plane come back?" — which is true the moment the
//     import lands, before any vulnerable symbol is called, and stays true whether
//     or not x/crypto/ssh currently has an advisory.
//
// A file behind `//go:build binance` is invisible to `go list` (and so to
// loadPackages) unless the tag is set; parsing sidesteps build tags entirely, so
// this sees every file including tagged and _test.go ones. A test that dials SSH is
// still the plane arriving.
//
// SIDE EFFECT WORTH KNOWING: 13 Dependabot alerts against golang.org/x/crypto/ssh
// are held open-but-unreachable on the strength of this import not existing. That
// dismissal is currently a claim someone re-verifies by hand; below, it is the
// build's answer.
//
// SCOPE, HONESTLY. This is keyed to the NAME "ssh". It catches the family that says
// so (golang.org/x/crypto/ssh, gliderlabs/ssh, and anything under them) and would
// miss an SSH client that never spells it — github.com/pkg/sftp is SSH-based and
// says "sftp"; a library named goph says neither. It is a tripwire on the known
// family, not a proof of absence. Enumerating every SSH library by name would be a
// blacklist, and a blacklist only blocks the spellings you thought of — this
// codebase has shipped that bug twice (SEC-M5's suffix grouping, ONBOARD-M1's echo
// blacklist, defeated by printf). Deny-by-default plus a reasoned allowlist is the
// shape that does not decay.

// sshImportAllowed is now PATH-SCOPED (S2a): golang.org/x/crypto/ssh is permitted
// ONLY inside cmd/kanz-provisioner/, the one-shot Job that bootstraps a node's k3s
// join over SSH. SSH returns to the estate at exactly this one point (KANZ_BRAIN.md,
// "estate is Kubernetes-managed", refined 2026-07-24: the node-join handshake). The
// same import ANYWHERE ELSE is still the cancelled operator plane arriving and
// offends. This is a lead decision (the hybrid-k3s substrate) with a BRAIN amendment,
// per this file's own rule.
//
// Keyed to a (importPath, dirPrefix) pair: the value is the repo-relative directory
// prefix the import is allowed under.
//
// knownhosts is a SUBPACKAGE of the entry above, allowed under the same prefix and for
// the opposite of the reason this test exists: it is what makes the one permitted SSH
// session verify WHO it is talking to. That session used to run with
// ssh.InsecureIgnoreHostKey, so it piped K3S_TOKEN — cluster admission — to whatever
// answered the dial (see cmd/kanz-provisioner/hostkey.go for the full account). This
// entry does not widen the plane by a single call site; it narrows the one that exists.
//
// It needs its own entry only because this map is keyed on the EXACT import path, which
// is deliberate — prefix-matching "golang.org/x/crypto/ssh" would silently admit every
// future subpackage under it, and admitting them one at a time with a reason is the
// deny-by-default shape this file argues for. Removing the insecure callback without
// this line leaves the build red, so the two changes belong together.
var sshImportAllowed = map[string]string{
	"golang.org/x/crypto/ssh":            "cmd/kanz-provisioner/",
	"golang.org/x/crypto/ssh/knownhosts": "cmd/kanz-provisioner/",
}

func TestNoSSHPlane(t *testing.T) {
	root := moduleRoot(t)

	type offence struct {
		importPath string
		file       string
	}
	var offences []offence
	matched := map[string]bool{} // allowlist entries actually seen, for the anti-rot check

	filesParsed := 0
	importsSeen := 0

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// testdata holds fixtures, not the estate's code; a golden file naming
			// ssh is not a dependency. Nothing else is skipped — there is no
			// vendor/ in this module, and _test.go files are deliberately IN scope.
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		// ImportsOnly: this needs the import block, not function bodies, and it
		// must not care whether the file's build tags are satisfied.
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		filesParsed++

		for _, spec := range f.Imports {
			ip, uerr := strconv.Unquote(spec.Path.Value)
			if uerr != nil {
				t.Fatalf("unquote import %s in %s: %v", spec.Path.Value, path, uerr)
			}
			importsSeen++

			if !strings.Contains(strings.ToLower(ip), "ssh") {
				continue
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				rel = path
			}
			relSlash := filepath.ToSlash(rel)
			if prefix, ok := sshImportAllowed[ip]; ok && strings.HasPrefix(relSlash, prefix) {
				matched[ip] = true
				continue
			}
			offences = append(offences, offence{importPath: ip, file: relSlash})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	// NON-VACUOUS. A walk that resolves the wrong root, or a parser flag that
	// silently drops import blocks, produces zero findings — which reads exactly
	// like success. This codebase has shipped a guard that tested nothing
	// (ONBOARD-M2); a gate that cannot fail is not a gate.
	if filesParsed == 0 {
		t.Fatalf("parsed 0 Go files under %s — the walk is broken, not the tree clean", root)
	}
	if importsSeen == 0 {
		t.Fatalf("saw 0 imports across %d Go files — imports are not being read, so this test proves nothing", filesParsed)
	}

	if len(offences) > 0 {
		sort.Slice(offences, func(i, j int) bool {
			if offences[i].importPath != offences[j].importPath {
				return offences[i].importPath < offences[j].importPath
			}
			return offences[i].file < offences[j].file
		})
		var b strings.Builder
		b.WriteString("the CANCELLED SSH plane is back in the build:\n\n")
		for _, o := range offences {
			b.WriteString("  " + o.file + " imports " + o.importPath + "\n")
		}
		b.WriteString("\nSSH-pushed hosts were cancelled on 2026-07-16 (KANZ_BRAIN.md): the estate is\n" +
			"Kubernetes-managed, so deployment is a GitOps commit authenticated by the\n" +
			"git/CI/cosign chain — there is no host to push a binary to and no SSH handshake\n" +
			"to carry an identity in. The sovereign blueprint still describes the SSH operator\n" +
			"plane; the blueprint is superseded on this point.\n\n" +
			"If you are implementing node deployment or revocation, read KANZ_BRAIN.md's\n" +
			"'THE ESTATE IS KUBERNETES-MANAGED' entry first — the k8s equivalents already\n" +
			"exist (GitOps, Vault-CSI secret purge, scale-to-zero, kanz-halt's broadcast).\n\n" +
			"If this import is genuinely not the operator plane returning, add it to\n" +
			"sshImportAllowed with the reason why. Reviving the SSH plane is a lead decision\n" +
			"and amends KANZ_BRAIN.md — it is not a test edit.")
		t.Fatal(b.String())
	}

	// ANTI-ROT. An allowlist entry whose import no longer exists is a dead
	// exemption: it grants permission nobody asked for, and the next reader takes
	// it as evidence the estate does SSH somewhere. Fail so it gets deleted.
	for ip, reason := range sshImportAllowed {
		if !matched[ip] {
			t.Errorf("sshImportAllowed has a DEAD entry: %q (%q) is no longer imported anywhere — delete it", ip, reason)
		}
	}
}
