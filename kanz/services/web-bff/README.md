# Browser boundary

The BFF serves the SPA and its cookie-authenticated API on one origin. Before
routing any unsafe request, it checks Go's `http.CrossOriginProtection` and,
when present, requires `Origin` to equal the request's scheme and Host exactly.
Sibling origins on the same site are refused too. Ambiguous, opaque and
malformed origins are refused before reading the body or changing a session.

The production scheme is HTTPS when secure cookies are enabled, including the
Cloudflare tunnel's HTTP loopback hop. The tunnel must preserve the public Host;
`Forwarded` and `X-Forwarded-*` cannot override the origin decision. Local HTTP
development uses `WEB_BFF_INSECURE_COOKIES=1`; Vite preserves the browser Host.
Headerless non-browser clients still require normal authentication. Safe GET,
HEAD and OPTIONS requests retain their routing behavior; OIDC callbacks retain
their state and PKCE checks.

All responses receive the same CSP, framing, MIME-sniffing, referrer, permissions
and cross-origin isolation headers. The CSP permits same-origin scripts, styles,
images, fonts and connections, without inline script or eval exceptions. HSTS
is enabled only with secure cookies and applies only to the current host.
Authentication and API responses are `no-store`; gateway responses cannot replace
the BFF's browser policy or grant CORS access to its sessions.

Regression tests cover rejection before body/session/upstream access, concurrent
requests over real TLS, local HTTP, OIDC redirects, static assets, errors and
hostile upstream headers. When changing CSP, also build `kanz-web` and exercise
login, authenticated navigation and logout in a real browser against the compiled
BFF and identity service, checking for `securitypolicyviolation` events.

The session store remains process-local. A BFF rollout invalidates existing
browser sessions and requires users to sign in again; keep the singleton pin.
