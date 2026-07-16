# DATA-M6 — the replay reader hangs and over-reads; CI has been red on it for 2 days

## Context

`tools/replay` is how state is rebuilt from the durable log — **it is the DR path**.
`TestReaderRange` fails against a real broker and has been failing in CI since `d121875`
(2026-07-15), the commit that wired `TEST_KAFKA_BROKERS` into `kanz-ci.yml:292` **specifically so
these Kafka-gated tests would stop silently skipping**. They stopped skipping and started failing;
96 commits landed on a red `main` and nobody read it.

Reproduced by the controller three times, on two brokers, one of them pristine and untouched by
any other test:

```
--- FAIL: TestReaderRange/offset_range_covers_everything (36.06s)
    reader_integration_test.go:106: bad=29759308 want 1     (also seen: 50418749, 80849060)
--- FAIL: TestReaderRange/time_range_filters (9.05s)
    reader_integration_test.go:129: ok=6 want 5 (events 2..6 inclusive)
```

The off-by-one is **bit-for-bit identical every run** — a deterministic logic bug, not flake. The
busy-loop's magnitude varies but it fires every run.

## THE CONTRACT — decided by the lead + controller. Do not re-litigate; implement it.

The `Range` doc comment (`tools/replay/reader.go:18-22`) already states the contract:

> *"EndOffset is inclusive; EndTime is exclusive, matching the [start, end) convention of common
> windowing."*

1. **`EndTime` is EXCLUSIVE. `reader.go:280` (`!m.Time.Before(EndTime)`) is CORRECT. The TEST IS
   CORRECT.** It sets `start=t0+2s`, `end=t0+7s` with an explicit `// exclusive` annotation, and
   expects 5 events — `[t0+2s, t0+7s)` covers events 2,3,4,5,6. Its comment "events 2..6
   inclusive" describes the **events covered**, not the end bound. `validate()` corroborates:
   `!r.EndTime.After(*r.StartTime)` rejects `End == Start`, which is half-open semantics.
   **DO NOT "fix" this by editing the assertion to match the code.** The reader returns 6 and the
   reader is wrong.
2. **`EndOffset` is INCLUSIVE.** Unchanged.
3. **NEW — replay is a SNAPSHOT of history and MUST terminate at end-of-log.** Probe the
   high-water mark once, at seek time, and stop at `min(EndOffset, HWM-1)`. A replay that blocks
   waiting for messages that do not exist yet is *tailing*, and a DR restore that never returns is
   useless. **This contradicts the `Range` doc's stated rationale** — *"so the read terminates in
   finite time without needing a high-water-mark probe"* — and that rationale is precisely what
   produced a 30-second hang. **The doc changes, not the decision.** Update it to say what the
   reader now does and why: the end bound bounds the range; the HWM bounds reality; the read stops
   at whichever comes first.

## What is PROVEN vs. what you must ROOT-CAUSE

**Proven by the controller — take these as given:**
- The test is right about the time window (see above).
- `ok == 20` **passes** in `offset_range_covers_everything`; only `bad` fails. So the reader
  **delivers every message correctly** — it just never terminates.
- `end := int64(99)` with ~11 messages/partition means `m.Offset > *EndOffset` **never fires**,
  because no message past the end ever arrives. `FetchMessage` blocks until the 30s ctx expires.
- `go list -deps ./tools/replay/...` does not contain lake-sink. DATA-M5 is not involved.

**NOT proven — you must find this empirically, and you must NOT guess:**
- **Why `time_range_filters` returns 6 events instead of 5.** The controller formed and discarded
  several hypotheses (partition-hash collision, `SetOffsetAt` precision, the malformed frame's
  unset `Time` defaulting to ~now, ListOffsets returning -1 on an empty partition) and confirmed
  none of them. **Instrument first: print every event the reader returns — partition, offset,
  Kafka time, event id — and the seek offset chosen per partition. Identify the SIXTH event
  concretely before you change one line.** Report what it actually was.
- Note the topic has **2 partitions** and keys alternate `key-0`/`key-1` via `&kafka.Hash{}`, so
  which `i` lands on which partition is not obvious — establish it, don't assume it.
