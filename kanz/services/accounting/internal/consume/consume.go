// Package consume folds live OMS-01 fill FACTs into the durable IBOR journal
// (PARITY-02c) — the seam IBOR-01b carried forward. It is the accounting
// counterpart of the OMS position projector: the same order.v1.Fill FACTs, the
// same weighted-average-cost accounting (ledger.FromFill → ledger.foldPosition),
// folded into the book-of-record journal instead of the execution book, so the
// IBOR and the OMS book agree by construction.
//
// Idempotency is two-layered: the bus consumer dedups on the envelope key, and
// ledger.Store.Append is idempotent on the entry id ("fill:"+fill_id), so a
// redelivered fill is a no-op even across a consumer restart.
package consume

import (
	"context"
	"errors"
	"fmt"
	"github.com/eighred/kanz/internal/fillfact"
	"log/slog"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/outbox"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// Order fill FACT event types (mirror order.EventType* without importing the OMS
// internal package — the Go internal-package rule forbids it across the service
// boundary, the same stance ledger.FromFill and the position projector take).
const (
	orderEventFilled          = fillfact.SubjectFilled
	orderEventPartiallyFilled = fillfact.SubjectPartiallyFilled
)

// Cash-movement FACT event types (WIRE-01f): the accounting.v1.LedgerEntry
// FACTs the cashmove.Publisher emits for non-trade cash legs. The Folder folds
// them into the same journal the fills land in, so subscriptions/redemptions/
// fees reach NAV.
const (
	cashEventSubscription = "accounting.cash.subscription"
	cashEventRedemption   = "accounting.cash.redemption"
	cashEventFee          = "accounting.cash.fee"
)

// Folder is the bus.EventHandler that folds fill FACTs into the ledger journal.
// Construct once and pass Handle to bus.Consumer.Subscribe for each fill subject.
type Folder struct {
	// tenant is the tenant this folder serves; Handle refuses any other (#223).
	tenant       string
	store        ledger.Store
	cashCurrency string
	// announcer publishes the portfolio's new cash level after a fold that
	// changed it (#450). nil ⇒ nothing announces, which the composition root
	// reports at startup rather than leaving to be discovered.
	announcer *Announcer
	// relay drains the outbox this folder's store enqueues into. Built by
	// NewFolder from the store's own queue, so a Folder always has a drain for
	// the announcements its Appends commit (#804) — the same construction
	// order.NewService makes, and for the same reason.
	relay  *outbox.Relay
	logger *slog.Logger
	// onAnnounceFailed counts announcements that never left this process. See
	// WithAnnounceFailureObserver for why a log line alone was not enough.
	onAnnounceFailed func()
	// relayOpts carry the relay's interval and counters from the composition
	// root; applied before NewFolder builds it.
	relayOpts []outbox.RelayOption
}

// WithAnnouncer makes the folder publish a portfolio's cash level after each
// successful fold (#450). Without it the ledger is still correct and nothing
// downstream can see a balance — which is the state the pre-trade buying-power
// gate fails closed on.
func WithAnnouncer(a *Announcer) FolderOption { return func(f *Folder) { f.announcer = a } }

// WithLogger sets the folder's logger; nil ⇒ slog.Default().
func WithLogger(l *slog.Logger) FolderOption {
	return func(f *Folder) {
		if l != nil {
			f.logger = l
		}
	}
}

// FolderOption customizes a Folder.
type FolderOption func(*Folder)

// WithAnnounceFailureObserver counts cash-balance announcements that never left
// this process.
//
// THE SWALLOW IS ARGUED AND THE ARGUMENT HOLDS (see announce): the ledger write
// is the book of record and nacking would turn a broker blip into a stalled
// fold. That comment ends "It is LOUD instead" — and until now loud meant a log
// line, so the claim rested on somebody scraping logs (#622).
//
// It matters because the downstream fallback is a REFUSAL: a consumer's
// staleness bound ages the balance out to UNKNOWN and the buying-power rule
// fails closed on it. So the visible symptom of lost announcements is orders
// being denied, and an operator should not have to work backwards from that.
//
// Nil ⇒ not counted; the failure is still logged.
func WithAnnounceFailureObserver(fn func()) FolderOption {
	return func(f *Folder) { f.onAnnounceFailed = fn }
}

// announcerFor returns the callback Append runs INSIDE its transaction to build
// this portfolio's cash announcement (#804), or nil when nothing announces.
//
// The level and the entry that produced it now commit together, so the record
// cannot be lost to a broker that was down for a moment — which is what it used
// to be, with no recovery but the next fold for the same portfolio.
func (f *Folder) announcerFor(portfolioID string) ledger.Announcer {
	if f.announcer == nil || portfolioID == "" {
		return nil
	}
	return func(ctx context.Context, st ledger.Store) ([]outbox.Record, error) {
		return f.announcer.Records(ctx, st, portfolioID)
	}
}

// flush publishes what the fold just committed, and NEVER fails the fold.
//
// THE TRADE-OFF THE OLD announce MADE IS UNCHANGED, and it is still the right
// one: the ledger write has committed and is the book of record, this
// announcement is DERIVED, and returning the error to the bus would nack the
// message and re-fold it — turning a broker blip into a stalled ledger, when the
// fold is the one thing that must not stop.
//
// WHAT CHANGED IS WHAT A FAILURE COSTS. It used to be permanent until unrelated
// activity arrived; the record is now durable, so a failure here is LATENCY and
// the relay's own pass sends it. The counter and the ERROR log stay, because an
// operator should learn that announcements are lagging from the thing that
// measures them rather than from orders being refused — but they now describe a
// delay rather than a loss, and the log says so.
func (f *Folder) flush(ctx context.Context, portfolioID string) {
	if f.relay == nil || portfolioID == "" {
		return
	}
	if _, err := f.relay.Flush(ctx, portfolioID); err != nil {
		if f.onAnnounceFailed != nil {
			f.onAnnounceFailed()
		}
		f.logger.ErrorContext(ctx, "accounting: cash balance announcement did not publish — it is "+
			"DURABLE and the outbox relay will retry it, so this is a delay rather than a loss. "+
			"Downstream consumers age this portfolio's balance out to UNKNOWN meanwhile, which "+
			"refuses orders under a buying-power mandate",
			"portfolio_id", portfolioID, "err", err)
	}
}

// NewFolder wires a Folder to a durable ledger.Store. cashCurrency stamps the
// cash leg a fill produces (the portfolio reporting currency until a
// per-instrument reference-data join lands — the FromFill carried-forward seam);
// empty defaults to "USD".
//
// It is also the currency a fill's FEE must be denominated in: FromFill refuses
// any other, so a venue that charges in the base asset DLQs rather than posting a
// fee against a currency it was not charged in (#221).
func NewFolder(tenant string, store ledger.Store, cashCurrency string, opts ...FolderOption) (*Folder, error) {
	if store == nil {
		return nil, errors.New("consume: ledger store is nil")
	}
	if cashCurrency == "" {
		cashCurrency = "USD"
	}
	f := &Folder{tenant: tenant, store: store, cashCurrency: cashCurrency, logger: slog.Default()}
	for _, opt := range opts {
		opt(f)
	}
	// THE RELAY IS BUILT LAST, from this store's own outbox and this announcer's
	// own publisher, so a Folder always has a drain for the announcements its
	// Appends commit (#804, #292). Options are applied first because they carry
	// the announcer the publisher comes from.
	//
	// No announcer means nothing to publish and nothing to drain — the deployment
	// the composition root already reports at startup.
	if f.announcer != nil && f.announcer.publisher != nil {
		q, ok := store.(interface{ Outbox() outbox.Queue })
		if !ok {
			return nil, errors.New("consume: the ledger store has no outbox, so a cash announcement " +
				"would be committed and never published")
		}
		relay, err := outbox.NewRelay(q.Outbox(), f.announcer.publisher, f.logger, f.relayOpts...)
		if err != nil {
			return nil, err
		}
		f.relay = relay
	}
	return f, nil
}

// Outbox is the relay draining the announcements this folder's store commits.
// The composition root must Run it, or every cash level waits for the next fold
// to flush it inline — which is the recovery gap #804 closed.
func (f *Folder) Outbox() *outbox.Relay { return f.relay }

// WithOutboxRelay passes options through to the relay NewFolder constructs.
func WithOutboxRelay(opts ...outbox.RelayOption) FolderOption {
	return func(f *Folder) { f.relayOpts = append(f.relayOpts, opts...) }
}

// Handle is the bus.EventHandler value wired into bus.Consumer.Subscribe. It
// decodes a fill-bearing FACT, builds the TRADE journal entry, and appends it.
// A non-nil return nacks/DLQs the delivery — a malformed or unappendable fill
// surfaces to the operator rather than being silently dropped (book-of-record
// data loss must be loud).
func (f *Folder) Handle(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	// This folder writes through an RLS pool pinned to f.tenant, so an envelope
	// from another tenant would be folded into this tenant's book (#223).
	if err := bus.RequireTenantScope(env.GetTenantId(), f.tenant); err != nil {
		return err
	}
	fill, portfolioID, err := decodeFill(env.GetEventType(), payload)
	if err != nil {
		return fmt.Errorf("consume: %s decode: %w", env.GetEventType(), err)
	}
	if fill == nil {
		return nil // not a fill-bearing event; ack
	}
	entry, err := ledger.FromFill(portfolioID, fill, f.cashCurrency, knowledgeTime(env))
	if err != nil {
		// A fee the cash leg cannot represent (a BTC fee against a USD book, #221)
		// DLQs the fill. The journal is append-only, so a wrong entry is permanent
		// and a held message is not: the operator re-drives it once the book can
		// carry the fee's own asset.
		return fmt.Errorf("consume: %s: %w", env.GetEventType(), err)
	}
	if err := f.store.Append(ctx, entry, f.announcerFor(portfolioID)); err != nil {
		return err
	}
	f.flush(ctx, portfolioID)
	return nil
}

// HandleCash is the bus.EventHandler for the cash-movement FACT subjects
// (WIRE-01f). It decodes an accounting.v1.LedgerEntry cash/fee FACT into a
// ledger cash Event and appends it — the non-trade counterpart of Handle. A
// non-cash event type is acked (not this handler's concern); a malformed or
// unappendable cash FACT is returned (nack/DLQ) so cash-of-record loss is loud.
// Idempotency rides the entry id (the producer's "cash:"+MovementID), so a
// redelivery is a no-op via Store.Append.
func (f *Folder) HandleCash(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	// This folder writes through an RLS pool pinned to f.tenant, so an envelope
	// from another tenant would be folded into this tenant's book (#223).
	if err := bus.RequireTenantScope(env.GetTenantId(), f.tenant); err != nil {
		return err
	}
	entry, err := decodeCash(env.GetEventType(), payload, knowledgeTime(env))
	if err != nil {
		return fmt.Errorf("consume: %s decode: %w", env.GetEventType(), err)
	}
	if entry == nil {
		return nil // not a cash-movement event; ack
	}
	if err := f.store.Append(ctx, entry, f.announcerFor(entry.PortfolioID)); err != nil {
		return err
	}
	f.flush(ctx, entry.PortfolioID)
	return nil
}

// decodeCash maps a cash-movement FACT to a ledger cash Event. It returns a nil
// event (no error) for a non-cash event type. The knowledge time falls back to
// the envelope's when the entry omits it.
func decodeCash(eventType string, payload []byte, envKnowledge time.Time) (*ledger.Event, error) {
	var entryType ledger.EntryType
	switch eventType {
	case cashEventSubscription, cashEventRedemption:
		entryType = ledger.EntryCash
	case cashEventFee:
		entryType = ledger.EntryFee
	default:
		return nil, nil
	}
	var le accountingpb.LedgerEntry
	if err := proto.Unmarshal(payload, &le); err != nil {
		return nil, err
	}
	// DOMAIN-CHECK THE WHOLE ENTRY BEFORE READING ANY NUMBER OFF IT (#95).
	//
	// Decimal.exponent is an unvalidated wire field and dec.FromProto materialises
	// 10^abs(exponent). An entry carrying {1, 2000000000} does not post a wrong
	// balance — it never returns, and the consumer grinding it stops acking, so
	// the cash subscription stalls behind that one message. Refusing here DLQs it
	// and leaves the ledger consuming.
	if field, in := dec.InDomainDeep(&le); !in {
		return nil, fmt.Errorf("cash entry %q carries an out-of-domain exponent at %s", le.GetEntryId(), field)
	}
	if le.GetEntryId() == "" || le.GetPortfolioId() == "" {
		return nil, fmt.Errorf("cash entry missing id or portfolio")
	}
	if le.GetCash() == nil || le.GetCashCurrency() == "" {
		return nil, fmt.Errorf("cash entry %q missing cash leg or currency", le.GetEntryId())
	}
	knowledge := le.GetKnowledgeTime().AsTime()
	if knowledge.IsZero() {
		knowledge = envKnowledge
	}
	effective := le.GetEffectiveTime().AsTime()
	if effective.IsZero() {
		effective = knowledge
	}
	return &ledger.Event{
		EntryID:     le.GetEntryId(),
		PortfolioID: le.GetPortfolioId(),
		// CARRIED, NOT DROPPED (#415). This decoder unmarshals the whole
		// LedgerEntry and used to omit venue_account_id while its sibling decodeFill
		// set it. Dropping it does not lose detail quietly: migration 0003 reads ''
		// as the positive claim "this entry touched no exchange account", so every
		// funded cash movement landed asserting something false in the append-only
		// book of record.
		VenueAccountID: le.GetVenueAccountId(),
		// NO SETTLEMENT BASIS IS ASSERTED HERE, AND THAT IS THE HONEST ANSWER
		// (#1043). accounting.v1.LedgerEntry carries no settlement field, so this
		// decoder genuinely cannot know whether a subscription, redemption or fee
		// has actually moved between the accounts — only that it was announced.
		// Leaving SettlementBasis at ledger.SettlementUnknown keeps the movement in
		// the traded book, out of the settled one, and counted as the gap it is;
		// Book.SettlementBasisComplete then refuses a settled-basis read rather than
		// returning a balance short by every unsettled movement. Stamping
		// SettlementSettled here would be cheaper and would be a fabrication — the
		// same #345 ground on which invented corporate actions are refused. WHAT
		// WOULD ARM IT is a settlement field on accounting.v1.LedgerEntry carrying
		// the producer's own assertion, which is #589's confirmation plane.
		Type:         entryType,
		Cash:         dec.FromProto(le.GetCash()),
		CashCurrency: le.GetCashCurrency(),
		Effective:    effective,
		Knowledge:    knowledge,
		SourceRef:    le.GetSourceRef(),
	}, nil
}

// knowledgeTime is when the book learned of the fill: the envelope ingestion
// time (when Kanz received it), falling back to publish time, then now — the
// bitemporal knowledge axis the ledger restatement read depends on.
func knowledgeTime(env *envelopepb.Envelope) time.Time {
	if t := env.GetIngestionTime().AsTime(); !t.IsZero() {
		return t
	}
	if t := env.GetPublishTime().AsTime(); !t.IsZero() {
		return t
	}
	return time.Now().UTC()
}

// decodeFill extracts the Fill and its portfolio from a fill-bearing FACT
// (mirrors the OMS position projector's decodeFill).
func decodeFill(eventType string, payload []byte) (*orderpb.Fill, string, error) {
	switch eventType {
	case orderEventFilled:
		var ev orderpb.OrderFilled
		if err := proto.Unmarshal(payload, &ev); err != nil {
			return nil, "", err
		}
		if field, in := dec.InDomainDeep(&ev); !in {
			return nil, "", fmt.Errorf("fill carries an out-of-domain exponent at %s", field)
		}
		return ev.GetFill(), ev.GetState().GetPortfolioId(), nil
	case orderEventPartiallyFilled:
		var ev orderpb.OrderPartiallyFilled
		if err := proto.Unmarshal(payload, &ev); err != nil {
			return nil, "", err
		}
		if field, in := dec.InDomainDeep(&ev); !in {
			return nil, "", fmt.Errorf("fill carries an out-of-domain exponent at %s", field)
		}
		return ev.GetFill(), ev.GetState().GetPortfolioId(), nil
	default:
		return nil, "", nil
	}
}
