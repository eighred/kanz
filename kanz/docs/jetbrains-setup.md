# JetBrains (GoLand / IntelliJ IDEA) setup

Nothing in this repo is editor-specific — there is no `.vscode/` or
`.devcontainer/`, so moving to JetBrains is a clean swap. But the project is
**not** open-and-go: four things will make the IDE look broken (or make tests
fail at random) if you skip them. Do these once.

Read `onboarding.md` first — this page only covers what is JetBrains- and
Windows-specific.

## Which IDE

The repo is 710 Go files, 58 Python, 43 proto, 16 TS, 11 SQL, and **all active
work (the execution system: `oms`, `tv-sync`, `webhook-ingest`, `market-ingest`)
lives in `kanz/`, which is pure Go.**

| | Use it when |
|---|---|
| **GoLand** *(recommended)* | You work on the execution system. Ships Go, protobuf, SQL/database tools, Docker/K8s, and JS/TS — everything the active direction needs. |
| **IntelliJ IDEA Ultimate** | You also work meaningfully on `kanz-py` (Python). The Go plugin is the same engine as GoLand; you additionally get first-class Python. GoLand has no first-class Python support. |

**Minimum version:** the module is on `go 1.26.1`, so you need a current release
(2025.2+). An older GoLand will not recognise the toolchain.

Claude Code has an official JetBrains plugin, so the existing workflow continues
inside the IDE.

## 1. Generate the schema SDK *before* opening the project

`kanz/go.mod` has `replace github.com/kanz-eng/kanz-schemas-go =>
../kanz-schemas/gen/go`, and `gen/` is gitignored (generated code is never
committed, EVT-15a). **Open the project without generating it and GoLand shows
thousands of unresolved imports** — the project is not broken, the SDK is just
missing.

Normally `make generate` (i.e. `buf generate`) does this. On this Windows box it
does not work, for two independent reasons:

- **`~/go/bin/buf.exe` is permanently blocked by Windows Application Control.**
  A *downloaded* exe is blocked; an exe you *build yourself into a project-local
  directory* runs fine. So build buf from source:
  ```sh
  GOBIN="$(pwd)/.gotmp/bin" go install github.com/bufbuild/buf/cmd/buf@v1.45.0
  ```
- **`kanz-schemas/proto/reference/v1/structured.proto` is broken**
  (`unknown type PaymentFrequency`), which fails the *whole-workspace* build, so
  even a working buf cannot generate everything. Until that is fixed, generate
  into an isolated workspace containing only the protos you need plus their
  import closure (copy them + a minimal `buf.yaml`), then copy the resulting
  `.pb.go` files into `kanz-schemas/gen/go/`, and run `go mod tidy` there.

This is a **local** step only; CI regenerates with a real buf.

## 2. Fix the test-binary block (`GOTMPDIR`) — do this first

Windows Application Control intermittently blocks **freshly built test binaries
in `%TEMP%`** (`Uygulama Denetimi ilkesi bu dosyayı engelledi`). `go test` — and
therefore GoLand's test runner — fails at random, and the suite silently
under-runs.

Point Go's build temp somewhere that is not `%TEMP%`. Set it in Go's own env so
it applies to **every** tool (CLI, GoLand, `go vet`, anything):

```sh
mkdir -p "$HOME/.go-tmp"
go env -w GOTMPDIR="$HOME/.go-tmp"     # undo with: go env -u GOTMPDIR
```

This is the single most effective fix and is why the shared run configurations
also set `GOTMPDIR` explicitly (belt and braces). The `Makefile` exports it too,
for CI.

## 3. Build tags — otherwise the connectors look broken

The exchange connectors are behind `//go:build binance` / `//go:build okx`; the
default build is deliberately vendor-free. Without tags configured, every venue
file shows as greyed out with errors.

**Settings → Go → Build Tags & Vendoring → Custom tags:** `binance okx`

Note: with both tags on, the `!binance` / `!okx` stub files (`venues_*_off.go`)
are excluded from the build. **That is correct**, not an error.

