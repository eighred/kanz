# Narrow the OMS Price Subscription — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop the OMS folding the estate's highest-volume stream, which its mark source provably discards.

**Architecture:** `OMS_PRICE_SUBJECT` defaults to `market.>`, so the OMS receives every market event including `market.book.snapshot` — L2 depth, 12 Kafka partitions, described in `topics-job.yaml` as "the highest-volume streams by far." That subject carries a `*marketpb.OrderBookSnapshot`, **not** a `MarketDataEvent`, so the fold unmarshals it as the wrong type, gets an empty instrument id, and returns nil. Every one. Narrowing to `market.*.trade` and `market.*.quote` matches the fold's actual consumption set exactly.

**Tech Stack:** Go 1.26, NATS/JetStream subject wildcards via `pkg/bus`.

## Global Constraints

- **This must not change which marks the OMS holds.** The new subject set is the fold's existing consumption set expressed as subjects — a behaviour-preserving narrowing, not a policy change. If you find a payload the fold uses that the new subjects would miss, stop and report.
- No floats. `*big.Rat` internally.
- Do NOT wire `bus.WithRetry` anywhere (`test/arch/bus_dlq_test.go` fails the build).
- The price spine stays a **broadcast** (`SubscribeBroadcast`), not a work queue — a durable group load-balances ticks across replicas, so each pod would hold a different mark map and the same order would be admitted by one and refused by another. `test/arch/price_spine_test.go` guards this; do not route around it.
- Do not weaken, rename, or delete any existing test.
- Build/test from `kanz/` with `GOFLAGS=-mod=mod`. Full suite with `-p 1`. Currently **159 test packages ok, 0 failures**.
- TDD: every test watched failing before the implementation.

---

### Task 1: Subscribe only the subjects the fold consumes

**Files:**
- Modify: `kanz/services/oms/internal/config/config.go` (`PriceSubject` at `:102-104`, its default at `:148`)
- Modify: `kanz/services/oms/cmd/oms/main.go` (`:206` log field, `:370-378` the subscription goroutine)
- Test: `kanz/services/oms/internal/config/config_price_test.go` (existing file — add to it)

**Interfaces:**
- Consumes: `(*mark.Source).Handle`, `consumer.SubscribeBroadcast(ctx, subject string, h bus.EventHandler) error`.
- Produces: `Config.PriceSubjects []string` (plural) replacing `Config.PriceSubject string`.

**The ground truth this rests on, established before writing the plan:**

`market.v1.MarketDataEvent` publishes on `market.<assetClass>.<variant>` (`services/market-data/internal/feed/bussink.go:82-98`), where the variant token is derived from the payload's oneof:

| payload | variant token | does the fold use it? |
|---|---|---|
| `Trade` | `trade` | **yes** — last trade price |
| `Quote` | `quote` | **yes** — bid/ask mid |
| `Bar` | `bar` | no — explicitly ignored |
| anything else | `unknown` | no |

and `market.book.snapshot` (`internal/marketedge/ingest/engine.go:27,132`) carries `*marketpb.OrderBookSnapshot` from `book.Book.Snapshot` — a **different message type entirely**.

So `market.*.trade` + `market.*.quote` is the fold's consumption set, expressed as subjects. NATS `*` matches exactly one token, so `market.book.snapshot` cannot match (`snapshot` ≠ `trade`/`quote`), and neither can `market.*.bar`. **The subject filter and the fold's payload switch are now two expressions of the same fact**, derived from the same `variant()` function.

- [ ] **Step 1: Write the failing test**

Add to `kanz/services/oms/internal/config/config_price_test.go`:

