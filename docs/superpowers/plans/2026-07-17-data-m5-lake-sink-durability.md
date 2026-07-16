# DATA-M5 — lake-sink must not commit an offset for a row that is not durable

## Context

**lake-sink permanently loses committed financial events on any hard crash.** Verified end to
end by the controller:

1. `pkg/bus/kafka.go:133-140` — `Subscribe` commits the Kafka offset **as soon as the handler
   returns nil**: `if err := h(ctx, out); err != nil { continue }` then `r.CommitMessages(ctx, m)`.
   `ReaderConfig.CommitInterval: 0` (`kafka.go:109`) means that commit is **synchronous** — once
   the handler returns nil, the offset is gone for good.
2. The handler is `cdc.EventSink.Handle`, which calls `sink.Write`.
   `services/lake-sink/internal/sink/file.go:51-67` — `Write` only fills a `bufio.Writer`.
   **Process-local userspace memory.** It returns nil.
3. The fsync happens on a **5-second ticker in an unrelated goroutine**
   (`cmd/lake-sink/main.go:126-139`).

So the offset advances past rows that exist only in a buffer. On OOMKill, eviction, panic, or
SIGKILL-after-grace, up to 5 seconds of **already-committed** events vanish; the consumer group
resumes **past** them on restart. They are gone from the lakehouse — the service whose stated
purpose is permanent history — and **nothing detects the gap**.

**Clean shutdown is already safe** and must stay that way: `main.go:83` calls `fileSink.Close()`,
which flushes and fsyncs (`file.go:84-97`). Rollouts do not lose data. This task is about the
crash path only.

**The archiver one hop upstream already gets this right** (DATA-M1 acks NATS only after Kafka
acknowledges). Same invariant — *never ack before durable* — enforced upstream, dropped
downstream.

## The decision

**`EventSink.Handle` must not return nil until the row it landed is durable.** The bus commits on
a nil return, so the durability barrier belongs exactly at the boundary that triggers the commit.

**Why this is affordable and why we are NOT batching.** The obvious objection to an fsync per
event is throughput. It does not apply here:
- `CommitInterval: 0` means the consumer **already** pays a synchronous broker round-trip per
  message. An fsync is comparable or cheaper.
- lake-sink drains **business events only** — its own manifest (`infra/deploy/lake-sink-deploy.yaml:130-136`)
  **deliberately excludes** the firehose: *"market.book/market.crypto (highest-volume,
  re-fetchable from the venue)"*. The topic set is orders, positions, balances, mandates,
  signals. Business rate, not tick rate.
- The alternative (batch the fsync, defer the commit until after it) requires changing
  `pkg/bus`'s Handler contract, which ~20 other consumers depend on. That is a large blast radius
  to buy throughput nobody has measured a need for. **If measurement later shows fsync-per-event
  is the bottleneck, deferred commit is the answer — as a measured change, not a guess.**

**Duplicates are explicitly acceptable; loss is not.** `sink.go:36-39` states the contract:
*"Sinks see at-least-once delivery — a duplicate carries the same event_id and is resolved by
downstream compaction, so Write need not dedup."* Flushing before returning can, on a crash
between fsync and commit, produce a duplicate on redelivery. **That is the correct direction of
failure** and the architecture already absorbs it.

## Global Constraints

- **Do not change `pkg/bus`.** The fix lives in lake-sink. The bus's commit-on-nil-return contract
  is correct; lake-sink was lying to it.
- **The barrier must live in `cdc.EventSink`, NOT in `cmd/lake-sink/main.go`.** `main` is
  package main and cannot be tested. `EventSink` already holds the `sink.Sink` (`cdc/sink.go:30`,
  `sink.Sink` has `Flush()` at `sink/sink.go:40-44`) — the seam already exists, use it.
- **Every path in `Handle` that returns nil after writing a row must flush.** Read `Handle`
  first: it has multiple returns (ok / transient resolve failure / permanent decode failure that
  still lands a row with `decode_error`). A permanent-decode row is still a landed row and still
  commits an offset — it must be durable too. Missing one path reintroduces the bug for that path
  only, which is worse than not fixing it (it becomes rare and therefore invisible).
- **Clean shutdown must remain safe.** Do not remove `fileSink.Close()` at `main.go:83`.
- `gofmt -l` clean, `go vet` clean, `go test ./... -count=1` green from `kanz/` with
  `GOFLAGS=-mod=mod` (expect 230 packages, 0 failures).

## Task 1 — the crash test, and it must FAIL first

**File:** `kanz/services/lake-sink/internal/cdc/durability_integration_test.go` (new).

**Gate it exactly like the existing harness** — `os.Getenv("TEST_KAFKA_BROKERS")`, `t.Skip` when
unset. Follow `pkg/bus/kafka_integration_test.go:18-20` and
`services/archiver/internal/archive/archiver_integration_test.go:41`. **Do not invent a new
gate.** CI already starts a KRaft broker and sets this variable
(`.github/workflows/kanz-ci.yml:182-199`), so a correctly-gated test runs there automatically.
`KAFKA_AUTO_CREATE_TOPICS_ENABLE=false` — create the topic explicitly; copy how the archiver's
integration test does it.

