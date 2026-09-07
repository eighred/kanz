# Windows verification setup

1. Move `GOTMPDIR` outside `%TEMP%`; Windows Application Control can block fresh
   binaries there and silently under-run tests. The Makefile's in-module
   `.gotmp` is valid, but formatting must use `git ls-files` rather than a tree
   walk.
2. Generate the schema SDK before Go commands. If the downloaded `buf.exe` is
   blocked, build buf into a project-local binary directory.
3. Set Go's module behavior to `-mod=mod` so generated SDK dependencies resolve.
4. Build tags are `redis`, `anthropic` and `perf`; no Binance or OKX tags exist.
5. Diagnose line endings with `git ls-files --eol`. A `w/crlf` file with an
   `i/lf` index is a local checkout artifact and should not create a commit.

`go test -race` requires CGO and normally cannot run on this Windows host. Record
that limit and rely on an actually executed Linux CI race job for the claim.
