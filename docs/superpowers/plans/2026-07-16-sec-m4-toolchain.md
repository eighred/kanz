# SEC-M4 — 18 production images ship 13 reachable stdlib vulnerabilities

## Context

`govulncheck ./...` reports **13 vulnerabilities the code actually reaches**, every one in the Go
standard library, every one fixed by **go1.26.5**. Among them:

- `GO-2026-4870` — unauthenticated TLS 1.3 KeyUpdate DoS in `crypto/tls`, reached from
  `bus.DialNATS`, `webhook-ingest`'s `ListenAndServe`, and the api-gateway proxy.
- `GO-2026-4866` / `GO-2026-4947` / `GO-2026-5037` — `crypto/x509` auth-bypass and parsing bugs.
- `GO-2026-4865` / `GO-2026-4980` / `GO-2026-4982` — `html/template` XSS.
- `GO-2026-4976` / `GO-2026-4918` / `GO-2026-4971` — `net/http`, `httputil`, `net`.

The stdlib a binary carries comes from the **toolchain that compiled it** — i.e. the
`FROM golang:` line in each Dockerfile. Ground truth today:

- **18 Dockerfiles pin `golang:1.26.1`** → they ship all 13.
- **4 pin `golang:1.26.5`** → clean (api-gateway, risk-engine, schema-registry, kanz-halt).

This is how it happened, and it is the interesting part: a Dependabot base-image bump moved
**3 of 22** Dockerfiles to 1.26.5 and left 18 behind. Nothing failed, because nothing checks that
the images agree. The same shape as SEC-M3 — a fact split across many files with no guard over it.

govulncheck also found 3 vulnerabilities in imported packages and 18 in required modules that the
code **does not call**. Those are out of scope: this plan fixes what is reachable.

## Global Constraints

- Target version is **`golang:1.26.5`** — exact string, matching the 4 Dockerfiles already on it.
  It is the floor that clears all 13 (the highest "Fixed in" among them is 1.26.5).
- **Do not** modify `go.mod`'s `go 1.26.1` directive. It is a *minimum*, satisfied by a 1.26.5
  toolchain; raising it would force every contributor onto a toolchain download and is not needed
  to fix a single one of these vulnerabilities. Out of scope.
- Change **only** the `FROM golang:` line. No Dockerfile restructuring, no base-image family
  changes, no distroless changes.
- `gofmt`, `go build ./...`, `go vet ./...` stay clean under the default build **and**
  `-tags "binance okx redis anthropic"`.
- The existing `test/arch` suite stays green.

## Task 1 — Bump the 18 stale Dockerfiles to golang:1.26.5

**Files:** the 18 Dockerfiles pinning `golang:1.26.1`:

```
kanz/cmd/kanz-migrate/Dockerfile
kanz/services/{accounting,alternatives,archiver,audit,compliance,copilot,datamaster,lake-sink,
market-data,market-ingest,oms,regulatory,tv-sync,venue-binance,venue-okx,wealth,webhook-ingest}/Dockerfile
```

**Change:** in each, `FROM golang:1.26.1 AS build` → `FROM golang:1.26.5 AS build`. Nothing else.

**Requirements:**
1. All 22 Dockerfiles pin `golang:1.26.5` afterwards; zero remain on `1.26.1`.
2. Comments that name a version (if any) must be updated to match; a comment claiming 1.26.1 beside
   a 1.26.5 pin is a lie the next reader inherits.

**Verification (must be executed, not asserted):**
- `grep -rh "FROM golang:" --include=Dockerfile .` → every line reads `golang:1.26.5`.
- **The proof that matters:** run govulncheck under the *target toolchain*, not the laptop's:
  ```
  docker run --rm -v "$(pwd -W 2>/dev/null || pwd)/..:/src" -w /src/kanz golang:1.26.5 \
    sh -c 'go install golang.org/x/vuln/cmd/govulncheck@latest && GOFLAGS=-mod=mod $(go env GOPATH)/bin/govulncheck ./... 2>&1 | tail -5'
  ```
  Expect: **no reachable stdlib vulnerabilities** (the "Your code is affected by N vulnerabilities
  from the Go standard library" line must be gone). Report the actual output verbatim.
- Build one image end-to-end to prove the base is real and the build still works:
  `docker build -f kanz/services/archiver/Dockerfile -t sec-m4-probe .` from the **repo root**
  (these Dockerfiles take the repo root as context — see their header comments).

**Do not** run the full Go test suite for this task; no Go source changes.

## Task 2 — Guard the base images against partial bumps

**File:** `kanz/test/arch/baseimage_test.go` (new), package `arch`.

**Why:** the defect was not that 1.26.1 was old. It was that **3 of 22 images moved and 18 did
not**, and nothing noticed. A bump is only safe if a partial one fails the build.

**Requirements:**
1. A test that finds **every** `Dockerfile` under the module root, extracts each `FROM golang:<ver>`
   pin, and fails if **more than one distinct version** is in use — naming the odd ones out and
   which files carry them.
2. It must fail with a message that says what to do (bump the stragglers), not just what is wrong.
3. It must be **non-vacuous**: if it finds zero Dockerfiles or zero `FROM golang:` lines, that is a
   failure, not a pass (the walk broke, or the format changed).
4. Reuse `moduleRoot(t)` from the existing arch suite — do not reimplement it.
5. AST parsing is not applicable here (Dockerfiles are not Go); a line scan is correct. Normalise
   CRLF — `deployability_test.go` documents that git on Windows hands these files over with CRLF and
   that an exact match on `\n` silently failed for every service on a Windows checkout.

**Explicitly out of scope:** do not assert a specific version number. Pinning the *value* here would
mean editing this test on every legitimate bump, and a test that must be edited to let a change
through gets edited without thought. The invariant is **agreement**, not a particular version.

**Verification (must be executed):**
- The test passes on the current tree (after Task 1).
- **Mutation:** revert one Dockerfile to `golang:1.26.1`, run the test, confirm it FAILS and names
  that file; restore it and confirm it passes again. Report both outputs.
  Restore with a targeted edit — **not** `git checkout`, which would also discard Task 1's change to
  that file.
- `go test ./test/arch/...` green.
