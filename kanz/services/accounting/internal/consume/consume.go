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
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

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
	orderEventFilled          = "order.order.filled"
	orderEventPartiallyFilled = "order.order.partially_filled"
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
}

// NewFolder wires a Folder to a durable ledger.Store. cashCurrency stamps the
// cash leg a fill produces (the portfolio reporting currency until a
// per-instrument reference-data join lands — the FromFill carried-forward seam);
// empty defaults to "USD".
//
// It is also the currency a fill's FEE must be denominated in: FromFill refuses
// any other, so a venue that charges in the base asset DLQs rather than posting a
// fee against a currency it was not charged in (#221).
func NewFolder(tenant string, store ledger.Store, cashCurrency string) (*Folder, error) {
	if store == nil {
		return nil, errors.New("consume: ledger store is nil")
	}
	if cashCurrency == "" {
		cashCurrency = "USD"
	}
	return &Folder{tenant: tenant, store: store, cashCurrency: cashCurrency}, nil
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
	return f.store.Append(ctx, entry)
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
	return f.store.Append(ctx, entry)
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
		Type:           entryType,
		Cash:           dec.FromProto(le.GetCash()),
		CashCurrency:   le.GetCashCurrency(),
		Effective:      effective,
		Knowledge:      knowledge,
		SourceRef:      le.GetSourceRef(),
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