```go
func TestPriceSubjectsDefaultToTheFoldsConsumptionSet(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"market.*.trade", "market.*.quote"}
	if len(cfg.PriceSubjects) != len(want) {
		t.Fatalf("PriceSubjects = %v, want %v", cfg.PriceSubjects, want)
	}
	for i := range want {
		if cfg.PriceSubjects[i] != want[i] {
			t.Fatalf("PriceSubjects = %v, want %v", cfg.PriceSubjects, want)
		}
	}
}

// The default must NOT be a bare market.> wildcard. That subject carries
// market.book.snapshot — an OrderBookSnapshot, not a MarketDataEvent — which
// the fold unmarshals as the wrong type and discards. It is the highest-volume
// stream in the estate, and folding it was pure waste.
func TestPriceSubjectsDoNotIncludeTheBookSpine(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, s := range cfg.PriceSubjects {
		if s == "market.>" || strings.HasPrefix(s, "market.book") {
			t.Fatalf("PriceSubjects contains %q, which delivers OrderBookSnapshot messages the "+
				"mark fold cannot use — the OMS would decode and discard the estate's "+
				"highest-volume stream", s)
		}
	}
}

func TestPriceSubjectsAreConfigurable(t *testing.T) {
	t.Setenv("OMS_PRICE_SUBJECTS", "market.crypto.trade, market.crypto.quote")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"market.crypto.trade", "market.crypto.quote"}
	if len(cfg.PriceSubjects) != 2 || cfg.PriceSubjects[0] != want[0] || cfg.PriceSubjects[1] != want[1] {
		t.Fatalf("PriceSubjects = %v, want %v (whitespace around commas must be trimmed)",
			cfg.PriceSubjects, want)
	}
}

// An empty or whitespace-only override must be an ERROR, not silently zero
// subjects. A pod that subscribes to nothing folds no marks, and every
// MARKET/STOP order is then refused PRICE_UNAVAILABLE forever — a trading
// outage that looks like a quiet config typo.
func TestEmptyPriceSubjectsIsAnError(t *testing.T) {
	for _, v := range []string{" ", ",", " , "} {
		t.Setenv("OMS_PRICE_SUBJECTS", v)
		if _, err := config.Load(); err == nil {
			t.Fatalf("Load accepted OMS_PRICE_SUBJECTS=%q, which subscribes to nothing and "+
				"refuses every market order forever", v)
		}
	}
}
```

Add `"strings"` to the test file's imports if absent.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/oms/internal/config/ -run PriceSubjects -v -count=1`
Expected: compile failure — `cfg.PriceSubjects undefined`.

- [ ] **Step 3: Change the config**

In `kanz/services/oms/internal/config/config.go`, replace the `PriceSubject` field at `:102-104`:

```go
	// PriceSubjects are the market-data subjects the OMS folds into its
	// reference-mark source, so the pre-trade gate can value MARKET/STOP
	// orders (COMP-M2).
	//
	// The default is the mark fold's CONSUMPTION SET expressed as subjects, not
	// a convenient wildcard. market.v1.MarketDataEvent publishes on
	// market.<assetClass>.<variant> where the variant token comes from the
	// payload oneof (bussink.go: Trade→trade, Quote→quote, Bar→bar), and the
	// fold uses Trade and Quote only. NATS `*` matches exactly one token, so
	// these two subjects cover every asset class — present and future — while
	// structurally excluding market.*.bar and, critically, market.book.snapshot.
	//
	// That last one is why this is not `market.>`: book snapshots carry an
	// OrderBookSnapshot, a DIFFERENT message type, which the fold unmarshals as
	// a MarketDataEvent, finds empty, and discards. It is the highest-volume
	// stream in the estate (12 Kafka partitions), and the OMS was decoding all
	// of it to throw all of it away.
	PriceSubjects []string
```

Replace the default at `:148` and add parsing after the struct literal is built, beside the existing `PriceMaxAge` validation:

```go
		PriceSubjects:          splitSubjects(envOr("OMS_PRICE_SUBJECTS", "market.*.trade,market.*.quote")),
```

and after the literal:

```go
	if len(cfg.PriceSubjects) == 0 {
		return Config{}, fmt.Errorf("OMS_PRICE_SUBJECTS: at least one subject is required; " +
			"a pod subscribing to nothing folds no marks and refuses every MARKET/STOP order")
	}
```

Add the helper beside `envOr`:

```go
// splitSubjects parses a comma-separated subject list, trimming whitespace and
// dropping empty entries. An all-empty input yields an empty slice, which Load
// rejects — subscribing to nothing is a silent trading outage, not a default.
func splitSubjects(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
```

`strings` and `fmt` are already imported by this file.

- [ ] **Step 4: Run the config tests**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/oms/internal/config/ -v -count=1`
Expected: PASS — the four new tests plus every pre-existing config test.

- [ ] **Step 5: Update the composition root**

In `kanz/services/oms/cmd/oms/main.go`, the observer's log field at `:206` currently reads `"subject", cfg.PriceSubject`. Change it to:

```go
				"portfolio", portfolioID, "instrument", instrumentID, "subjects", cfg.PriceSubjects)
