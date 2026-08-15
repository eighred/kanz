package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// AN APPLICATION THAT IS FINISHED, GREEN, AND BLANK IN A BROWSER (#371).
//
// # What happened
//
// #371's five slices were complete — 12 test files, 110 passing frontend tests,
// every screen wired through /api/*. The web-bff image built cleanly. And the
// image contained NO SPA: its Dockerfile compiled the Go binary and copied
// nothing else, because kanz-web/dist is gitignored (it is a build artifact) and
// nothing in the build pipeline produced it into the docker context.
//
// The result would have started, passed every probe, served /api and /auth
// correctly, and returned nothing at /. Every existing check agrees the work is
// done: the Go suite, the Vitest suite, the image build, the deployability
// parity check. None of them asks whether the thing a browser loads is in the
// artifact.
//
// # Why the SPA has to be in THIS image rather than anywhere else
//
// It is a security property, not packaging. The BFF serves the application on
// its OWN origin and proxies /api/* from the same hostname, so the session stays
// an httpOnly, SameSite=Lax cookie. A separate static host is a second origin,
// which forces SameSite=None — the setting that makes a cookie usable cross-site
// and therefore the one CSRF defences exist to prevent — plus
// CORS-with-credentials and a cookie domain kept in step with the edge.
// infra/edge/README.md states this as the reason the tunnel has ONE origin.
//
// So "the SPA is served from somewhere else" is not an alternative arrangement
// this guard should tolerate; it is the arrangement the design refuses.
//
// # What this checks
//
// If a production manifest tells the BFF where the SPA is (WEB_BFF_STATIC_DIR),
// the Dockerfile must actually put it there. The two are edited by different
// people at different times in different languages, which is exactly the pairing
// that drifts silently — the manifest keeps naming a directory and the image
// stops containing one, or never did.
func TestBFFImageServesTheSPAItsManifestPointsAt(t *testing.T) {
	root := moduleRoot(t)

	dockerfile := filepath.Join(root, "services", "web-bff", "Dockerfile")
	df, err := os.ReadFile(dockerfile)
	if err != nil {
		t.Fatalf("read the web-bff Dockerfile: %v", err)
	}
	dfText := string(df)

	// NON-VACUITY: the Dockerfile must be the one we think it is. A renamed or
	// emptied file would otherwise make every assertion below pass on nothing.
	if !strings.Contains(dfText, "/out/web-bff") {
		t.Fatalf("services/web-bff/Dockerfile does not build the web-bff binary — this guard is " +
			"reading the wrong file, or the build has moved")
	}

	// Where the image puts the SPA: the destination of a COPY whose source is the
	// built frontend. Parsed rather than hardcoded so moving it is one edit.
	copySPA := regexp.MustCompile(`(?m)^COPY\s+kanz-web/dist/?\s+(\S+)\s*$`)
	m := copySPA.FindStringSubmatch(dfText)
	if m == nil {
		t.Fatalf("services/web-bff/Dockerfile never copies kanz-web/dist into the image.\n\n" +
			"The image would start, pass every probe, serve /api and /auth correctly and return " +
			"NOTHING at / — an application that is finished, green and blank in a browser. The SPA " +
			"must ship in THIS image because the BFF serves it on its own origin; a separate static " +
			"host is a second origin, which forces the session cookie to SameSite=None (see " +
			"infra/edge/README.md).")
	}
	imageDir := strings.TrimSuffix(m[1], "/")

	// AND THE WORKFLOW MUST PRODUCE dist BEFORE THE BUILD READS IT. kanz-web/dist
	// is gitignored, so a COPY of it fails unless something put it in the context
	// — and it must be BOTH workflows: a release that ships an image without the
	// SPA is the same outage, arriving on the artifact anyone actually deploys.
	repoRoot := filepath.Dir(root)
	for _, wf := range []string{"build.yml", "release.yml"} {
		b, err := os.ReadFile(filepath.Join(repoRoot, ".github", "workflows", wf))
		if err != nil {
			t.Fatalf("read %s: %v", wf, err)
		}
		if !strings.Contains(string(b), "npm run build") {
			t.Errorf(".github/workflows/%s never runs `npm run build`, so kanz-web/dist does not exist "+
				"in the docker context and the web-bff image ships without the SPA (or fails to build "+
				"at the COPY, depending on buildkit's mood about a missing path)", wf)
		}
	}

	// Every production manifest that names a static dir must name THIS one.
	deployDir := filepath.Join(root, "infra", "deploy")
	staticDir := regexp.MustCompile(`WEB_BFF_STATIC_DIR[,:]\s*value:\s*"?([^"\s}]+)"?`)
	var checked int
	err = filepath.Walk(deployDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, mm := range staticDir.FindAllStringSubmatch(string(b), -1) {
			checked++
			if got := strings.TrimSuffix(mm[1], "/"); got != imageDir {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s sets WEB_BFF_STATIC_DIR=%s, but the image puts the SPA at %s.\n\n"+
					"The BFF would serve an empty directory: no error, no failed probe, a blank page. "+
					"The manifest and the Dockerfile are edited by different people at different times "+
					"in different languages, which is why this pairing is checked rather than trusted.",
					filepath.ToSlash(rel), got, imageDir)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk infra/deploy: %v", err)
	}
	// A manifest is not required — web-bff may legitimately not be deployed yet —
	// so `checked == 0` is not a failure. It is logged so a reader can tell "the
	// pairing agrees" from "there is no pairing to disagree".
	if checked == 0 {
		t.Logf("no production manifest sets WEB_BFF_STATIC_DIR yet; the image places the SPA at %s "+
			"and this guard will check the first manifest that names one", imageDir)
	}
}
