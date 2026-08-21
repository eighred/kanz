package execution

import (
	"context"
	"log/slog"
	"sync"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/pkg/bus"
)

// SubjectMarketCryptoTrade is the subject the crypto venue adapters publish
// reference-mark ticks on. Shared for the same reason SubjectStateHealed is:
// one subject whichever exchange produced the tick, so tv-sync's MarkSource
// subscribes once.
const SubjectMarketCryptoTrade = "market.crypto.trade"

// MarkTickPublisher is the one place a venue adapter turns a polled price into a
// market.v1 Trade FACT, and the one place a tick that does not reach the bus is
// COUNTED AND NAMED (#673).
//
// # What it replaces
//
// Both crypto adapters' ticker feeds built this envelope themselves and threw
// the publish error away, in two services, with a *slog.Logger in scope on both
// sides and no counter on either. A publish that was refused on every tick
// therefore produced the same observable estate as an exchange with nothing to
// report: no line, no series, no difference.
//
// # Why that mattered, stated no wider than it is
//
// The CONSEQUENCE was already contained. The Source in internal/marketdata/mark
// expires an entry past its maxAge and answers nil rather than a stale price,
// and OMSPriceFeedStalled in infra/observability/alerts/operational.rules.yaml
// fires when a fold that HELD marks holds no live one. A dead feed does not
// silently value a book forever.
//
// What was missing is DIAGNOSIS. "The venue went quiet", "the connection
// dropped" and "the broker refused every envelope" reach an operator as the same
// stalled feed, and they are three different repairs. The error is the only
// thing that separates them, and it was the one value being discarded.
//
// Broker refusal is the realistic member of that set here, not a hypothetical:
// bus.Validate requires tenant_id, a time.Ticker loop has no inbound delivery to
// inherit one from, and this exact feed has already run with an empty
// ProducerConfig.Tenant — see TestEveryBusProducerConfigSetsATenant, which
// exists because of it.
//
// # What it deliberately does NOT do
//
// It does not retry, reconnect, back off, or take the process down. A missed
// tick is recoverable and the next poll is the retry; a reconnect storm driven
// by a refused ENVELOPE would never converge, because reconnecting is not the
// repair for bytes the broker will refuse identically forever. It also never
// substitutes a price — the feeds around it skip an unfetchable or unparseable
// tick rather than fabricate one, and that stance is unchanged.
//
// It reports. That is the whole job.
//
// # The signal shape, and why it is two signals
//
// Copied from the noteUngoverned shape in internal/compliance: the observer
// fires on EVERY drop, because that is the number a dashboard and an alert need,
// and the WARN is rate-limited, because a five-second ticker over a persistently
// refused subject would otherwise write a line per instrument per tick and bury
// the rest of the log on the day it matters.
//
// It differs from noteUngoverned in one way ON PURPOSE. That latch is
// once-per-key forever, because a portfolio does not stop being ungoverned by
// itself. A mark feed does recover, so this latch CLEARS on the next successful
// publish for that instrument: a feed that breaks, heals and breaks again earns
// a WARN per outage rather than one for the first and silence for the rest.
//
// # Why a dedicated counter rather than kanz_bus_publish_total
//
// kanz_bus_publish_total{subject,result} does move on these — when the Publisher
// happens to be a metrics-wired *bus.Producer, which is a fact about a
// composition root and not a property of this seam. Two things it cannot carry:
// it is per SUBJECT, so it cannot say WHICH instruments went dark while others
// kept publishing, and no rule in this repository watches it — there is no alert
// over result="error" anywhere under infra/. A counter nothing reads, and a
// counter that cannot name the affected instrument, are not the signal somebody
// needs at 03:00, so this one is labelled by instrument and is the venue
// adapters' own.
type MarkTickPublisher struct {
	pub    Publisher
	logger *slog.Logger
	mic    string
	// onDropped is called for EVERY dropped tick; the composition root wires it
	// to a counter. nil ⇒ the WARN is the only surface, which is a degraded
	// posture rather than a broken one.
	onDropped func(mic, instrumentID string)

	mu sync.Mutex
	// dropping holds the instruments currently inside a failing episode — the
	// ones already named in a WARN. Cleared per instrument by the next success,
	// which is what makes the NEXT outage audible.
	dropping map[string]bool
}

// NewMarkTickPublisher binds a tick publisher to one venue.
//
// onDropped may be nil. A nil logger falls back to slog.Default() rather than
// panicking a background poll goroutine — losing the process is a worse outcome
// than losing the handler the composition root configured.
func NewMarkTickPublisher(pub Publisher, logger *slog.Logger, mic string, onDropped func(mic, instrumentID string)) *MarkTickPublisher {
	if logger == nil {
		logger = slog.Default()
	}
	return &MarkTickPublisher{
		pub:       pub,
		logger:    logger,
		mic:       mic,
		onDropped: onDropped,
		dropping:  map[string]bool{},
	}
}

// PublishTrade publishes one instrument's last price as a market.v1 Trade FACT.
//
// IT RETURNS NOTHING, AND THAT IS THE POINT. A returned error on a poll loop is
// what became a discarded assignment twice; no caller has a better answer to a
// dropped tick than this one, so there is nothing to hand back and nothing left
// to throw away.
func (p *MarkTickPublisher) PublishTrade(ctx context.Context, instrumentID, symbol string, price *commonpb.Decimal) {
	ev := &marketpb.MarketDataEvent{
		InstrumentId: instrumentID,
		Symbol:       symbol,
		Mic:          p.mic,
		EventTime:    timestamppb.Now(),
		Data:         &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{Price: price}},
	}
	err := p.pub.Publish(ctx, bus.Event{
		Subject:       SubjectMarketCryptoTrade,
		EventType:     SubjectMarketCryptoTrade,
		EventClass:    envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion: 1,
		Domain:        "market",
		EventTime:     time.Now().UTC(),
		PartitionKey:  instrumentID,
		Payload:       ev,
	})
	if err == nil {
		p.landed(instrumentID)
		return
	}
	p.noteDropped(instrumentID, symbol, err)
}

// noteDropped makes one lost tick countable, and once per outage audible.
func (p *MarkTickPublisher) noteDropped(instrumentID, symbol string, err error) {
	if p.onDropped != nil {
		p.onDropped(p.mic, instrumentID)
	}
	if !p.firstOfEpisode(instrumentID) {
		return
	}
	p.logger.Warn("mark tick DROPPED: the exchange answered but the price did not reach the bus — "+
		"this instrument's mark is now ageing out at every consumer that folds it",
		"mic", p.mic,
		"instrument_id", instrumentID,
		"symbol", symbol,
		"subject", SubjectMarketCryptoTrade,
		"err", err,
		"fix", "read the error. A REFUSED envelope (validation, tenant_id, schema) is a "+
			"configuration fault that needs a change deployed — retrying it forever changes "+
			"nothing. A transport error is the broker, or this pod's connection to it. Nothing "+
			"here retries: the next poll is the retry, and a second WARN for this instrument "+
			"means the outage ended and started again.")
}

// firstOfEpisode reports whether instrumentID has not been warned about since it
// last published successfully, and records that it now has.
func (p *MarkTickPublisher) firstOfEpisode(instrumentID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dropping[instrumentID] {
		return false
	}
	p.dropping[instrumentID] = true
	return true
}

// landed clears the WARN latch for an instrument whose tick reached the bus. It
// is deliberately silent: a recovery line per instrument would double the log
// volume of a flapping feed, and a counter that stops climbing is the recovery
// signal.
func (p *MarkTickPublisher) landed(instrumentID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.dropping, instrumentID)
}
