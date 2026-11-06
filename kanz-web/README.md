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
npm ci --no-audit --no-fund
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
