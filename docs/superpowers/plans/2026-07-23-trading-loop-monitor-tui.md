# Trading-Loop Monitor TUI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A read-only terminal UI, `kanz-monitor`, that watches the live trading loop — a streaming order-lifecycle feed from the bus beside a polled panel of current positions, health, and the compliance/collateral counters.

**Architecture:** One new binary under `kanz/cmd/kanz-monitor`, built on Bubble Tea (the Elm-architecture Go TUI). Two independent data sources feed one model through Bubble Tea messages: (1) a **bus reader** that `SubscribeBroadcast`s `order.>` and `risk.position.>` — ephemeral, per-pod, `DeliverLastPerSubject`, cannot steal work from any durable consumer group, self-reaps in 5 minutes; (2) a **poller** that scrapes `/metrics` and the gateway `/v1` read routes on a ticker. The TUI mutates NO state and issues NO commands — it is a monitor, and its read paths are exactly the ones the e2e proof already uses.

**Tech Stack:** Go 1.26, Bubble Tea + Lip Gloss (`github.com/charmbracelet/bubbletea`, `.../lipgloss`), the existing `pkg/bus` consumer, `pkg/transport` for mesh, the generated `order.v1`/`domain.v1` SDK, and stdlib `net/http` for the poller.

## Global Constraints

- **READ-ONLY, ALWAYS.** The monitor never publishes, never issues a command, never writes Postgres. It subscribes and it GETs. Any task that adds a publish, a POST, or a DB write is a plan failure. It must be impossible for a monitor to move capital.
- **Never join a durable consumer group.** Use `Consumer.SubscribeBroadcast` (`pkg/bus/consumer.go:266`) ONLY — never `Consumer.Subscribe(ctx, subj, group, h)`. A queue-group subscription competes with the real OMS/tv-sync consumers and would STEAL deliveries from them. `SubscribeBroadcast` is ephemeral, per-connection, starts at `DeliverLastPerSubjectPolicy`, and has a 5-minute `InactiveThreshold` so it self-reaps. This is the single most dangerous mistake available in this plan.
- **Bus payloads are envelope-framed protobuf.** `SubscribeBroadcast`'s handler receives `(ctx, env *envelopepb.Envelope, payload []byte)` — already Unframed and validated by the consumer. Unmarshal `payload` into the `order.v1`/`domain.v1` message named by `env.GetEventType()`. Never hand-parse frames.
- **The subjects are exact — do not retype them.** They live in `kanz/services/oms/internal/order/events.go:20-35` (`EventTypeAccepted`…`EventTypeOutcome`) and the position subject builder in `kanz/internal/platform/subject/subject.go`. Import the constants where importable; otherwise copy the literals verbatim from those files, not from memory. This plan's guessed field names have been wrong before — read the proto.
- **Auth, local path:** plaintext NATS on `localhost:4222` for the bus (dev rig runs plaintext — the e2e proof dials it with zero credentials); an HS256 bearer minted by `kanz-devtoken` for the gateway `/v1` routes. Production is mTLS to the bus (an SVID via `transport.NewMesh`) and OIDC at the gateway; the monitor takes a TLS config from the mesh exactly as the services do, so the SAME binary works in both — a `--nats-plaintext` flag selects the dev path.
- **Do NOT reach tenant-scoped Postgres or the tv-sync SSE stream in this plan.** Both are viable (the map found them) but each carries a cost — Postgres needs an `app.tenant_id` GUC pool and RLS, the SSE stream is reachable only by bypassing the gateway direct to tv-sync. The bus already carries positions on the compacted `POSITION` stream, so the first version reads the book from the bus, not SQL. SSE and SQL are explicitly deferred (see Verification / known limits).
- **Run Go commands from `kanz/`.** `go 1.26.1`, `toolchain go1.26.5`. Do NOT pass `-race` — this host has no C compiler. Check `gofmt` by COUNTING output (`gofmt -l cmd/kanz-monitor/ | wc -l`), never by exit code — it exits 0 while listing drifted files.
- **New arch-guard exposure:** a new `cmd/` binary that dials NATS trips `TestOperatorCLIsHaveBrokerAccounts` (SEC-M3c) — but the monitor is a READER, not an operator, and adding it to `operatorSVIDs` would grant it a broker identity it should not have on the mesh. Task 6 addresses this deliberately; do not blindly add it to that map.

