package server

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// staticHandler serves the compiled SPA from the same origin as /api and /auth
// (#371).
//
// ONE ORIGIN IS THE WHOLE POINT. The session cookie is httpOnly and
// SameSite=Lax; serving the SPA from a different host would mean either
// loosening it to SameSite=None (which is what makes a cookie usable in a
// cross-site context, and therefore what CSRF defences exist to prevent) or
// bolting on CORS with credentials and a cookie domain that has to be kept in
// step with the edge. Same origin removes the question rather than answering it.
//
// THE FALLBACK IS THE PART THAT GOES WRONG. A SPA needs unknown paths to return
// index.html so the client router can handle /venues on a hard refresh. Applied
// naively, that also swallows:
//
//   - a mistyped API path, which returns 200 + HTML, so the client's JSON parse
//     fails with "unexpected token <" and the real 404 is invisible;
//   - a missing asset, which returns HTML with a text/html type, so the browser
//     refuses the module and reports a MIME error rather than a missing file.
//
// Both read as a broken app rather than a missing route. So the fallback applies
// ONLY to extension-less paths that are not reserved server routes.
// THE ROOT IS A REAL BOUNDARY, NOT A STRING COMPARISON (CodeQL go/path-injection,
// alert #4).
//
// This used to join the request path onto the directory and check the result
// still had the directory as a prefix. That is the conventional guard and it is
// weaker than it reads:
//
//   - os.Stat FOLLOWS SYMLINKS, and the prefix check is applied to the path
//     BEFORE resolution. A symlink inside the build output pointing anywhere on
//     the filesystem passes the check and is then served — and the build output
//     is produced by a bundler, not by hand, so "there are no symlinks in dist"
//     is an assumption about a tool's future behaviour.
//   - it is a lexical test standing in for a filesystem property, which is
//     exactly the substitution the scanner flags, and it was right to.
//
// os.Root enforces containment in the kernel-facing API instead: every method
// refuses a name whose components leave the root, and it follows symlinks ONLY
// while they stay inside it. The check is no longer something this file can get
// subtly wrong.
type staticHandler struct {
	root *os.Root
	fsrv http.Handler
}

// reservedPrefixes are the server's own routes. A request under one of these
// must never be answered with index.html — it is either handled by the mux above
// or it is a genuine 404, and turning it into HTML hides which.
var reservedPrefixes = []string{"/api/", "/auth/", "/healthz", "/readyz", "/metrics"}

// newStaticHandler serves dir, or returns nil when dir is empty — the BFF then
// runs API-only, which is the local-development shape where Vite serves the SPA
// itself on another port.
func newStaticHandler(dir string) (*staticHandler, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("web-bff: WEB_BFF_STATIC_DIR=%s cannot be opened (%w) — "+
			"build the SPA first (tools/web-up.sh), or unset it to run API-only", abs, err)
	}
	// Fail at STARTUP if the build is not there. A missing static root that is
	// discovered per-request means the pod is Ready and every page is a 404 — an
	// outage that looks like a routing bug.
	if _, err := root.Stat("index.html"); err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("web-bff: WEB_BFF_STATIC_DIR=%s has no index.html (%w) — "+
			"build the SPA first (tools/web-up.sh), or unset it to run API-only", abs, err)
	}
	// FileServerFS over the root's own fs.FS, so the request path is resolved by
	// the same bounded API the existence check uses. Two different resolvers for
	// one directory is how a check and the thing it guards come to disagree.
	return &staticHandler{root: root, fsrv: http.FileServerFS(root.FS())}, nil
}

// Close releases the directory handle os.OpenRoot holds.
//
// IT IS NOT BOOKKEEPING. A Root keeps an open descriptor on the static
// directory for the life of the handler — that is how it can enforce
// containment against a directory that is later moved or replaced. A process
// holds exactly one for its lifetime, so leaking it costs nothing in
// production; anything that builds Servers repeatedly (tests, and any future
// config reload) leaks one per Server, and on Windows the open handle also
// stops the directory being removed at all.
//
// Nil-safe, because newStaticHandler returns a nil handler in the API-only
// shape and every caller would otherwise need the same check.
func (h *staticHandler) Close() error {
	if h == nil || h.root == nil {
		return nil
	}
	return h.root.Close()
}

func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	upath := path.Clean("/" + r.URL.Path)

	for _, p := range reservedPrefixes {
		if upath == strings.TrimSuffix(p, "/") || strings.HasPrefix(upath, p) {
			http.NotFound(w, r)
			return
		}
	}

	// A real file wins, whatever its shape.
	if h.exists(upath) {
		h.fsrv.ServeHTTP(w, r)
		return
	}
	// An unresolved path WITH an extension is a missing asset, not a route.
	if path.Ext(upath) != "" {
		http.NotFound(w, r)
		return
	}
	// A CONSTANT NAME, resolved inside the root. The shell is the one file this
	// handler serves without being asked for it by name, so nothing
	// user-controlled should reach the filesystem on this line.
	http.ServeFileFS(w, r, h.root.FS(), "index.html")
}

// exists reports whether upath names a regular file inside the static root.
//
// It decides the FALLBACK as well as the hit: a traversal that slipped through
// here would answer with index.html for a path outside the root, which looks
// like the app working rather than like an attack being refused.
//
// Root.Stat REFUSES rather than resolves anything that leaves the root —
// including through a symlink — so an escape arrives as an error and returns
// false, which is the same answer as "no such file". There is deliberately no
// branch distinguishing them: a caller who can tell a refused traversal from a
// missing asset learns the layout of the filesystem outside the root.
func (h *staticHandler) exists(upath string) bool {
	// Root paths are relative to the root and slash-separated; the leading "/"
	// path.Clean guaranteed above would make this an absolute name, which Root
	// rejects outright.
	name := strings.TrimPrefix(upath, "/")
	if name == "" {
		return false
	}
	info, err := h.root.Stat(name)
	if err != nil {
		return false
	}
	return fs.FileMode(info.Mode()).IsRegular()
}
