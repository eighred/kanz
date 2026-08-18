package arch

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// secretPkg is the ONE place a secret may be resolved from. Everything else
// must call into it.
const secretPkg = "pkg/secret"

// ONE SECRET RESOLVER, AND NO LOCAL COPIES.
//
// Every service composition root plus cmd/kanz-migrate carried its own
// `func secret(k string) string`, and fifteen of the seventeen discarded
// os.ReadFile's error:
//
//	if p := os.Getenv(k + "_FILE"); p != "" {
//	    if b, err := os.ReadFile(p); err == nil {
//	        return strings.TrimSpace(string(b))
//	    }
//	}
//	return os.Getenv(k)          // ← reached when the mount is UNREADABLE
//
// Setting <k>_FILE is the deployment stating that a durable secret mount was
// intended. If that file is missing or unreadable, that is a DEPLOYMENT FAULT —
// but the fall-through answers it with the plaintext env and then with "", so a
// failed Vault CSI mount is indistinguishable from a secret nobody configured.
// Several services then treat "" as a legal value selecting an in-memory store,
// so the pod comes up, reports healthy, and loses everything on restart.
//
// THE POINT OF THIS GUARD IS THE COPIES, NOT THE BUG. venue-binance and
// venue-okx had the CORRECT implementation for weeks while fifteen others stayed
// wrong, because nothing connected them. A fix that lives in a copy does not
// spread; it just makes the next reader think the problem is solved. Deleting
// the copies is what makes the fix estate-wide, and this test is what stops
// them coming back.
func TestOnlyOnePlaceResolvesSecrets(t *testing.T) {
	root := moduleRoot(t)

	files := goFilesUnder(t, root)
	// NON-VACUITY. A walk that finds no Go files passes both checks below no
	// matter how many local resolvers exist.
	if len(files) == 0 {
		t.Fatalf("found zero Go files under %s — the scanner is broken, not the estate", root)
	}

	// 1. NO LOCAL func secret(. The obvious reintroduction: someone adds a new
	//    service by copying an existing composition root.
	localSecretRe := regexp.MustCompile(`(?m)^func secret\(`)
	// 2. NO OPEN-CODED <K>_FILE RESOLUTION. The subtler reintroduction: the same
	//    logic under a different name, which check 1 alone would wave through.
	//
	//    RESOLUTION, NOT MENTION. The first version of this flagged any file
	//    containing "_FILE" and immediately produced two false positives — a
	//    config test doing t.Setenv(key+"_FILE", path) to drive Load() through
	//    the environment, which is exactly how that behaviour SHOULD be tested,
	//    and this file itself. A guard that fires on correct code gets deleted,
	//    so it now requires both halves of the pattern: building the key AND
	//    reading a file. Setting the env var is legitimate anywhere; reading the
	//    path it names is the part that belongs in one place.
	fileEnvRe := regexp.MustCompile(`"_FILE"`)
	readFileRe := regexp.MustCompile(`os\.ReadFile`)

	// These files necessarily contain both halves of the pattern this hunts — the
	// needles are right there in the regexes, and the walks read files. A
	// scanner cannot scan itself for the thing it is looking for, so they are
	// skipped by name rather than by cleverness (splitting the literals to dodge
	// the match would work and would be unreadable, which is worse).
	//
	// THE SECOND ENTRY IS THE COMPLEMENT OF THIS GUARD, NOT AN EVASION OF IT.
	// This one asks "does anybody resolve a mount by hand?"; that one asks "does
	// anybody mount a secret and then never read it?" — the shape identity shipped,
	// which had no local copy for this guard to find. Neither resolves a secret;
	// both read manifests and Go source looking for one.
	scanners := map[string]bool{
		"test/arch/secret_helper_test.go":          true,
		"test/arch/mounted_secret_is_read_test.go": true,
	}

	var localHelpers, openCoded []string
	for _, f := range files {
		if strings.HasPrefix(f.rel, secretPkg+"/") || f.rel == secretPkg || scanners[f.rel] {
			continue // the one legitimate implementation, and the scanners
		}
		if localSecretRe.MatchString(f.body) {
			localHelpers = append(localHelpers, f.rel)
		}
		if fileEnvRe.MatchString(f.body) && readFileRe.MatchString(f.body) {
			openCoded = append(openCoded, f.rel)
		}
	}

	sort.Strings(localHelpers)
	for _, f := range localHelpers {
		t.Errorf("%s declares its own func secret()\n\n"+
			"There is one resolver, %s.Read, and it returns an error when a declared "+
			"<K>_FILE mount is unreadable. A local copy is how fifteen roots kept the "+
			"error-discarding version while two had the fix — a fix in a copy does not "+
			"spread, it just stops the next reader looking. Call secret.Read instead.",
			f, secretPkg)
	}

	sort.Strings(openCoded)
	for _, f := range openCoded {
		t.Errorf("%s builds a \"<K>_FILE\" env key AND reads a file — it is resolving a "+
			"secret mount by hand\n\n"+
			"That is the same defect as a local secret() under a different name, and "+
			"renaming is exactly how this check gets evaded without anyone intending to. "+
			"Setting a <K>_FILE env var is fine anywhere (tests do it to drive Load); "+
			"READING the path it names is what belongs in one place. If a genuine second "+
			"resolution strategy is needed, it goes in %s beside Read, where its error "+
			"handling is reviewed once.", f, secretPkg)
	}
}

// goFile is one Go source file: where it is, and what is in it.
type goFile struct {
	rel  string // module-relative, forward slashes
	body string
}

func goFilesUnder(t *testing.T, root string) []goFile {
	t.Helper()

	var out []goFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", ".gotmp":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		out = append(out, goFile{rel: filepath.ToSlash(rel), body: string(b)})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}
