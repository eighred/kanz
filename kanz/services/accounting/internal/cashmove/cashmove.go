// Package cashmove is the cash-movement FACT producer (WIRE-01f): it emits
// subscription / redemption / fee cash movements as accounting.v1.LedgerEntry
// FACTs on the bus, which the WIRE-01b Folder folds into the IBOR journal. Fill
// folding was already live (order.v1 fills → EntryTrade); the cash side — the
// non-trade cash legs a fund book actually moves — had no producer, so a
// subscription or a management fee never reached NAV. This closes that gap.
//
// It reuses the OMS posttrade.BusFailSink pattern: a shared bus.Producer wrapped
// with a domain→FACT encoder, so producer_sequence stays monotonic across
// emitted movements. Idempotency rides the entry id ("cash:"+MovementID) — the
// Folder's Append is idempotent, so a redelivered movement is a no-op.
package cashmove

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/bus"
)

// Kind is the sort of cash movement, which fixes both the journal entry type and
// the cash sign: a subscription adds cash, a redemption and a fee remove it.
type Kind int

const (
	// Subscription is investor capital in: a positive CASH entry.
	Subscription Kind = iota
	// Redemption is investor capital out: a negative CASH entry.
	Redemption
	// Fee is a management/performance fee out: a negative FEE entry.
	Fee
)

// wire returns the event subject/type and the accounting.v1 entry type for a
// kind; ok=false for an unknown kind.
func (k Kind) wire() (subject string, entry accountingpb.EntryType, ok bool) {
	switch k {
	case Subscription:
		return "accounting.cash.subscription", accountingpb.EntryType_ENTRY_TYPE_CASH, true
	case Redemption:
		return "accounting.cash.redemption", accountingpb.EntryType_ENTRY_TYPE_CASH, true
	case Fee:
		return "accounting.cash.fee", accountingpb.EntryType_ENTRY_TYPE_FEE, true
	default:
		return "", accountingpb.EntryType_ENTRY_TYPE_UNSPECIFIED, false
	}
}

// signedCash applies the kind's sign to a movement's magnitude: subscriptions
// add cash, redemptions and fees remove it.
func (k Kind) signedCash(amount *big.Rat) *big.Rat {
	c := new(big.Rat).Abs(amount)
	if k == Redemption || k == Fee {
		c.Neg(c)
	}
	return c
}

// CashMovement is one non-trade cash event a fund book moves. Amount is the
// magnitude (its sign is derived from Kind); Effective is the domain time the
// movement takes economic effect.
type CashMovement struct {
	MovementID  string
	PortfolioID string
	Kind        Kind
	Amount      *big.Rat
	Currency    string
	Effective   time.Time
	SourceRef   string

	// VenueAccountID is the EXCHANGE ACCOUNT this movement settled against, or ""
	// for one that touched none (#415).
	//
	// THE LEDGER COULD SAY A PORTFOLIO HELD CASH AND NEVER WHERE. Every other
	// layer was already built for this: accounting.v1.LedgerEntry carries
	// venue_account_id (field 12), ledger.Event has the field, the FILL path sets
	// it (ledger/fill.go), ledger/postgres.go declares it to the transaction via
	// app.venue_account_id, and migration 0003 adds the column, a write-guard that
	// refuses an undeclared account, and an index whose comment says it exists for
	// "per-account cash and position folds — what is actually in okx-sub-1". Only
	// the cash producer could not fill it in, so the index for per-account CASH
	// had no cash to index.
	//
	// OPTIONAL, AND THE EMPTY VALUE MEANS SOMETHING. Migration 0003 treats '' as
	// the POSITIVE DECLARATION that an entry touches no exchange account, which is
	// the truth for an investor subscription into the fund's own bank. A transfer
	// that funds okx-sub-1 is the other case. Requiring one answer for both would
	// make one of them a lie, so this is not defaulted and never guessed.
	VenueAccountID string
}

