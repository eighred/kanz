package server

// SERVING THE SPA FROM THE SAME ORIGIN AS THE API (#371).
//
// The fallback is what makes a hard refresh on /venues work, and it is also the
// thing that quietly breaks everything else if it is applied to any unresolved
// path. Both failure modes below report as a broken app rather than a missing
// route, which is why they are pinned.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/services/web-bff/internal/clientip"
	"github.com/eighred/kanz/services/web-bff/internal/identityclient"
	"github.com/eighred/kanz/services/web-bff/internal/session"
)

func staticBFF(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html><title>kanz</title>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("export const x = 1"), 0o600); err != nil {
		t.Fatal(err)
	}
	ip, err := clientip.NewResolver("", nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(&Readiness{}, Options{
		Identity:   identityclient.New("http://identity.invalid", "", time.Second),
		ClientIP:   ip,
		StaticDir:  dir,
		Sessions:   session.NewManager(time.Hour),
		GatewayURL: "http://gw.invalid",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The Server holds the static directory open (os.Root). Closing it is what
	// production does on shutdown; here it also lets t.TempDir remove the
	// directory, which Windows refuses while a handle is open — so a leaked
	// handle fails the test rather than passing quietly, which is the right way
	// round.
	t.Cleanup(func() { _ = srv.Close() })
	return srv, dir
}

func get(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestARealAssetIsServedAndTheAppShellIsNot(t *testing.T) {
	srv, _ := staticBFF(t)

	rec := get(t, srv, "/assets/app.js")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "export const x") {
		t.Fatalf("asset: status %d body %q", rec.Code, rec.Body.String())
	}
	if rec := get(t, srv, "/"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>kanz") {
		t.Fatalf("root: status %d", rec.Code)
	}
}

// A CLIENT ROUTE SURVIVES A HARD REFRESH. This is the reason a fallback exists.
func TestAnUnknownExtensionlessPathServesTheAppShell(t *testing.T) {
	srv, _ := staticBFF(t)
	for _, p := range []string{"/venues", "/nodes/abc", "/portfolio/pf-1/exposure"} {
		rec := get(t, srv, p)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>kanz") {
			t.Errorf("%s: status %d, want the app shell — a hard refresh on a client route "+
				"must not 404", p, rec.Code)
		}
	}
}

// A MISSING ASSET IS A 404, NOT HTML.
//
// Falling back here returns index.html with a text/html type, so the browser
// refuses the module and reports a MIME error — which sends the reader looking
// at content types instead of at the file that is not there.
func TestAMissingAssetIsNotAnsweredWithTheAppShell(t *testing.T) {
	srv, _ := staticBFF(t)
	rec := get(t, srv, "/assets/missing.js")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %q).\n\n"+
			"Answering a missing asset with index.html makes the browser report a MIME error "+
			"rather than a missing file.", rec.Code, rec.Body.String())
	}
}

// AN UNKNOWN API PATH STAYS A 404, NOT HTML.
//
// The client parses /api responses as JSON. index.html at 200 fails with
// "unexpected token <" and the real 404 never surfaces.
func TestAnUnknownServerPathIsNotAnsweredWithTheAppShell(t *testing.T) {
	srv, _ := staticBFF(t)
	for _, p := range []string{"/api/v1/nope", "/auth/nope", "/metrics"} {
		rec := get(t, srv, p)
		if strings.Contains(rec.Body.String(), "<title>kanz") {
			t.Errorf("%s: answered with the app shell (status %d).\n\n"+
				"The client parses this as JSON, so the failure surfaces as 'unexpected token <' "+
				"and the real status is invisible.", p, rec.Code)
		}
	}
}

