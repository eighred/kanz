# SEC-M5 — the vulnerability scanner measures a toolchain nothing ships

## Context

SEC-M4 moved all 22 Dockerfiles to `golang:1.26.5`, taking the shipped images from 13 reachable
stdlib vulnerabilities to 0. Its whole-branch review then found the other half of the problem:

`.github/workflows/security.yml` (`dependency-scan`) runs `actions/setup-go@v5` with
`go-version-file: kanz/go.mod` → Go **1.26.1** → then `govulncheck ./...`. Against the *fixed* tree
that still reports **13 reachable vulnerabilities, exit 3** — while the images it exists to protect
are clean. So SEC-M4 turned a **true alarm into a false one**, and false alarms get muted.

The compounding half is the real defect: SEC-M4's guard enforces only that the **Dockerfiles**
agree, while the scanner measures a **different** version derived from `go.mod`. So *"all 22 agree
on an old, vulnerable base"* passes every gate in CI. That is SEC-M4's failure mode relocated — one
version living in two places with nothing comparing them.

**The scope decision has been taken by the lead (2026-07-16): use the `toolchain` directive.** This
supersedes SEC-M4's plan, which put `go.mod` out of scope on the grounds that raising the `go`
directive would force contributors onto a toolchain download. A `toolchain` line does **not** raise
the `go 1.26.1` minimum; it does mean the go command auto-selects 1.26.5, which is the accepted
cost.

## Ground truth — already verified by the controller, do not re-derive

Executed on this box (host toolchain go1.26.1, exactly like CI's setup-go):

- Adding `toolchain go1.26.5` to `kanz/go.mod` makes `go env GOVERSION` report **`go1.26.5`** — the
  auto-switch works without touching the `go 1.26.1` directive.
- `govulncheck ./...` then reports **"Your code is affected by 0 vulnerabilities"** (from 13). This
  is precisely what `security.yml` will do.
- `go build ./...`, `go vet ./...` and `go test ./test/arch/...` are all green under 1.26.5.
- The change is **2 lines** in go.mod and produces **no `go.sum` churn**.
- **`kanz/go.mod` is CRLF.** A perl/sed edit anchored on `$` without `\r?` silently does nothing.

Five workflows derive Go from `kanz/go.mod` (`security.yml`, `build.yml`, `kanz-ci.yml`,
`latency.yml` ×2), so this one directive corrects all of them.

## Global Constraints

- **Do not** change the `go 1.26.1` directive. It is the language minimum; only the `toolchain` line
  is added. Raising `go` is not the decision that was taken.
- Target toolchain is exactly **`go1.26.5`** — the same version the 22 Dockerfiles pin.
- `gofmt`, `go build ./...`, `go vet ./...` stay clean under the default build **and**
  `-tags "binance okx redis anthropic"`.
- The existing `test/arch` suite stays green.
- Do not touch `preview.yml`'s `go-version: '1.26'`. It is a floating minor that already resolves to
  a current patch, and pinning it would add another unguarded home for the version — the exact
  defect this task closes. Out of scope.

## Task 1 — Add the toolchain directive

**File:** `kanz/go.mod`

**Change:** add `toolchain go1.26.5` after the `go 1.26.1` line. Nothing else. Preserve CRLF.

**Requirements:**
1. `go 1.26.1` is unchanged.
2. `toolchain go1.26.5` is present.
3. No `go.sum` change, no dependency change.

**Verification (must be EXECUTED, output pasted verbatim into the report):**
- `grep -E "^(go|toolchain)" kanz/go.mod` → shows both lines.
- `go env GOVERSION` → `go1.26.5`.
- **The proof that matters** — reproduce what `security.yml` does, on this host's 1.26.1:
  `cd kanz && GOFLAGS=-mod=mod govulncheck ./... 2>&1 | tail -6`
  Expect **"Your code is affected by 0 vulnerabilities"**. (`govulncheck` is already installed at
  `$(go env GOPATH)/bin/govulncheck`.) Report the output verbatim.
- `go build ./...` and `go vet ./...` clean, default **and** `-tags "binance okx redis anthropic"`.

## Task 2 — Make the guard see the Go source-of-truth

**File:** `kanz/test/arch/baseimage_test.go` (extend; do not create a second file).

**Why:** the guard currently compares Dockerfiles to each other. After Task 1 the toolchain version
lives in **two** places — the Dockerfiles' `FROM golang:` pins and `go.mod`'s `toolchain` — and
nothing compares them. Leaving it there would reproduce SEC-M4 one level up: the next bump moves one
and not the other, and every gate stays green.

**Requirements:**
1. Extend the existing agreement check so `go.mod`'s `toolchain goX.Y.Z` participates: the version it
   names must equal the version every Dockerfile pins. A disagreement fails the build, naming
   `go.mod` and the version it holds beside the Dockerfiles' version.
2. Keep the existing behaviour intact: Dockerfile-vs-Dockerfile disagreement still fails; bumps are
   still directed at the **newest** version, never the majority; comparison stays numeric per
   component; non-vacuity and CRLF normalisation survive.
3. **Non-vacuous about its new input too:** if `go.mod` has no `toolchain` line, or it cannot be
   parsed, that is a FAILURE, not a skip — a guard that silently ignores a missing source-of-truth is
   how the version drifts back apart.
4. Do not assert a specific version number anywhere. The invariant is **agreement**, not a value.
5. Reuse `moduleRoot(t)`.

**Verification (must be EXECUTED, output pasted verbatim):**
- The test passes on the tree after Task 1.
- **Mutation A:** change `go.mod`'s toolchain to `go1.26.4`, confirm the test FAILS naming `go.mod`
  and the mismatch. Restore with `git checkout -- go.mod` and confirm green.
- **Mutation B:** delete the `toolchain` line entirely, confirm the test FAILS (requirement 3).
  Restore and confirm green.
- **Mutation C:** revert one Dockerfile to `golang:1.26.1`, confirm the pre-existing behaviour still
  fires and still directs UP. Restore and confirm green.
- `go test ./test/arch/... -count=1` green; `gofmt -l .` clean; `git status --short` shows no
  modified tracked files when finished.