const (
	domainAccounting  = "accounting"
	schemaRefLedger   = "accounting.v1.LedgerEntry:1"
	schemaVersionCash = 1
)

// Publisher emits CashMovements as accounting.v1.LedgerEntry FACTs over a shared
// bus.Producer. Safe for concurrent Publish (the producer is).
type Publisher struct {
	producer *bus.Producer
	now      func() time.Time
}

// NewPublisher builds a Publisher over producer. now is an injectable clock for
// the emitted knowledge/event time (deterministic in tests); nil ⇒ time.Now.
func NewPublisher(producer *bus.Producer, now func() time.Time) (*Publisher, error) {
	if producer == nil {
		return nil, errors.New("cashmove: nil producer")
	}
	if now == nil {
		now = time.Now
	}
	return &Publisher{producer: producer, now: now}, nil
}

// Publish emits m as a LedgerEntry FACT, partitioned by portfolio_id so a book's
// cash movements stay ordered. It validates the movement first — a book-of-record
// FACT with an empty id/portfolio, non-positive amount, or unknown kind is a
// producer error, never emitted.
func (p *Publisher) Publish(ctx context.Context, m CashMovement) error {
	entry, subject, err := encode(m, p.now())
	if err != nil {
		return err
	}
	return p.producer.Publish(ctx, bus.Event{
		Subject:          subject,
		EventType:        subject,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    schemaVersionCash,
		Domain:           domainAccounting,
		EventTime:        m.Effective,
		PartitionKey:     m.PortfolioID,
		PayloadSchemaRef: schemaRefLedger,
		Payload:          entry,
	})
}

// encode maps a CashMovement to its accounting.v1.LedgerEntry FACT and subject,
// validating the movement. knowledge is the time the book learns of it.
func encode(m CashMovement, knowledge time.Time) (*accountingpb.LedgerEntry, string, error) {
	subject, entryType, ok := m.Kind.wire()
	if !ok {
		return nil, "", fmt.Errorf("cashmove: unknown kind %d", m.Kind)
	}
	if m.MovementID == "" || m.PortfolioID == "" {
		return nil, "", errors.New("cashmove: movement id and portfolio id are required")
	}
	if m.Amount == nil || m.Amount.Sign() <= 0 {
		return nil, "", errors.New("cashmove: amount must be positive")
	}
	if m.Currency == "" {
		return nil, "", errors.New("cashmove: currency is required")
	}
	eff := m.Effective
	if eff.IsZero() {
		eff = knowledge
	}
	// SCALED, NOT WRAPPING (#94). This is the cash leg posted to the BOOK OF
	// RECORD. dec.ToProto wraps above ~$92bn at scale 8 and the ledger would then
	// carry, and report, a figure the platform invented — a subscription or a
	// redemption for an amount nobody moved. A fund large enough to reach that
	// threshold is exactly the fund whose NAV nobody can afford to be wrong.
	cash, ok := dec.ToProtoScaled(m.Kind.signedCash(m.Amount))
	if !ok {
		return nil, "", fmt.Errorf("cashmove: movement %q amount is not representable as a Decimal "+
			"— refusing to post a cash entry the ledger cannot hold exactly", m.MovementID)
	}
	entry := &accountingpb.LedgerEntry{
		EntryId:     "cash:" + m.MovementID,
		PortfolioId: m.PortfolioID,
		// Passed through, never defaulted: "" is the positive declaration that this
		// movement touched no exchange account (migration 0003), and guessing an
		// account would post cash against collateral it never reached.
		VenueAccountId: m.VenueAccountID,
		EntryType:      entryType,
		Cash:           cash,
		CashCurrency:   m.Currency,
		EffectiveTime:  timestamp(eff),
		KnowledgeTime:  timestamp(knowledge),
		SourceRef:      m.SourceRef,
	}
	return entry, subject, nil
}

// timestamp converts a time to a proto Timestamp; a zero time yields nil.
func timestamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t.UTC())
}
