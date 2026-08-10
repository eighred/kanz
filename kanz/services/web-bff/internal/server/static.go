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
type staticHandler struct {
	dir  string
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
	// Fail at STARTUP if the directory is not there. A missing static root that
	// is discovered per-request means the pod is Ready and every page is a 404 —
	// an outage that looks like a routing bug.
	if _, err := os.Stat(filepath.Join(abs, "index.html")); err != nil {
		return nil, fmt.Errorf("web-bff: WEB_BFF_STATIC_DIR=%s has no index.html (%w) — "+
			"build the SPA first (tools/web-up.sh), or unset it to run API-only", abs, err)
	}
	return &staticHandler{dir: abs, fsrv: http.FileServer(http.Dir(abs))}, nil
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
	http.ServeFile(w, r, filepath.Join(h.dir, "index.html"))
}

// exists reports whether upath names a regular file inside the static root.
//
// The join is checked rather than trusted: http.Dir already refuses escaping
// paths, but this method also decides the FALLBACK, and a traversal that slipped
// through here would answer with index.html for a path outside the root — which
// looks like the app working.
func (h *staticHandler) exists(upath string) bool {
	full := filepath.Join(h.dir, filepath.FromSlash(upath))
	if !strings.HasPrefix(full, h.dir+string(os.PathSeparator)) && full != h.dir {
		return false
	}
	info, err := os.Stat(full)
	if err != nil {
		return false
	}
	return fs.FileMode(info.Mode()).IsRegular()
}