---

### Task 1: The binary skeleton and a Bubble Tea shell that quits cleanly

**Files:**
- Create: `kanz/cmd/kanz-monitor/main.go`
- Create: `kanz/cmd/kanz-monitor/model.go`
- Test: `kanz/cmd/kanz-monitor/model_test.go`

**Interfaces:**
- Produces: `type model struct{…}` implementing `tea.Model` (`Init() tea.Cmd`, `Update(tea.Msg) (tea.Model, tea.Cmd)`, `View() string`); `func newModel(cfg Config) model`; `type Config struct{ NATSURL string; GatewayURL string; Token string; Tenant string; PollInterval time.Duration; Plaintext bool }`.

- [ ] **Step 1: Add the dependencies**

```bash
cd kanz && go get github.com/charmbracelet/bubbletea@latest github.com/charmbracelet/lipgloss@latest
```

Then confirm they resolved into go.mod:

```bash
grep -E "charmbracelet/(bubbletea|lipgloss)" kanz/go.mod
```

Expected: both lines present. These are the only new external deps this plan introduces.

- [ ] **Step 2: Write the failing test for the model's quit behaviour**

Create `kanz/cmd/kanz-monitor/model_test.go`:

```go
package main

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// A monitor an operator cannot quit is a monitor they will kill with the process
// manager, losing the clean unsubscribe. q and ctrl+c must both quit.
func TestModel_QuitKeys(t *testing.T) {
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'q'}},
		{Type: tea.KeyCtrlC},
	} {
		m := newModel(Config{})
		_, cmd := m.Update(key)
		if cmd == nil {
			t.Fatalf("key %v produced no command; expected tea.Quit", key)
		}
		// tea.Quit is a func returning a tea.QuitMsg; invoke and check the type.
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("key %v did not map to tea.Quit", key)
		}
	}
}

// View must never panic on the zero model — Bubble Tea calls View before the
// first data message arrives, and a nil-map deref there crashes the UI on launch.
func TestModel_ViewOnEmptyModelDoesNotPanic(t *testing.T) {
	m := newModel(Config{})
	_ = m.View()
}
```

- [ ] **Step 3: Run it and watch it fail to compile**

```bash
cd kanz && go test ./cmd/kanz-monitor/ -run TestModel -v 2>&1 | tail -10
```

Expected: FAIL to compile — `undefined: newModel`, `undefined: Config`.

- [ ] **Step 4: Write the model shell**

Create `kanz/cmd/kanz-monitor/model.go`:

```go
package main

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Config is the monitor's read configuration. Every field is a READ coordinate —
// there is deliberately no producer, no signing key beyond a bearer token, and no
// write path anywhere in this struct. A monitor cannot move capital.
type Config struct {
	NATSURL      string        // the spine to subscribe to (read-only broadcast)
	GatewayURL   string        // api-gateway base URL for /v1 read routes + /metrics
	Token        string        // HS256 bearer for the gateway (kanz-devtoken locally)
	Tenant       string        // the tenant whose book to show; poller scopes to it
	PollInterval time.Duration // how often the poller scrapes; <=0 ⇒ 2s
	Plaintext    bool          // true ⇒ dial NATS without TLS (dev rig); false ⇒ mesh mTLS
}

// model is the whole UI state. It is mutated ONLY by Update, only in response to
// messages from the bus reader and the poller — never by those goroutines directly,
// which is Bubble Tea's concurrency contract and the reason the UI needs no locks.
type model struct {
	cfg Config

	// events is the rolling order-lifecycle feed (newest last), capped so a long
	// session cannot grow unbounded.
	events []lifecycleEvent

	// book is the current position per instrument, folded from the compacted
	// POSITION stream. Keyed "portfolio/instrument".
	book map[string]position

	// counters and health are the latest poll snapshot.
	counters counterSnapshot
	health   map[string]bool // service name → ready

	width, height int
	err           error // last non-fatal error, shown in a status line
}

func newModel(cfg Config) model {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	return model{
		cfg:    cfg,
		book:   map[string]position{},
		health: map[string]bool{},
	}
}

// The message and row types the later tasks fill in. Declared here so the model
// compiles and View can range over empty slices/maps without a nil panic.
type lifecycleEvent struct {
	At      time.Time
	Type    string // the order.order.* event_type, trimmed for display
	OrderID string
	Detail  string // venue, price×qty, reason — whatever the payload carries
}

type position struct {
	Portfolio, Instrument string
	Quantity, AvgPrice    string // exact RatStrings off the wire; never float
}

type counterSnapshot struct {
	Filled, Rejected, Quarantined, Ungoverned, Unpriced, SharedCollateral int
}

func (m model) Init() tea.Cmd { return nil } // Task 3/4 return the real startup cmds

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	}
	return m, nil
}

func (m model) View() string {
	// Task 5 replaces this with the real layout. A minimal non-panicking stub so
	// Bubble Tea's pre-data View call is safe.
	return "kanz-monitor — connecting…  (q to quit)\n"
}
```

