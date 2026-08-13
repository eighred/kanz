package consume

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"sort"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// SubjectPortfolioCash is where the book of record announces what a portfolio can
// spend (#450). {domain}.{entity}.{event_type}, so it archives to the
// accounting.balance Kafka topic that already exists.
const SubjectPortfolioCash = "accounting.balance.portfolio"

const schemaRefPortfolioCash = "accounting.v1.PortfolioCashBalance:1"

// Announcer publishes a portfolio's cash level after the ledger folds an entry
// that changed it (#450).
//
// ACCOUNTING ANNOUNCES AND NOBODY ELSE COMPUTES. The ledger is the only component
// that folds trade legs, cash movements and corporate actions bitemporally with
// restatements. A consumer that re-derived a balance would be a second
// implementation of one number, and the two would drift — surfacing as a trading
// control that refuses or admits wrongly.
//
// A LEVEL, NOT A DELTA: idempotent on redelivery, and self-correcting after a
// gap. The next announcement is right regardless of how many were missed, which
// is what makes a lost publish survivable.
type Announcer struct {
	store     ledger.Store
	publisher Publisher
	baseCcy   string
	logger    *slog.Logger
	now       func() time.Time
}

// Publisher is the bus publish surface — satisfied by *bus.Producer.
type Publisher interface {
	Publish(ctx context.Context, e bus.Event) error
}

// NewAnnouncer wires an Announcer. A nil publisher disables announcing, which the
// Folder reports at startup rather than discovering silently (#418's lesson: an
// unwired seam and a healthy one must not look the same).
func NewAnnouncer(store ledger.Store, publisher Publisher, baseCcy string, logger *slog.Logger, now func() time.Time) *Announcer {
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	if baseCcy == "" {
		baseCcy = "USD"
	}
	return &Announcer{store: store, publisher: publisher, baseCcy: baseCcy, logger: logger, now: now}
}

// Announce recomputes portfolioID's cash from the journal and publishes it.
//
// THE ERROR IS RETURNED BUT THE CALLER MUST NOT NACK ON IT. The ledger write is
// the book of record and has already committed; this announcement is DERIVED. A
// failed publish that failed the fold would turn a broker blip into a stalled
// ledger, and the fold is what must never stop. The consumer's staleness bound
// is what makes a lost announcement safe: it falls back to "cash unavailable",
// which the buying-power rule fails closed on.
func (a *Announcer) Announce(ctx context.Context, portfolioID string) error {
	if a.publisher == nil {
		return nil
	}
	book, _, err := ledger.MaterializeCurrent(ctx, a.store, portfolioID)
	if err != nil {
		return fmt.Errorf("announce %s: materialize: %w", portfolioID, err)
	}
	total, ok := dec.ToProtoScaled(book.CashBalance(a.baseCcy))
	if !ok {
		// SCALED, NOT WRAPPING (#94). A balance the platform cannot represent
		// exactly must not be announced as a smaller one: a consumer would compare
		// a wrapped figure against a spending limit and admit an order the fund
		// cannot pay for.
		return fmt.Errorf("announce %s: cash balance is not representable as a Decimal", portfolioID)
	}

	perAccount, err := a.byVenueAccount(ctx, portfolioID)
	if err != nil {
		return err
	}

	now := a.now().UTC()
	msg := &accountingpb.PortfolioCashBalance{
		PortfolioId:    portfolioID,
		BaseCurrency:   a.baseCcy,
		Total:          total,
		ByVenueAccount: perAccount,
		AsOf:           timestamppb.New(now),
		KnowledgeTime:  timestamppb.New(now),
	}
	return a.publisher.Publish(ctx, bus.Event{
		Subject:          SubjectPortfolioCash,
		EventType:        SubjectPortfolioCash,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    1,
		Domain:           "accounting",
		EventTime:        now,
		PartitionKey:     portfolioID,
		PayloadSchemaRef: schemaRefPortfolioCash,
		Payload:          msg,
	})
}

// byVenueAccount folds the journal into per-(exchange account, asset) balances —
// the half execution.ExpectedBalances needs (#418), carried on the same FACT so
// one publisher serves both consumers rather than two drifting apart.
//
// Sorted, so the announcement is byte-stable for a given book: an unsorted map
// range would make every republish look like a change to anything diffing them.
func (a *Announcer) byVenueAccount(ctx context.Context, portfolioID string) ([]*accountingpb.VenueAccountCash, error) {
	events, err := a.store.Journal(ctx, portfolioID)
	if err != nil {
		return nil, fmt.Errorf("announce %s: journal: %w", portfolioID, err)
	}
	balances := ledger.VenueAccountCash(events)

	var out []*accountingpb.VenueAccountCash
	for _, account := range ledger.VenueAccounts(balances) {
		byAsset := balances[account]
		assets := make([]string, 0, len(byAsset))
		for asset := range byAsset {
			assets = append(assets, asset)
		}
		sort.Strings(assets)
		for _, asset := range assets {
			amount, ok := dec.ToProtoScaled(orZero(byAsset[asset]))
			if !ok {
				return nil, fmt.Errorf("announce %s: %s balance in %s is not representable as a Decimal",
					portfolioID, asset, account)
			}
			out = append(out, &accountingpb.VenueAccountCash{
				VenueAccountId: account,
				Asset:          asset,
				Amount:         amount,
			})
		}
	}
	return out, nil
}

func orZero(r *big.Rat) *big.Rat {
	if r == nil {
		return new(big.Rat)
	}
	return r
}