// A TRAVERSAL RESOLVES TO NOTHING OUTSIDE THE ROOT.
func TestAPathOutsideTheStaticRootIsRefused(t *testing.T) {
	srv, dir := staticBFF(t)
	secret := filepath.Join(filepath.Dir(dir), "outside.txt")
	if err := os.WriteFile(secret, []byte("not for the browser"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(secret) })

	for _, p := range []string{"/../outside.txt", "/assets/../../outside.txt"} {
		rec := get(t, srv, p)
		if strings.Contains(rec.Body.String(), "not for the browser") {
			t.Fatalf("%s served a file from OUTSIDE the static root", p)
		}
	}
}

// NO BUILD CONFIGURED MEANS NO SHELL — the API-only shape, where a stray path
// 404s rather than pretending an app is deployed.
func TestWithNoStaticDirTheServerIsAPIOnly(t *testing.T) {
	ip, _ := clientip.NewResolver("", nil)
	srv, err := New(&Readiness{}, Options{
		Identity:   identityclient.New("http://identity.invalid", "", time.Second),
		ClientIP:   ip,
		Sessions:   session.NewManager(time.Hour),
		GatewayURL: "http://gw.invalid",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if rec := get(t, srv, "/venues"); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 with no static dir", rec.Code)
	}
}

// A CONFIGURED BUT ABSENT BUILD FAILS AT STARTUP, not per request.
func TestAMissingStaticBuildRefusesToStart(t *testing.T) {
	ip, _ := clientip.NewResolver("", nil)
	_, err := New(&Readiness{}, Options{
		Identity:   identityclient.New("http://identity.invalid", "", time.Second),
		ClientIP:   ip,
		StaticDir:  filepath.Join(t.TempDir(), "never-built"),
		Sessions:   session.NewManager(time.Hour),
		GatewayURL: "http://gw.invalid",
	})
	if err == nil {
		t.Fatal("a missing static build started anyway — the pod would report Ready while " +
			"every page 404s, which reads as a routing bug rather than a missing deploy step")
	}
}

// A SYMLINK OUT OF THE STATIC ROOT IS REFUSED — the hole the previous guard
// left open (CodeQL go/path-injection, alert #4).
//
// The old check joined the request path onto the directory and required the
// result to keep the directory as a prefix. That is a LEXICAL test: it inspects
// the path before the filesystem resolves it, and os.Stat then follows symlinks.
// A link inside the build output passed the prefix check and was served.
//
// It is not hypothetical in the way "attacker writes into dist" would be: the
// build output is produced by a bundler, and "no tool will ever emit a symlink
// there" is an assumption about future behaviour rather than a property anyone
// enforces. os.Root refuses the escape at the API instead of trusting the
// string, and this pins that.
func TestASymlinkOutOfTheStaticRootIsRefused(t *testing.T) {
	srv, dir := staticBFF(t)

	secret := filepath.Join(filepath.Dir(dir), "outside-secret.txt")
	if err := os.WriteFile(secret, []byte("not for the browser"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(secret) })

	link := filepath.Join(dir, "escape.txt")
	if err := os.Symlink(secret, link); err != nil {
		// Windows needs Developer Mode or elevation to create one. Skipping is
		// honest here and the reason is named — CI runs this on Linux, where the
		// assertion below is the one that matters.
		t.Skipf("cannot create a symlink on this platform (%v) — this test is meaningful on CI's Linux runner", err)
	}

	rec := get(t, srv, "/escape.txt")
	if strings.Contains(rec.Body.String(), "not for the browser") {
		t.Fatalf("a symlink inside the static root served a file from OUTSIDE it (status %d).\n\n"+
			"A prefix check on the joined path cannot see this: it runs before the filesystem "+
			"resolves the link. Containment has to be enforced by the API that opens the file.",
			rec.Code)
	}
	// And it must not be answered with the app shell either: that would render a
	// refused traversal as a working app, which is the failure the fallback rule
	// exists to prevent.
	if strings.Contains(rec.Body.String(), "<!doctype html") {
		t.Fatalf("/escape.txt was answered with index.html — an extension-bearing path must 404")
	}
}