- [ ] **Step 5: Write main.go — flags and the program run**

Create `kanz/cmd/kanz-monitor/main.go`. Model the flag/`run()`/`os.Exit(1)` shape on `cmd/kanz-devtoken/main.go` (read it first). It parses flags into `Config`, builds the `tea.Program`, and runs it:

```go
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "kanz-monitor: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	var cfg Config
	fs := flag.NewFlagSet("kanz-monitor", flag.ContinueOnError)
	fs.StringVar(&cfg.NATSURL, "nats", envOr("KANZ_NATS_URL", "nats://localhost:4222"), "spine URL to subscribe to (read-only)")
	fs.StringVar(&cfg.GatewayURL, "gateway", envOr("KANZ_GATEWAY_URL", "http://localhost:8080"), "api-gateway base URL for /v1 reads + /metrics")
	fs.StringVar(&cfg.Token, "token", os.Getenv("KANZ_TOKEN"), "HS256 bearer for the gateway (see kanz-devtoken)")
	fs.StringVar(&cfg.Tenant, "tenant", envOr("KANZ_TENANT", "__system__"), "tenant whose book to monitor")
	fs.DurationVar(&cfg.PollInterval, "poll", 2*time.Second, "poll interval for /metrics and /v1 reads")
	fs.BoolVar(&cfg.Plaintext, "nats-plaintext", true, "dial NATS without TLS (dev rig). false ⇒ mesh mTLS")
	// -spiffe-socket is read in Task 3 when Plaintext is false.
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	p := tea.NewProgram(newModel(cfg), tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
```

- [ ] **Step 6: Run the tests, build, and eyeball it**

```bash
cd kanz && go test ./cmd/kanz-monitor/ -run TestModel -v 2>&1 | tail -8
cd kanz && go build ./cmd/kanz-monitor/
```

Expected: both PASS/succeed. (Do not try to run the TUI in CI — it needs a TTY. A human runs `go run ./cmd/kanz-monitor` to see the shell and press `q`.)

- [ ] **Step 7: Commit**

