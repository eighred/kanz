# Windows + Go environment setup

Nothing in this repo is editor-specific. What follows is **environment** setup:
four things that will make the project look broken — or make tests fail at
random — if you skip them. They apply to the CLI, to `go vet`, and to whatever
editor you point at the tree. Do them once.

Read `onboarding.md` first; this page only covers what is Windows- and
toolchain-specific.

## 1. Generate the schema SDK *before* opening the project

`kanz/go.mod` `replace`s the schema SDK module to `../kanz-schemas/gen/go` (the
module path itself is in `go.mod` — read it there rather than trusting a copy
here), and `gen/` is gitignored (generated code is never committed, EVT-15a).
**Open the project without generating it and you get thousands of unresolved
imports** — the project is not broken, the SDK is just missing.

`make generate` (i.e. `buf generate`) does this. On this Windows box there is one
obstacle: **`~/go/bin/buf.exe` is permanently blocked by Windows Application
Control.** A *downloaded* exe is blocked; an exe you *build yourself into a
project-local directory* runs fine. So build buf from source:

```sh
GOBIN="$(pwd)/.gotmp/bin" go install github.com/bufbuild/buf/cmd/buf@v1.45.0
```

This is a **local** step only; CI regenerates with a real buf.

## 2. Fix the test-binary block (`GOTMPDIR`) — do this first

Windows Application Control intermittently blocks **freshly built test binaries
in `%TEMP%`** (`Uygulama Denetimi ilkesi bu dosyayı engelledi`). `go test` fails
at random, and the suite silently under-runs — which is the dangerous part: a
run that never compiled half the packages still exits 0.

Point Go's build temp somewhere that is not `%TEMP%`. Set it in Go's own env so
it applies to **every** tool, not just the shell you set it in:

```sh
mkdir -p "$HOME/.go-tmp"
go env -w GOTMPDIR="$HOME/.go-tmp"     # undo with: go env -u GOTMPDIR
```

This is the single most effective fix. The `Makefile` exports `GOTMPDIR` too,
pointing at `kanz/.gotmp`, for CI.

## 3. Build tags — otherwise the connectors look broken

The exchange connectors are behind `//go:build binance` / `//go:build okx`; the
default build is deliberately vendor-free. Without tags configured, every venue
file shows as greyed out with errors.

The tags have to reach `gopls`, which is a per-editor setting. In VS Code, in
`.vscode/settings.json` (untracked — see `.gitignore`):

```json
{ "go.buildTags": "binance okx" }
```

On the command line, pass them explicitly: `go build -tags "binance okx" ./...`.

Note: with both tags on, the `!binance` / `!okx` stub files (`venues_*_off.go`)
are excluded from the build. **That is correct**, not an error.

## 4. Go modules environment

```sh
go env -w GOFLAGS=-mod=mod
```

The module pins this so a stale generated SDK is regenerated rather than failing
the build. Setting it via `go env -w` covers every tool; an editor-local Go
environment setting works too, but only for that editor.

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

## Gotchas that are *not* blockers

- **`-race` does not work on this box.** It requires cgo; without a C toolchain
  you get `-race requires cgo`. Set `CGO_ENABLED=1` with a C compiler installed,
  or leave it to CI — CI runs it, and until it does, a concurrency claim is
  unproven.
- **`make` is not installed on Windows** (not shipped with Git for Windows). The
  Makefile is the canonical task list and works in CI/WSL/Linux; on Windows
  either install it (`scoop install make`) or run the underlying `go` commands.

## Verify the setup

```sh
cd kanz
go build ./...                                   # default build (vendor-free)
go build -tags "binance okx" ./services/oms/...  # connectors compile
go test ./services/oms/...                       # no Application Control failures
go list -deps ./services/oms/cmd/oms | grep -c coder/websocket   # must print 0
```

With the tags configured, `okx_connector.go` and `binance_connector.go` should
show no errors in the editor either.