```

Replace the subscription goroutine at `:370-378` with one per subject:

```go
	for _, subject := range cfg.PriceSubjects {
		wg.Add(1)
		go func(subject string) {
			defer wg.Done()
			logger.Info("oms subscribing to the price spine (broadcast)", "subject", subject)
			err := consumer.SubscribeBroadcast(ctx, subject, marks.Handle)
			if err != nil && !errors.Is(err, context.Canceled) {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}(subject)
	}
```

The `subject` parameter is passed explicitly rather than captured, matching the `sub` loop above it in the same file.

- [ ] **Step 6: Build, vet, and the arch guards**

Run: `cd kanz && GOFLAGS=-mod=mod go build ./... && GOFLAGS=-mod=mod go vet ./... && GOFLAGS=-mod=mod go test ./test/arch/ -count=1`
Expected: clean, and all arch tests pass — in particular `price_spine_test.go`, which requires the price spine to use `SubscribeBroadcast` and not the work-queue `subs` slice. If it fails, you have routed around the determinism guard.

- [ ] **Step 7: Prove the narrowing is behaviour-preserving**

Write a throwaway test in `kanz/internal/marketdata/mark/` that hands `Handle` a marshalled `*marketpb.OrderBookSnapshot` (the payload `market.book.snapshot` carries, built from `marketpb.OrderBookSnapshot`) and asserts **no mark is recorded** — confirming the fold never used that subject and nothing is lost by no longer receiving it.

Record the output, then **delete the throwaway** and confirm `git status` is clean. This is evidence for the report, not a permanent test: the fold's contract is "Trade and Quote only", already covered by its own suite.

- [ ] **Step 8: Full suite**

Run: `cd kanz && GOFLAGS=-mod=mod go test -p 1 ./...`
Expected: 159 test packages ok, 0 failures.

- [ ] **Step 9: Commit**

```bash
cd kanz && gofmt -w services/oms/
git add kanz/services/oms/
git commit -m "perf(oms): fold only the market subjects the mark source consumes"
```

---

### Task 2: Update the board

**Files:**
- Modify: `KANZ_TASKS.md` — the `market.>` volume row

- [ ] **Step 1: Record what changed and what did not**

The row currently says the OMS consumes `market.>` with no bound, and that it needs a cluster load test. Update it: the subscription is now `market.*.trade` + `market.*.quote`, which **excludes `market.book.snapshot`** — the highest-volume stream, carrying a message type the fold provably discarded.

**State plainly what this does and does not fix.** It removes a large, verified source of pure waste. It does **not** bound the remaining subscription: trade and quote volume is still unbounded, the mark map still has no eviction, and every replica still folds every tick because the spine is a broadcast. The cluster load test is still needed — this narrows the input, it does not cap it.

- [ ] **Step 2: Correct an overstated claim in the same row**

The COMP-M2 work recorded that `DeliverLastPerSubject` "warms a cold pod's map" on startup. That is **weaker than it reads**: it delivers the last message per *subject*, and all crypto trades share the single subject `market.crypto.trade`. So a starting pod warms **one instrument per subject**, not its whole map. Correct that wherever the board states it.

- [ ] **Step 3: Validate the table**

Run: `cd /c/Users/root/Desktop/eighred-kanz && sh .superpowers/sdd/validate-board.sh KANZ_TASKS.md`
Expected: `board OK: N rows, 5 columns each`, exit 0. This script **exits non-zero** on a malformed row — do not replace it with an inline `awk`, which exits 0 on a finding and lets a `&& git add` chain commit through a failure.

**Watch for literal `|` characters in prose** — they split a markdown table cell.

- [ ] **Step 4: Commit**

```bash
git add KANZ_TASKS.md
git commit -m "docs(board): the OMS folds only mark-bearing subjects; warming claim corrected"
```

---

## Verification (whole feature)

- [ ] `cd kanz && GOFLAGS=-mod=mod go build ./... && GOFLAGS=-mod=mod go vet ./...` — clean.
- [ ] `cd kanz && GOFLAGS=-mod=mod go test -p 1 ./...` — 159 test packages ok, 0 failures.
- [ ] The default `PriceSubjects` is `["market.*.trade", "market.*.quote"]` and contains no `market.>` or `market.book*`.
- [ ] An empty/whitespace `OMS_PRICE_SUBJECTS` is an error, not zero subscriptions.
- [ ] `test/arch/price_spine_test.go` still passes — the spine is still a broadcast.
- [ ] A throwaway test confirmed the fold records no mark from an `OrderBookSnapshot` payload, then was deleted.

## Out of scope

- **Bounding the remaining volume.** Trade/quote rates are still unbounded and the mark map still has no eviction. Both stay on the board; this narrows the input rather than capping it.
- **Per-instrument subscription.** The OMS cannot know which instruments it needs before an order names one, and the mark must exist *before* admission — so demand-driven subscription is circular without a warm-up path that does not exist.