- Note the malformed frame at `reader_integration_test.go:79-84` sets **no `Time`**, so its
  timestamp is broker/writer-assigned (≈now), an hour after `t0`. Establish whether that matters.

## Global Constraints

- **A real broker is running for you at `localhost:9092`** — `export TEST_KAFKA_BROKERS=localhost:9092`.
  Same image/config as CI (`apache/kafka:3.9.0`, single-node KRaft, `AUTO_CREATE_TOPICS=false`).
  **Do not start another broker. Do NOT stop, remove, or `docker rm` ANY container** — unrelated
  containers exist on this machine and destroying them has already cost real work this session.
- **Never `git checkout -- .` / `git checkout <path>` / `git stash`.** It has destroyed
  uncommitted work in this repo twice. Undo with targeted edits.
- Do not change `pkg/bus`.
- `gofmt -l` clean, `go vet` clean.

## Task 1 — root-cause and fix the over-read (`ok=6 want 5`)

1. **Instrument and report before fixing.** Print every returned event (partition/offset/time/id)
   and the per-partition seek offset. State plainly, in your report, which event is the sixth and
   why it was included.
2. Fix the reader so `[StartTime, EndTime)` returns exactly the events in the window. If the cause
   is in `seekStart`/`SetOffsetAt` behaviour rather than the end filter, **fix it there** — and if
   the correct fix is a defensive start-time filter in the read loop (because a seek is a
   positioning hint, not a guarantee), say so and justify it. A seek that lands early is not
   automatically a bug in Kafka; a reader that trusts it blindly may be.
3. `TestReaderRange/time_range_filters` must pass **without modifying its assertions or bounds.**

## Task 2 — the reader must terminate at end-of-log

1. Probe the high-water mark per partition once at seek time and stop at `min(EndOffset, HWM-1)`;
   the same reasoning applies to an `EndTime` beyond the last message's timestamp — that read must
   terminate too, not block.
2. Prefer an existing seam: `kafka-go`'s `Reader.ReadLag`, `Conn.ReadLastOffset`, or the
   `ListOffsets` the reader already reaches for in `SetOffsetAt`. **Reuse; do not hand-roll a
   protocol call.**
3. `TestReaderRange/offset_range_covers_everything` must pass **and must not take ~30 seconds** —
   its runtime is the proof. Assert nothing about timing in the test itself; just report the
   before/after durations.
4. **Update the `Range` doc comment** (`reader.go:18-22`). Its current rationale is false and is
   what caused this. Say what the reader does now: the end bound bounds the request, the HWM
   bounds reality, the read stops at the first of the two. A stale rationale left in place is what
   `KANZ_BRAIN.md` calls a fossil — *"the next engineer will read it as a requirement."*

## Task 3 — the test helper must not spin

`drain()` (`reader_integration_test.go:157-176`) loops on `Next(ctx)` and counts any
`err != nil && ev.Envelope == nil` as `bad`, forever. Once the ctx is dead, `Next` returns
`ctx.Err()` immediately and the helper busy-loops tens of millions of times.

Tasks 1-2 mean the ctx should never expire — but this helper is why the failure reported
`bad=29759308` instead of *"the reader hung"*, which is what actually happened. **A test helper
that turns a hang into a wrong number costs the next reader an hour.** Make `drain` return on a
context error, distinguishing it from a genuine per-message decode failure. Keep `bad` counting
**only** malformed frames — `bad == 1` must still mean "exactly one malformed frame", which is
what the test is asserting.

## Verification (EXECUTE, paste real output)

- `TestReaderRange` **fully green**, with **no changes to its assertions or bounds** — the only
  permitted test change is `drain`'s termination (Task 3).
- Paste before/after **durations** for `offset_range_covers_everything` (the ~36s → fast drop is
  Task 2's proof).
- Your Task 1 instrumentation output identifying the sixth event, verbatim.
- `export TEST_KAFKA_BROKERS=localhost:9092; go test ./tools/replay/... -count=1`
- Full suite: `go test ./... -count=1` from `kanz/`. **Run it WITH `TEST_KAFKA_BROKERS` set** —
  without it, every Kafka-gated test silently skips and the run proves nothing. That skip is the
  entire reason this bug survived two days.
- `gofmt -l .` and `go vet ./...` clean.