```bash
git add kanz/cmd/kanz-monitor/ kanz/go.mod kanz/go.sum
git commit -m "feat(monitor): kanz-monitor TUI skeleton — read-only Bubble Tea shell

The client surface is the CLI by decision; this adds the first interactive one, a
READ-ONLY monitor of the trading loop. No producer, no command, no write path —
its Config carries only read coordinates. Quits cleanly on q/ctrl+c so the bus
unsubscribe runs.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: Decode one lifecycle event from a real envelope+payload

**Files:**
- Create: `kanz/cmd/kanz-monitor/decode.go`
- Test: `kanz/cmd/kanz-monitor/decode_test.go`

**Interfaces:**
- Produces: `func decodeLifecycle(env *envelopepb.Envelope, payload []byte) (lifecycleEvent, bool)` — maps an order.order.* FACT to a display row; `ok=false` for an event type the monitor does not render.
- Produces: `func decodePosition(env *envelopepb.Envelope, payload []byte) (position, bool)` — maps a `domain.v1.PositionState` to a book row.

**Why decode is its own task and its own test:** it is the one piece of loop-facing logic that is unit-testable without a broker, and it is where a guessed proto field name silently produces a blank row. Test it against real marshaled protos before wiring any network.

- [ ] **Step 1: Read the real payload protos**

```bash
grep -n "message OrderFilled\|message OrderRouted\|message OrderRejected\|message Fill\|message OrderState" -A 12 kanz-schemas/proto/order/v1/order_events.proto
grep -n "message PositionState" -A 14 kanz-schemas/proto/domain/v1/*.proto
```

Use the ACTUAL field names and getters these show. `OrderFilled` carries `order_id` and the fill detail rides its `Fill`/`OrderState` — confirm exactly what each FACT payload holds before writing a getter. Confirm the event_type constants against `services/oms/internal/order/events.go:20-35`.

- [ ] **Step 2: Write the failing test with real marshaled payloads**

Create `kanz/cmd/kanz-monitor/decode_test.go`. Build a real `orderpb.OrderFilled` (and a `domainpb.PositionState`), marshal it, wrap a minimal `envelopepb.Envelope{EventType: "order.order.filled"}`, and assert `decodeLifecycle` returns the right `Type`/`OrderID` and a non-empty `Detail`; assert an unknown event_type returns `ok=false`. Mirror the marshaling shape used in `services/webhook-ingest/internal/ingest/integration_test.go`. Use the verified field names from Step 1 — do not copy this plan's guesses.

- [ ] **Step 3: Run it, watch it fail to compile**

```bash
cd kanz && go test ./cmd/kanz-monitor/ -run 'TestDecode' -v 2>&1 | tail -10
```

Expected: `undefined: decodeLifecycle`.

- [ ] **Step 4: Write decode.go**

Implement both functions with a `switch env.GetEventType()` over the constants from `events.go`. For each rendered FACT, unmarshal the matching proto and fill `lifecycleEvent`/`position`. Quantities and prices go into the row as their exact wire strings via `dec.Str(dec.FromProto(...))` — NEVER as float64; this is a capital-path display and a rounded number shown to an operator is a wrong number. An unrendered event_type returns `ok=false` (the reader drops it silently — a monitor showing "unknown event" teaches the operator to ignore the feed).

- [ ] **Step 5: Run the tests**

```bash
cd kanz && go test ./cmd/kanz-monitor/ -count=1 2>&1 | tail -6
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add kanz/cmd/kanz-monitor/decode.go kanz/cmd/kanz-monitor/decode_test.go
git commit -m "feat(monitor): decode order-lifecycle and position FACTs into display rows

Amounts render as their exact wire RatStrings, never float64 — an operator
monitoring capital must never be shown a rounded quantity. An unrendered event
type is dropped, not surfaced as noise.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: The bus reader — SubscribeBroadcast order.> and risk.position.> into Bubble Tea messages

**Files:**
- Create: `kanz/cmd/kanz-monitor/busreader.go`
- Test: `kanz/cmd/kanz-monitor/busreader_test.go`

**Interfaces:**
- Consumes: `decodeLifecycle`/`decodePosition` (Task 2), `pkg/bus`, `pkg/transport`.
- Produces: `func (m model) startBusReader() tea.Cmd` — returns a `tea.Cmd` that dials the spine and streams; and the messages `busEventMsg lifecycleEvent`, `busPositionMsg position`, `busErrMsg struct{ err error }`.

**Why this shape:** Bubble Tea's model is single-threaded; a background goroutine must hand data in as `tea.Msg`s, not touch the model. The idiom is a `tea.Cmd` that owns the connection and pushes messages through `tea.Program.Send`. Because Bubble Tea calls `Init` once, the reader is started from `Init`/`Update`, and the dial happens off the UI thread.

- [ ] **Step 1: Write the reader**

Create `kanz/cmd/kanz-monitor/busreader.go`. It must:
- Build a `*bus.Consumer` over a `bus.DialNATS` connection. When `cfg.Plaintext`, pass no TLS. Otherwise build a mesh via `transport.NewMesh(ctx, cfg.SPIFFESocket)` and pass `mesh.Client` — mirror how `services/oms/cmd/oms/main.go` dials.
- Subscribe with **`consumer.SubscribeBroadcast(ctx, "order.>", handler)`** and a second `SubscribeBroadcast(ctx, "risk.position.>", handler)`. NEVER `Subscribe` with a group. Add a comment stating why: a queue group would steal deliveries from the real OMS/tv-sync consumers.
- In each handler, call the Task 2 decoders and, on `ok`, forward a `busEventMsg`/`busPositionMsg` via the program's `Send`. On a decode/validation error, forward a `busErrMsg` (the UI shows it in the status line; it does not crash).

Pass the `*tea.Program` into the reader (or use Bubble Tea's `tea.Cmd`-returns-msg pattern with a channel drained by a looping `tea.Cmd`) — pick the idiom the installed Bubble Tea version documents; both are standard. Whichever you choose, the model goroutine must remain the only writer of model state.

- [ ] **Step 2: Wire the reader into Init**

Change `model.Init` (Task 1) to return `m.startBusReader()`, and handle `busEventMsg`/`busPositionMsg`/`busErrMsg` in `Update`:
- `busEventMsg` → append to `m.events`, capping the slice at e.g. 200 (drop oldest).
- `busPositionMsg` → upsert `m.book[portfolio/instrument]` (last-write-wins; the POSITION stream is compacted so this matches its semantics).
- `busErrMsg` → set `m.err`.

- [ ] **Step 3: Test what is testable without a broker**

Create `kanz/cmd/kanz-monitor/busreader_test.go`. The dial needs a broker, so test the message-folding in `Update` instead: send a `busEventMsg` and assert it lands in `m.events`; send 201 and assert the cap held at 200 with the oldest dropped; send two `busPositionMsg` for one key and assert last-write-wins. This is the logic a broker cannot help you verify anyway.

```bash
cd kanz && go test ./cmd/kanz-monitor/ -run 'TestBus|TestModel' -count=1 -v 2>&1 | tail -12
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add kanz/cmd/kanz-monitor/busreader.go kanz/cmd/kanz-monitor/busreader_test.go kanz/cmd/kanz-monitor/model.go
git commit -m "feat(monitor): stream order.> and risk.position.> via SubscribeBroadcast

SubscribeBroadcast, never a queue group: the monitor is ephemeral and per-pod and
must never steal a delivery from the real OMS or tv-sync consumers. Events fold
into a capped feed; positions upsert last-write-wins, matching the compacted
POSITION stream.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: The poller — /metrics and the gateway /v1 read routes on a ticker

**Files:**
- Create: `kanz/cmd/kanz-monitor/poller.go`
- Test: `kanz/cmd/kanz-monitor/poller_test.go`

**Interfaces:**
- Produces: `func (m model) pollTick() tea.Cmd` (a `tea.Tick` that fires every `cfg.PollInterval`); message `pollMsg struct{ counters counterSnapshot; health map[string]bool; err error }`.
- Produces: `func parseCounters(metricsText string) counterSnapshot` — pull the named counters out of a Prometheus exposition-format body.

**Why parse text, not scrape a client:** the six counters the monitor shows are named metrics on `/metrics`; a tiny line parser over the exposition format is far less than pulling a Prometheus client dependency, and it is trivially unit-testable against a captured body.

- [ ] **Step 1: Write the failing parser test**

Create `kanz/cmd/kanz-monitor/poller_test.go` with a captured `/metrics` snippet containing the real counter lines and assert `parseCounters` extracts each. Use the EXACT metric names from `services/oms/cmd/oms/main.go`: `kanz_oms_orders_quarantined_total`, `kanz_compliance_ungoverned_orders_total`, `kanz_compliance_unpriced_orders_total`, `kanz_oms_shared_collateral_orders_total`, and the `kanz_bus_publish_total{...}` families. Also test that a missing metric yields 0, not an error — a fresh pod exports no counter until the first increment, and that must read as zero, not crash the poll.

- [ ] **Step 2: Run it, watch it fail**

```bash
cd kanz && go test ./cmd/kanz-monitor/ -run TestParseCounters -v 2>&1 | tail -8
```

Expected: `undefined: parseCounters`.

- [ ] **Step 3: Write poller.go**

`parseCounters` scans the metrics body line by line, matching each metric name and summing its samples (a counter split by a label like `reason=` sums across labels for the tile). `pollTick` returns `tea.Tick(cfg.PollInterval, …)` whose callback does the HTTP work off the UI thread and returns a `pollMsg`:
- GET `cfg.GatewayURL + "/metrics"` (or each service's `/metrics` — start with the OMS's, since it owns all six counters), Bearer `cfg.Token`, parse.
- GET each loop service's `/readyz` (through the gateway where proxied, direct where not) and fold `health[svc] = (200)`.
- Optionally GET `cfg.GatewayURL + "/v1/households/" + tenant` etc. for a value tile — but keep the first version to counters + health; positions already stream from the bus in Task 3.

Handle `cfg.Token == ""` gracefully: skip the authed routes, still scrape `/metrics` if it is unauthenticated locally, and set a status note. A missing token must degrade, not crash.

- [ ] **Step 4: Wire pollTick into Init and handle pollMsg**

`Init` now returns `tea.Batch(m.startBusReader(), m.pollTick())`. `Update` handles `pollMsg` by replacing `m.counters`/`m.health` and re-arming the tick (return `m.pollTick()` again).

- [ ] **Step 5: Run tests**

```bash
cd kanz && go test ./cmd/kanz-monitor/ -count=1 2>&1 | tail -6
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add kanz/cmd/kanz-monitor/poller.go kanz/cmd/kanz-monitor/poller_test.go kanz/cmd/kanz-monitor/model.go
git commit -m "feat(monitor): poll /metrics counters and service health on a ticker

A missing counter reads as zero, not an error — a fresh pod exports none until the
first increment. quarantined/ungoverned/unpriced/shared-collateral are the
incident signals an operator watches; they sit beside the live feed.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: The layout — feed pane beside the state/counters/health panel

**Files:**
- Modify: `kanz/cmd/kanz-monitor/model.go` (replace `View`)
- Modify: `kanz/cmd/kanz-monitor/main.go` (wire `-spiffe-socket`; guard the mTLS flag — see Step 0)
- Create: `kanz/cmd/kanz-monitor/view.go`
- Test: `kanz/cmd/kanz-monitor/view_test.go`

**Interfaces:**
- Produces: `func (m model) render() string` — the full frame, composed with Lip Gloss from the model.

- [ ] **Step 0: Close the mTLS-flag safety gap Task 3 exposed (do this first)**

Task 3 left `cfg.SPIFFESocket` permanently `""` because `main.go` never wired the flag — so `--nats-plaintext=false` today builds a DISABLED (nil-TLS) mesh and silently dials PLAINTEXT instead of failing. "Silently plaintext when the operator asked for mTLS" is the wrong direction for a security posture. Fix it in `main.go`:

- Add `fs.StringVar(&cfg.SPIFFESocket, "spiffe-socket", os.Getenv("SPIFFE_ENDPOINT_SOCKET"), "SPIFFE Workload API socket for the mesh SVID (required when --nats-plaintext=false)")`.
- After `fs.Parse`, if `!cfg.Plaintext && cfg.SPIFFESocket == ""`, return an error: mTLS was requested but there is no workload socket to obtain an SVID from, so the dial would silently degrade to plaintext — refuse rather than mislead. (`transport.NewMesh(ctx, "")` returns a disabled mesh with a nil Client, which is exactly the silent-plaintext path.)
- Delete the now-false `// -spiffe-socket is read in Task 3` comment left in main.go by Task 1.

Add a test in `main_test.go` (create it) asserting that a `Config{Plaintext:false, SPIFFESocket:""}` is rejected by whatever validation function you factor this into — pull the check into a small `validate(cfg) error` so it is unit-testable without running the TUI.

- [ ] **Step 1: Write the view**

Create `kanz/cmd/kanz-monitor/view.go`. Using Lip Gloss, compose a two-column frame that adapts to `m.width/m.height`:
- LEFT: the EXECUTION feed — the last N `m.events` newest-at-bottom, each a line `HH:MM:SS  event_type  order_id  detail`. Colour by type (filled green, rejected/quarantined red, routed/accepted default) using Lip Gloss styles, but degrade to plain when the terminal is narrow.
- RIGHT, stacked: a **Book** box (rows from `m.book`, instrument · qty · avg), a **Counters** box (the six numbers; render a non-zero `quarantined`/`ungoverned` in red — those are incidents), and a **Health** box (each service ✓/✗ from `m.health`).
- A status line: the tenant, the spine URL, connection state, and `m.err` if set. `q to quit`.

Wide content must scroll or truncate inside its box, never push the layout wider than `m.width`.

- [ ] **Step 2: Test the pure render on a populated model**

Create `kanz/cmd/kanz-monitor/view_test.go`: build a `model` with a couple of events, a book row, non-zero counters and mixed health, set `width/height`, call `render()`, and assert the output CONTAINS the order id, the instrument, the quarantined count, and a ✓ and ✗ — and that no line exceeds `m.width` (the horizontal-overflow guard). Render is a pure function of the model, so this needs no TTY.

- [ ] **Step 3: Point View at render**

Replace `model.View` to return `m.render()`.

- [ ] **Step 4: Test + build**

```bash
cd kanz && go test ./cmd/kanz-monitor/ -count=1 2>&1 | tail -6
cd kanz && go build ./cmd/kanz-monitor/ && gofmt -l cmd/kanz-monitor/ | wc -l
```

Expected: tests PASS, build clean, gofmt count 0.

- [ ] **Step 5: Commit**

```bash
git add kanz/cmd/kanz-monitor/view.go kanz/cmd/kanz-monitor/view_test.go kanz/cmd/kanz-monitor/model.go
git commit -m "feat(monitor): two-pane layout — live feed beside book, counters, health

A non-zero quarantined or ungoverned count renders red: those are the numbers that
should never be non-zero, and the operator must see them at a glance. Content
truncates inside its box; the frame never overflows the terminal width.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 6: The arch-guard reckoning — a reader is not an operator

**Files:**
- Modify: `kanz/test/arch/nats_identity_test.go` (or wherever the cmd/-dialer guard lives)
- Test: the modification IS the test change

**Interfaces:** none — this reconciles the new binary with the estate's identity guards.

**Why this is its own task:** a new `cmd/` binary that calls `bus.DialNATS` trips `TestOperatorCLIsHaveBrokerAccounts` (SEC-M3c), which demands an entry in `operatorSVIDs`. But `operatorSVIDs` grants a broker IDENTITY on the production mesh, and the operator SVIDs there carry publish rights. A read-only monitor must NOT be handed publish authority. The honest resolution is a distinct category, not a rubber-stamped operator entry.

- [ ] **Step 1: See the guard fail**

```bash
cd kanz && go test ./test/arch/ -run TestOperatorCLIsHaveBrokerAccounts -count=1 2>&1 | tail -8
```

Expected: FAIL naming `kanz-monitor` — a cmd/ dialer with no declared identity.

- [ ] **Step 2: Decide the category deliberately, then encode it**

Read the guard and `operatorSVIDs`. The monitor is a **read-only observer**, not an operator that publishes. Two honest options — pick per what the guard's structure supports and justify in the commit:

- **(a)** Add a separate `readOnlyMonitorSVIDs` (or a boolean tag on the entry) whose members are asserted to have, in `tenancy.yaml`, a `permissions` block that DENIES publish (e.g. `publish: { deny: [">"] }`) and allows only the subscribe + JetStream-consumer machinery the reader needs (`_INBOX.>`, and — because `SubscribeBroadcast` binds an ephemeral consumer — the `$JS.API.>`/`$JS.ACK.>` a consumer needs, exactly as the service-permissions work established). This makes "this identity can watch the loop and provably cannot move it" a guarded fact.
- **(b)** If the monitor is intended as a LOCAL dev tool only (plaintext, never on the mesh), assert instead that it is NOT in `operatorSVIDs` and carries no `tenancy.yaml` grant at all — and document that running it against the production mesh is out of scope until (a) is done.

Whichever you choose, the guard must end GREEN with the monitor's status EXPLICIT — never by loosening the guard to ignore unknown `cmd/` dialers, which would blind it to a real operator that forgot its SVID.

If you choose (a), also add the `kanz-monitor` SVID + its deny-publish `permissions` block to `tenancy.yaml`, and confirm the service-permissions and JetStream-machinery guards from earlier this session accept it (they will require the consumer machinery grant).

- [ ] **Step 3: Prove it and run the whole arch suite**

```bash
cd kanz && go test ./test/arch/ -count=1 2>&1 | tail -6
```

Expected: PASS, with the monitor's identity category explicit.

- [ ] **Step 4: Commit**

```bash
git add kanz/test/arch/nats_identity_test.go kanz/infra/nats/tenancy.yaml
git commit -m "test(arch): classify kanz-monitor as a read-only observer, not an operator

A new cmd/ dialer trips SEC-M3c, but the monitor must never hold the publish
authority an operator SVID carries. It is admitted as a read-only identity whose
tenancy grant DENIES publish and allows only the subscribe + consumer machinery —
so 'can watch the loop, provably cannot move it' is a guarded fact, not a comment.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 7: Prove it against the rig

**Files:** none (verification only)

- [ ] **Step 1: Full gate**

```bash
cd kanz && go build ./... && go vet ./cmd/kanz-monitor/ && go test ./... -count=1 2>&1 | tail -20
cd kanz && gofmt -l cmd/kanz-monitor/ | wc -l
```

Expected: green; gofmt count 0.

- [ ] **Step 2: Run it against the live rig and drive a trade through it**

With the rig up (see the `kanz-e2e-cluster-proof` memory), port-forward NATS, mint a token, and launch the monitor in one terminal:

```bash
kubectl port-forward -n kanz-messaging svc/nats 14222:4222 &
TOKEN=$(cd kanz && go run ./cmd/kanz-devtoken --secret dev-only-not-a-real-jwt-secret --tenant __system__ --role kanz-user)
cd kanz && go run ./cmd/kanz-monitor --nats nats://localhost:14222 --token "$TOKEN" --tenant __system__
```

In a second terminal, publish an alert through webhook-ingest exactly as the e2e proof does (a signed LIMIT order on a sim MIC). Confirm in the TUI:
- the lifecycle feed shows `signal.received → submit → accepted → routed → filled → outcome` for that order, live;
- the Book pane shows the resulting position;
- the Counters pane reflects reality (filled incremented; quarantined still 0).

Then, to prove the incident path renders: plant a quarantined order the way the quarantine rig-proof did, restart the OMS, and confirm the monitor's `quarantined` counter goes non-zero and renders red.

- [ ] **Step 3: Prove it steals nothing**

While the monitor runs, confirm the real loop still works end to end (the fill still lands, tv-sync still projects) — i.e. the `SubscribeBroadcast` reader has NOT diverted any delivery. This is the safety property the whole plan hinges on; verify it, do not assume it.

- [ ] **Step 4: Record on the board**

Add a row: the monitor exists, is read-only (guarded), streams the loop + polls counters, proven on the rig. State what it does NOT do yet — no Postgres reads, no tv-sync SSE, no historical scrollback beyond the 200-event cap — so the row does not read as more than it is.

```bash
bash tools/validate-board.sh KANZ_TASKS.md
git add KANZ_TASKS.md && git commit -m "docs(board): kanz-monitor — read-only trading-loop TUI, rig-proven

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Verification

The plan is complete when:

1. `cd kanz && go build ./... && go test ./...` is green; `gofmt -l cmd/kanz-monitor/ | wc -l` is 0.
2. `decodeLifecycle`/`decodePosition` and `parseCounters` are unit-tested against real marshaled protos and a real `/metrics` body; the event-cap and last-write-wins folding are tested in `Update`.
3. The bus reader uses `SubscribeBroadcast` and NEVER a queue group (grep the source to confirm no `consumer.Subscribe(` with a group argument exists in `cmd/kanz-monitor/`).
4. `go test ./test/arch/` is green with `kanz-monitor`'s identity category explicit — read-only, publish denied.
5. On the rig: a driven trade appears in the live feed, the book, and the counters; a planted quarantine turns the counter red; and the real loop is unaffected by the monitor watching it.
6. `bash tools/validate-board.sh KANZ_TASKS.md` exits 0.

**Known limits, to be stated on the board rather than glossed:**

- **Read-only by construction, and that is the whole point** — it cannot halt, cancel, or amend. Driving the loop stays the operator CLIs' job.
- **No historical view** beyond the in-memory 200-event cap and the compacted POSITION stream's current values. It is a live monitor, not a query tool; `orders`/`tv_facts` history (Postgres, RLS-scoped) is deferred.
- **No tv-sync SSE** — the richest per-account delta stream is reachable only direct-to-tv-sync (the gateway will not proxy SSE). Deferred; the bus `order.>`/`risk.position.>` streams cover the loop-level view.
- **Local-first auth.** Proven on the plaintext rig with an HS256 token. The mesh-mTLS path (`--nats-plaintext=false`) is written but only exercised where a real SVID exists — the same standing SPIRE limit every other service carries.