**A broker is already running locally for you** at `localhost:9092`
(`export TEST_KAFKA_BROKERS=localhost:9092`).

**The scenario — this models a pod crash faithfully:**

1. Produce N=10 small envelopes to a fresh topic.
2. Wire the real chain exactly as `runSink` does: `bus.NewConsumer` → `cdc.NewEventSink(decoder,
   fileSink, ...)` → a `sink.FileSink` rooted at `t.TempDir()`. Use a nil/absent registry resolver
   so rows land envelope-only — this test is about durability, not decode.
3. Subscribe with a fresh consumer group and consume all 10. Each `Handle` returning nil commits
   the offset **synchronously**.
4. **Simulate the crash: do NOT call `Flush()`. Do NOT call `Close()`.** Cancel the context and
   walk away. A process that is SIGKILLed never gets to run either one — that is precisely what
   this models. The `bufio.Writer`'s contents are process-local and die with it.
5. **Assert: all 10 rows are readable FROM DISK**, by walking the landing root and counting
   NDJSON lines in the part-files. Reading the files (not the buffer) is the whole point — what
   is on disk is what survives.
6. **Then prove the loss is PERMANENT, not lag:** start a second consumer on the **same group**
   and assert it receives **zero** messages, because the offsets were committed. This is the
   assertion that separates "durability lag" from "silent permanent loss". Without it, a reader
   could believe the 5s ticker would have saved them; it would not — nothing ever re-reads a
   committed offset.

**Keep the rows small** so `bufio` cannot auto-flush them for the wrong reason and make the test
pass by accident. Assert the exact count, never `> 0`.

**Verification (EXECUTE, paste output):** this test must **FAIL before Task 2** — showing rows on
disk well short of 10 (expect 0) — and pass after. If it passes before the fix, you have not
reproduced the bug: **stop and report, do not proceed.**

## Task 2 — the fix

**File:** `kanz/services/lake-sink/internal/cdc/sink.go`.

1. After a row is written, flush before returning nil. `sink.Sink` already exposes `Flush()`, and
   `FileSink.Flush()` already does `bufio.Flush()` + `f.Sync()` (`file.go:100-105`) — a real
   fsync. **You are calling machinery that already exists, not building any.**
2. A flush failure is a **transient** failure: return the error so the bus does **not** commit and
   the broker redelivers. Landing the row again is safe (at-least-once + compaction).
3. Cover **every** nil-returning path that wrote a row, including the permanent-decode-error row.
4. Update the comment on `Handle` to state the contract it now keeps: *it does not return nil
   until the row is durable, because the bus commits the offset on a nil return.* This is the
   load-bearing constraint and it is invisible in the code — it is the one comment worth writing
   here.

**Unit tests** (no broker needed, alongside the existing `cdc` tests):
- `Handle` returns an error when `Flush` fails → the bus will not commit.
- `Handle` flushes on the permanent-decode-error path too.
- A fake sink asserting flush-happened-after-write ordering.

## Task 3 — delete the ticker it replaces

**Files:** `kanz/services/lake-sink/cmd/lake-sink/main.go`,
`kanz/services/lake-sink/internal/config/config.go`.

**Why:** with Task 2, `Handle` flushes every row, so the buffer is empty by the time the 5s ticker
fires. It flushes nothing. Leaving it is not harmless: `config.go:39-40` documents
`FlushInterval` as *"bounds how long a buffered row waits before it is durable"* — a statement
that will be **false**, describing a durability model the code no longer uses. `KANZ_BRAIN.md`
names this failure directly: *"A decision whose stated rationale names a thing that no longer
exists is not a decision any more — it is a fossil, and the next engineer will read it as a
requirement."*

1. Remove the ticker goroutine (`main.go:125-139`) and `FlushInterval` /
   `LAKE_SINK_FLUSH_INTERVAL` (`config.go:39-40, 56`).
2. **Keep `Flush()` on the `sink.Sink` interface and keep `Close()` at `main.go:83`** — clean
   shutdown still flushes, and `Close` still uses `flush()`. Only the *ticker* is redundant.
3. Check whether any manifest sets `LAKE_SINK_FLUSH_INTERVAL` before removing it
   (`grep -rn LAKE_SINK_FLUSH_INTERVAL` across `infra/` and `.github/`). If one does, remove it
   there too in the same commit — a Deployment setting a variable nothing reads is the next
   reader's trap.
4. Update the `sink.Sink` interface doc (`sink/sink.go:36-39`) if it now misdescribes when a row
   becomes durable.

## Verification (EXECUTE all, paste real output)

- Task 1's test **failing before Task 2**, passing after. Both outputs, verbatim.
- `export TEST_KAFKA_BROKERS=localhost:9092; go test ./services/lake-sink/... -count=1`
- Full suite: `go test ./... -count=1` from `kanz/` — 230 packages, 0 failures.
- `gofmt -l .` and `go vet ./...` clean.
- `grep -rn "LAKE_SINK_FLUSH_INTERVAL" .` returns nothing outside the plan/git history.
- **Never `git checkout -- .`** to undo anything — it has destroyed uncommitted work in this repo
  twice. Use targeted edits.
