package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A SECRET THE DEPLOYMENT MOUNTS AND THE PROCESS NEVER READS (#530 follow-up).
//
// # What happened
//
// identity-deploy.yaml sets IDENTITY_DATABASE_URL_FILE and nothing else.
// services/identity/internal/config/config.go read os.Getenv("IDENTITY_DATABASE_URL")
// — the PLAINTEXT key, which the manifest never sets — and Load() returns
// "IDENTITY_DATABASE_URL is required" when it is empty.
//
// So the deployed credential authority exited 2 at startup and never came up.
// Its image is distroless with ENTRYPOINT ["/identity"] and no shell, and the
// manifest sets no command, so nothing resolved the file on the process's
// behalf. Everything downstream of identity — every login, every token, the
// gateway's whole authenticated path — was waiting on a pod that could not start.
//
// # Why the sibling guard did not catch it
//
// secret_helper_test.go forbids LOCAL COPIES of secret resolution, and its own
// header says "THE POINT OF THIS GUARD IS THE COPIES, NOT THE BUG". identity had
// no copy. It simply never resolved <K>_FILE at all, which is the one shape a
// copy-hunting guard cannot see: there was nothing to find.
//
// Twenty-four mounted secrets across the estate went through secret.Read.
// Exactly one did not, and nothing connected them — which is the failure mode
// pkg/secret's own header describes from the other direction: "venue-binance and
// venue-okx were correct for weeks while every other root stayed wrong, because
// nothing connected them."
//
// # The rule, and the distinction that makes it precise
//
// If a manifest sets <K>_FILE and the Go side reads the BASE key <K>, that read
// must go through secret.Read — which prefers the file and, crucially, ERRORS on
// an unreadable mount rather than falling through to "".
//
// A manifest key that the Go side reads AS ITSELF is a different thing and is
// not flagged: IDENTITY_SIGNING_KEY_FILE is a PEM PATH parameter (config field
// SigningKeyFile), not a secret-helper pair. There is no IDENTITY_SIGNING_KEY,
// so there is nothing for secret.Read to prefer between.
func TestEveryMountedSecretIsReadThroughTheHelper(t *testing.T) {
	repoRoot := filepath.Dir(moduleRoot(t))
	deployDir := filepath.Join(repoRoot, "kanz", "infra", "deploy")

	mounted := mountedFileKeys(t, deployDir)
	// NON-VACUITY. A manifest scan that finds nothing passes this guard no matter
	// how many services never read their secret.
	if len(mounted) < 10 {
		t.Fatalf("found only %d *_FILE env keys across %s — the manifest scan is broken, not the "+
			"estate; there are more than twenty", len(mounted), deployDir)
	}

	root := moduleRoot(t)
	src := goSourceUnder(t, root)
	if len(src) == 0 {
		t.Fatal("read no Go source — the scanner is broken, not the estate")
	}

	var offenders []string
	for _, fileKey := range mounted {
		base := strings.TrimSuffix(fileKey, "_FILE")

		// The Go side may legitimately read the _FILE key ITSELF as a path
		// parameter. That is not a secret-helper pair and there is nothing to
		// resolve — skip it before looking for the base key.
		if strings.Contains(src, `os.Getenv("`+fileKey+`")`) {
			continue
		}
		// Does anything read the plaintext base key directly?
		if !strings.Contains(src, `os.Getenv("`+base+`")`) {
			continue
		}
		// It does. That is only correct if secret.Read owns it instead — and if
		// BOTH appear, secret.Read is the one that runs in the composition root
		// while the os.Getenv is something else (a test rig, a different arm), so
		// the pair is not an offence on its own.
		if strings.Contains(src, `secret.Read("`+base+`")`) {
			continue
		}
		offenders = append(offenders, fileKey+" is mounted, but the process reads os.Getenv(\""+base+"\")")
	}
	sort.Strings(offenders)

	if len(offenders) > 0 {
		t.Errorf("%d mounted secret(s) the process cannot actually read:\n  %s\n\n"+
			"The deployment sets <K>_FILE and the code reads the plaintext <K>, which the manifest "+
			"never sets. Depending on how the caller treats an empty value the pod either exits at "+
			"startup — identity did, and the credential authority never came up — or silently "+
			"selects an in-memory store and loses everything on restart.\n\n"+
			"Read it with secret.Read(\"<K>\") (pkg/secret). It prefers the file, and it ERRORS on an "+
			"unreadable mount instead of falling through to the env var and then to \"\", which is "+
			"the fall-through that makes a failed CSI mount indistinguishable from a secret nobody "+
			"configured.",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// mountedFileKeys returns every <K>_FILE env key any deploy manifest sets.
var mountedFileKeyRe = regexp.MustCompile(`name:\s*([A-Z][A-Z0-9_]*_FILE)\s*,`)

func mountedFileKeys(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v — the guard cannot check what it cannot read", dir, err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		// COMMENTS STRIPPED. These manifests discuss their own secret handling at
		// length, and a guard that read prose would take an example for a mount.
		for _, line := range strings.Split(string(b), "\n") {
			if i := strings.Index(line, "#"); i >= 0 {
				line = line[:i]
			}
			for _, m := range mountedFileKeyRe.FindAllStringSubmatch(line, -1) {
				seen[m[1]] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// goSourceUnder concatenates every non-test Go file under root, comments
// stripped by the crude but sufficient means of ignoring line comments — the
// tokens matched here are string literals, which never appear in a `//` line
// that matters.
func goSourceUnder(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	for _, f := range goFilesUnder(t, root) {
		if strings.HasSuffix(f.rel, "_test.go") {
			continue
		}
		for _, line := range strings.Split(f.body, "\n") {
			if i := strings.Index(line, "//"); i >= 0 {
				line = line[:i]
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	return b.String()
}
