package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
)

// lifecycleSubject and positionSubject are the two subjects this monitor
// subscribes on the bus. They are named constants (rather than inline string
// literals in runBusReader) specifically so busreader_test.go can assert on
// them directly: dialing a real broker to observe the subscribed subject is
// not viable from this package's tests, so pinning the constant is the
// regression guard against positionSubject widening back to risk.position.>
// (see runBusReader's comment for why that would collide the aggregate and
// per-venue position grains).
const (
	lifecycleSubject = "order.>"
	positionSubject  = "risk.position.changed.>"
)

// busEventMsg carries one decoded order-lifecycle row into Update.
type busEventMsg lifecycleEvent

// busPositionMsg carries one decoded position-book row into Update.
type busPositionMsg position

// busErrMsg carries a non-fatal bus error (dial, subscribe, or an unparseable
// payload) into Update. It is shown in the status line and must never crash
// the UI — see model.Update.
type busErrMsg struct{ err error }

// startBusReader launches the bus connection on its own goroutine and returns
// the tea.Cmd that waits for the first message it produces.
//
// Bubble Tea's model is single-threaded, so the goroutine started here NEVER
// touches m directly — it only ever writes tea.Msg values onto m.busCh. Update
// re-issues waitForBusMsg after folding every message, so the channel is
// drained continuously for the life of the program. This is the standard
// Bubble Tea "realtime feed" idiom (a looping tea.Cmd over a channel), chosen
// over threading a *tea.Program through the model because it keeps model a
// plain value type with no dependency on the Program that owns it.
func (m model) startBusReader() tea.Cmd {
	go m.runBusReader(context.Background())
	return waitForBusMsg(m.busCh)
}

// waitForBusMsg blocks for exactly one message off ch and hands it to Update.
// Update calls this again after every message it folds, so the feed never
// stops draining for as long as the program runs.
func waitForBusMsg(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		return <-ch
	}
}

// runBusReader dials the spine read-only and subscribes to the two subjects
// this monitor renders. It blocks until ctx is canceled (or a dial step
// fails), so it must always run on its own goroutine — never called from the
// model goroutine.
//
// READ-ONLY, deliberately: there is no bus.Producer anywhere in this file, no
// Publish call, and nothing here can move capital. It only subscribes and
// decodes.
func (m model) runBusReader(ctx context.Context) {
	var tlsConfig *tls.Config
	if !m.cfg.Plaintext {
		// SEC-M3: the production broker requires a client SVID; a nil TLSConfig
		// is a plaintext client it refuses at the handshake. Mirrors
		// services/oms/cmd/oms/main.go's dial exactly.
		mesh, err := transport.NewMesh(ctx, m.cfg.SPIFFESocket)
		if err != nil {
			m.busCh <- busErrMsg{err: fmt.Errorf("bus: mesh: %w", err)}
			return
		}
		defer func() { _ = mesh.Close() }()
		tlsConfig = mesh.Client
	}

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: m.cfg.NATSURL, Name: "kanz-monitor", TLSConfig: tlsConfig})
	if err != nil {
		m.busCh <- busErrMsg{err: fmt.Errorf("bus: dial: %w", err)}
		return
	}
	defer func() { _ = client.Close() }()

	consumer, err := bus.NewConsumer(client)
	if err != nil {
		m.busCh <- busErrMsg{err: fmt.Errorf("bus: consumer: %w", err)}
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		// SubscribeBroadcast, NEVER Subscribe with a group: a queue-group
		// Subscribe JOINS the durable consumer group that the real OMS shares
		// across its replicas, and would STEAL deliveries from it — the
		// lifecycle events the OMS needs to process would instead be handed to
		// this read-only monitor. SubscribeBroadcast is an ephemeral,
		// per-connection consumer that gets its OWN copy of every event, so the
		// monitor can never divert a delivery the trading system needs.
		if err := consumer.SubscribeBroadcast(ctx, lifecycleSubject, func(_ context.Context, env *envelopepb.Envelope, payload []byte) error {
			// decodeLifecycle's ok=false is a DELIBERATE drop for event types
			// this monitor does not render (see decode.go) as well as for an
			// unparseable payload — the two are indistinguishable at this
			// signature, and decode.go documents that the caller must drop
			// silently rather than surface "unknown event" noise that would
			// teach the operator to ignore the feed. So unlike decodePosition
			// below, a false here is never forwarded as busErrMsg.
			if row, ok := decodeLifecycle(env, payload); ok {
				m.busCh <- busEventMsg(row)
			}
			return nil
		}); err != nil {
			m.busCh <- busErrMsg{err: fmt.Errorf("%s: %w", lifecycleSubject, err)}
		}
	}()

	go func() {
		defer wg.Done()
		// SubscribeBroadcast, NEVER Subscribe with a group: risk.position.changed.>
		// is consumed by the risk engine and tv-sync through their own durable
		// groups, and a monitor joining that group would steal position
		// updates meant for them. SubscribeBroadcast's DeliverLastPerSubject
		// start is also what gives this monitor the CURRENT book immediately
		// on connect, rather than only future changes.
		//
		// The subject is narrowed to risk.position.changed.> (the AGGREGATE
		// grain) rather than the broader risk.position.> the tenancy grant
		// permits: the OMS position projector publishes TWO grains per fill —
		// the portfolio-level aggregate on risk.position.changed.> and a
		// per-venue holding on risk.position.venue.changed.> — and BOTH carry
		// the same portfolio_id + instrument_id. decodePosition does not
		// branch on event_type, so both would decode to the same book key
		// "portfolio/instrument" and collide under this model's last-write-wins
		// upsert (see model.go), making the Book pane show one venue's subset
		// or the aggregate non-deterministically for any portfolio holding an
		// instrument across more than one venue. risk.position.changed.>
		// requires the subject's 3rd token to be "changed", so it matches the
		// aggregate subject but NOT risk.position.venue.changed.> (whose 3rd
		// token is "venue") — the two grains cannot collide once subscribed
		// this way. The Book pane has no venue dimension (position is
		// {Portfolio, Instrument, Quantity, AvgPrice}), so the aggregate is
		// the only grain that is ever correct to render here.
		if err := consumer.SubscribeBroadcast(ctx, positionSubject, func(_ context.Context, env *envelopepb.Envelope, payload []byte) error {
			// decodePosition's ok=false means only an unparseable payload (its
			// doc comment says so explicitly) — a genuine error, so it is
			// surfaced rather than dropped.
			row, ok := decodePosition(env, payload)
			if !ok {
				m.busCh <- busErrMsg{err: fmt.Errorf("%s: unparseable payload", positionSubject)}
				return nil
			}
			m.busCh <- busPositionMsg(row)
			return nil
		}); err != nil {
			m.busCh <- busErrMsg{err: fmt.Errorf("%s: %w", positionSubject, err)}
		}
	}()

	wg.Wait()
}