## 4. Go modules environment

**Settings → Go → Go Modules → Environment:** `GOFLAGS=-mod=mod`

The module pins this so a stale generated SDK is regenerated rather than failing
the build.

## 5. Line endings — when `gofmt -l` flags files you never touched

`.gitattributes` pins `*.go` and `*.proto` to `text eol=lf`, because gofmt emits
LF: a CRLF working copy makes `gofmt -l` list a file whose *formatting* is
perfect, and `make fmt` / `make lint` then look broken for a reason that has
nothing to do with the code.

The attribute only takes effect on files checked out *after* it was added. A
clone (or a working tree) that predates it keeps CRLF, and the symptom is a
handful of files that `gofmt -l` reports forever no matter how often you format
them.

**Diagnose with `git ls-files --eol`, not by grepping for carriage returns.**

```sh
git ls-files --eol '*.go' | awk '$2 == "w/crlf" {print $NF}'   # worktree is CRLF
git ls-files --eol '*.go' | awk '$1 != "i/lf"   {print $NF}'   # index is not LF
```

The two columns answer different questions and only the first is normally the
problem:

| | meaning | what it means for you |
|---|---|---|
| `i/lf` | the **index** (what git stores) is LF | correct — nothing to commit |
| `w/crlf` | your **working copy** is CRLF | a stale checkout; fix locally, do **not** commit |

If the index is already `i/lf` — which it is for every `.go` file in this repo —
then **there is nothing to fix in the repository**. Re-checkout the offending
files so the attribute applies:

```sh
rm <files> && git checkout -- <files>
git ls-files --eol '*.go' | awk '$2 == "w/crlf"' | wc -l   # expect 0
```

`git status` will show nothing modified afterwards, which is the confirmation
that this was a local artifact and not repository drift.

### Why the obvious check is wrong

The tempting diagnostic is to count carriage returns:

```sh
git show HEAD:path/to/file.go | grep -c $'\r'      # DO NOT trust this
```

In Git Bash `$'\r'` does not always survive into `grep` as a literal CR. When it
degrades to an empty pattern, grep matches **every** line, and the count comes
back equal to the file's line count — which reads exactly like "every line ends
CRLF". That misreading turns a local checkout artifact into an apparent
committed-CRLF problem, and the "fix" is a no-op line-ending churn across files
nobody touched. `git ls-files --eol` asks git directly and cannot be fooled this
way.

## Shared run configurations

`.idea/runConfigurations/` is **tracked** (the root `.gitignore` ignores the rest
of `.idea/`), so these come with the clone, already carrying the right tags and
env:

- **All tests** — the whole suite, default vendor-free build
- **OMS tests (binance okx)** — the OMS suite with both connectors linked
- **OMS (binance okx)** — run the OMS binary with both connectors linked

If GoLand cannot resolve the `<module>` on first open, pick the module once from
the dropdown in the configuration and it sticks.

## Gotchas that are *not* blockers

- **`-race` does not work on this box.** It requires cgo; without a C toolchain
  you get `-race requires cgo`. Leave GoLand's "Run with race detector"
  unchecked, or set `CGO_ENABLED=1` with a C compiler installed. CI runs it.
- **`make` is not installed on Windows** (not shipped with Git for Windows). The
  Makefile is the canonical task list and works in CI/WSL/Linux; on Windows
  either install it (`scoop install make`) or run the underlying `go` commands —
  the shared run configurations cover the common ones.

## Verify the setup

```sh
cd kanz
go build ./...                                   # default build (vendor-free)
go build -tags "binance okx" ./services/oms/...  # connectors compile
go test ./services/oms/...                       # no Application Control failures
go list -deps ./services/oms/cmd/oms | grep -c coder/websocket   # must print 0
```

In the IDE: `okx_connector.go` and `binance_connector.go` should show **no**
errors, and the "OMS tests (binance okx)" configuration should run green.
