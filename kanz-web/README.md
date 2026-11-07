# Operator browser client

The Broker account view binds its four-read snapshot to the requested account
and current session identity. Route/session changes clear the old snapshot,
abort outstanding requests and reject obsolete completions. An aborted read
cannot invoke the global unauthorized callback against a replacement session.
This is presentation isolation; the gateway/BFF remain authorization owners.

Order and execution tables render a moving window of rows with fixed-height,
non-wrapping cells and horizontal scrolling. Decimal strings are never converted
to JavaScript numbers. Optional values remain missing, and retained-history and
unknown-freshness warnings remain visible. The row count describes only the
served response. There is no new sorting, numerical algorithm or worker workload.

Each scroll segment contains at most 10,000 rows, with previous/next controls
and a global served-row range. This avoids browser layout-height clipping while
preserving access to every served row. Replacing the snapshot resets the page.

This bounds rendered DOM only: the API still transfers and parses the full
retained response. It does not establish complete history, atomicity across the
four reads, real-time streaming, production deployment or Web/TUI parity.
Server-side cardinality/pagination and worker adoption remain separate work in
[#1413](https://github.com/eighred/kanz/issues/1413).

Use the Node major in `.nvmrc` and the lockfile:

```sh
rustup toolchain install 1.90.0 --profile minimal --target wasm32-unknown-unknown
npm ci --no-audit --no-fund
npm run prebuild
npm run typecheck
npm test
npm run build
npx --no-install playwright install chromium
node e2e/broker-history.cjs
```

The mandatory Chromium qualification serves the actual built Vue bundle on a
loopback listener with synthetic read responses. It checks 10,000-row scrolling,
DOM cardinality, fixed row layout, sticky headers, resize, exact decimal strings,
missing-versus-zero fees, history warnings and obsolete-route refusal. It does
not claim BFF authentication proof; existing real BFF browser tests retain that
responsibility. Happy-DOM tests cover session replacement, logout, abort/401
races, mismatch refusal and stale success/error/finally behavior.

The shared `prebuild`/`predev` recipe compiles the dependency-free Rust rendering
kernel with pinned Rust 1.90.0. It verifies the source inputs, closed ABI and
2 MiB memory ceiling, then publishes a content-addressed WASM asset and one
atomic generated import. Missing prerequisites or inconsistent assets stop the
build; generated files are not committed. Direct typechecking or tests on a
fresh checkout require `npm run prebuild` first.

After Vue mounts, a bounded same-origin fetch verifies the asset size and SHA-256
before instantiation. Authentication and routing do not wait for this load.
The BFF CSP permits WASM compilation through `wasm-unsafe-eval`; JavaScript eval,
inline scripts and blob sources remain forbidden. Failed or unavailable WASM
keeps the existing exact TypeScript renderer in use. Decimal coefficients over
the kernel's 128-character bound retain that legacy path; missing values and
caller defaults are unchanged. This is presentation only, not financial authority.

`TestBrowserRendererProductionBundle` exercises the production Vite bundle through
the real BFF session/proxy/static handlers with synthetic data and Chromium. Its
five cases cover actual WASM export calls, missing/tampered/held assets and the old
CSP refusal, including exact legacy values and JavaScript eval/inline refusal.
The separately resealed wrong-ABI case is an actual-module unit test, not a claim
that a browser asset passed the hash check. Linux CI runs the pinned Node version;
local Node-version diagnostics are not substitutes for that qualification.
