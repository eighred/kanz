package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoDockerfileDownloadsModulesWithoutRetry(t *testing.T) {
	root := moduleRoot(t)

	const (
		sharedScript = "go-mod-download.sh"
		// Floor guards against a walk that stops matching and reports nothing.
		// Measured: 29 Dockerfiles, 29 downloading.
		floor = 20
	)

	dockerfiles, downloading := 0, 0

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "Dockerfile" {
			return nil
		}
		dockerfiles++

		b, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("read %s: %v", path, rerr)
		}
		// CRLF on a Windows checkout (core.autocrlf=true), same normalisation as
		// baseimage_test.go — without it every line test below silently sees
		// nothing and this guard passes having checked no file at all.
		content := strings.ReplaceAll(string(b), "\r\n", "\n")

		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)

		usesScript := false
		for _, line := range strings.Split(content, "\n") {
			trimmed := strings.TrimSpace(line)
			// COMMENTS ARE NOT CODE. Skipped before anything is matched, because
			// the comment this change added to every one of these files contains
			// the very string the rule forbids.
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if strings.Contains(trimmed, sharedScript) {
				usesScript = true
				continue
			}
			if !strings.HasPrefix(trimmed, "RUN ") {
				continue
			}
			if strings.Contains(trimmed, "go mod download") {
				t.Errorf("%s: downloads modules without a retry:\n    %s\n\n"+
					"The module proxy and checksum database drop HTTP/2 streams mid-transfer, so a bare "+
					"call turns a transient network fault into a red build on a commit that is fine — "+
					"observed three times in one session, on three different modules. Use the shared "+
					"script instead, which retries a transport failure and still fails the build when "+
					"the proxy is genuinely down:\n\n"+
					"    COPY kanz/build/%s /usr/local/bin/%s\n"+
					"    RUN sh /usr/local/bin/%s\n\n"+
					"Do NOT add a retry loop to this Dockerfile — 29 of them share one script "+
					"deliberately, and a copied helper is how a fix stops spreading (#776).",
					rel, trimmed, sharedScript, sharedScript, sharedScript)
			}
		}
		if usesScript {
			downloading++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	// Non-vacuity, both halves: the walk found files, and those files still do
	// the thing this guard is about. Either number collapsing means the guard has
	// stopped checking rather than that the estate got safer.
	if dockerfiles < floor {
		t.Fatalf("found only %d Dockerfile(s) under %s, want at least %d — the walk is not reaching "+
			"the image fleet, so this guard proves nothing (#776)", dockerfiles, root, floor)
	}
	if downloading < floor {
		t.Fatalf("only %d of %d Dockerfile(s) use %s. Either the shared script was renamed and this "+
			"guard now credits nothing, or the fleet has gone back to downloading modules some other "+
			"way — both mean the retry is no longer in the path it was written for (#776)",
			downloading, dockerfiles, sharedScript)
	}
}

// THE SHARED SCRIPT STILL FAILS A BUILD WHEN THE PROXY IS GENUINELY DOWN.
//
// The dangerous version of this change is a retry loop that ends in `|| true`, or
// whose last iteration is swallowed: it exits 0 on a proxy that is down and hands
// the build a half-populated module cache. That trades a loud failure for a
// confusing one, and it would look exactly like success.
//
// So the last attempt must be UNGUARDED — outside the loop, its status the
// script's status — and the script must be bounded in wall-clock, because a
// raised retry count with no deadline turns a red build into a stuck one.
func TestTheSharedDownloadScriptStillFailsARealOutage(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "build", "go-mod-download.sh")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v\n\n29 Dockerfiles copy this script in; if it moved, move this guard with "+
			"it rather than deleting it (#776)", path, err)
	}
	content := strings.ReplaceAll(string(b), "\r\n", "\n")

	var code []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		code = append(code, trimmed)
	}
	if len(code) == 0 {
		t.Fatal("the shared download script has no executable lines — it is all comment, so every " +
			"Dockerfile that copies it downloads nothing and the build proceeds on an empty cache (#776)")
	}

	// The final statement must be the unguarded download.
	last := code[len(code)-1]
	if last != "go mod download" {
		t.Errorf("the script's last statement is %q, not a bare `go mod download`.\n\n"+
			"The final attempt has to run OUTSIDE the retry loop with its status unmodified. A script "+
			"that ends in a conditional, a `|| true`, or an `exit 0` reports success on a proxy that "+
			"is genuinely down and hands the build a half-populated module cache — which is a worse "+
			"failure than the red build this change was made to prevent (#776).", last)
	}

	// AGAINST THE CODE, NOT THE FILE. The script's own comment names each of
	// these as something it must not do, so matching the raw text finds the
	// rationale in a compliant script and fails it. Caught here by this guard
	// firing on its own subject the first time it ran.
	executable := strings.Join(code, "\n")
	for _, forbidden := range []string{"GOFLAGS=-insecure", "GONOSUMDB", "GONOSUMCHECK", "GOPRIVATE=*"} {
		if strings.Contains(executable, forbidden) {
			t.Errorf("the script sets %s.\n\nThe fix for a flaky proxy is a retry, never relaxed "+
				"verification. The checksum database is consulted on whatever bytes finally arrive, so "+
				"retrying changes how many attempts are made and never whether the result is "+
				"verified — disabling that check would trade a flake for a supply-chain hole (#776).",
				forbidden)
		}
	}

	// BOUNDED IN WALL-CLOCK, NOT ONLY IN ATTEMPTS — and this asks for the knob
	// AND the comparison, not for the word.
	//
	// The first version of this arm searched the code for "DEADLINE" and a
	// mutation walked straight through it: renaming the ASSIGNMENT left the USE
	// site still mentioning the name, so the substring was found in a script whose
	// deadline had been deleted. Presence of a symbol is not evidence that
	// anything is bounded by it — the same shape as #992, found the same way.
	if !strings.Contains(executable, "GO_MOD_DOWNLOAD_DEADLINE") {
		t.Error("the script no longer reads GO_MOD_DOWNLOAD_DEADLINE, so its wall-clock bound is gone " +
			"or is no longer tunable by the caller.\n\nA retry count with no deadline can hang a job " +
			"rather than fail it, and a job that hangs gets waited on while a job that fails gets " +
			"read — that trades a red build for a stuck one (#776).")
	}
	compared := false
	for _, line := range code {
		if strings.Contains(line, "elapsed") && (strings.Contains(line, "-ge") || strings.Contains(line, "-gt")) {
			compared = true
		}
	}
	if !compared {
		t.Error("the script reads a deadline but never compares elapsed time against it, so the value " +
			"is declared and unused — a bound nothing enforces (#776).")
	}
}
