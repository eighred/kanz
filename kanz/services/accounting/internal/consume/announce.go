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
	"github.com/eighred/kanz/internal/outbox"
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
// CORPORATE ACTIONS ARE NOT IN THIS BALANCE (#588). The sentence above is the
// AUTHORITY ruling and it still stands — but one of the three inputs it names has
// never arrived. ledger.foldCorpAct pays dividend and coupon cash and any merger
// consideration onto the held quantity, and NOTHING ANNOUNCES A CORPORATE ACTION
// anywhere in this platform: accounting.v1.CorporateAction has no publisher, no
// NATS subject and no Kafka topic. So the level published here is complete for
// trade legs and for cash movements, and SILENTLY OMITS every dividend, coupon
// and merger payment the fund has received. Read
// kanz_accounting_entry_source_wired{type="corporate_action"} before treating a
// balance as the whole of what a portfolio holds.
//
// THE FIX IS NOT TO COMPUTE A SECOND BALANCE somewhere that does see corporate
// actions — nothing does, and a second computation is the drift this ruling
// exists to prevent. What is missing is upstream of every consumer.
//
// SO THE ANNOUNCEMENT SAYS SO (#614). Every level published here carries an
// EntrySourcePosture: which kinds of journal entry this deployment actually
// feeds, and which it cannot. A consumer can then tell a whole number from a
// partial one instead of inferring completeness from silence, and the pre-trade
// gate's refusal can name the missing feed rather than reporting a spending
// limit that was never reached. It corrects nothing and admits nothing — the
// sign of the omission is not even knowable (foldCorpAct pays quantity x
// per-unit, SIGNED, so an unfolded dividend understates a long book's cash and
// overstates a short book's).
//
// A LEVEL, NOT A DELTA: idempotent on redelivery, and self-correcting after a
// gap. The next announcement is right regardless of how many were missed, which
// is what makes a lost publish survivable.
type Announcer struct {
	store     ledger.Store
	publisher Publisher
	baseCcy   string
	posture   EntrySourcePosture
	logger    *slog.Logger
	now       func() time.Time
}

// EntrySourcePosture is what the DEPLOYMENT feeds, stated on every announcement
// (#614): the ledger entry-type names something in this process produces, and
// the ones it can fold and nothing produces.
//
// IT DESCRIBES THE ESTATE AND NOT THE PORTFOLIO, so it is identical on every
// portfolio's announcement. It rides along anyway because the consumer that
// needs it is a pre-trade control reading a local view on the order-admission
// path (#450 forbids it a synchronous call), and a posture fetched separately is
// a second thing that can be stale, absent, or describe a different accounting
// deployment than the balance came from.
//
// BOTH LISTS, so a statement that says nothing is detectable as such: an
// EntrySourcePosture with both empty is not "everything is fed", it is a caller
// that did not state a posture, and it is published as no statement at all
// rather than as a clean bill of health.
type EntrySourcePosture struct {
	Produced   []string
	Unproduced []string
}

// stated reports whether this posture says anything at all.
func (p EntrySourcePosture) stated() bool { return len(p.Produced) > 0 || len(p.Unproduced) > 0 }

// toProto renders the posture onto the wire, or nil when it says nothing.
func (p EntrySourcePosture) toProto() *accountingpb.BalanceCompleteness {
	if !p.stated() {
		return nil
	}
	// Sorted, for the same reason byVenueAccount is: the announcement must be
	// byte-stable for a given book, or every republish looks like a change to
	// anything diffing them.
	produced := append([]string(nil), p.Produced...)
	unproduced := append([]string(nil), p.Unproduced...)
	sort.Strings(produced)
	sort.Strings(unproduced)
	return &accountingpb.BalanceCompleteness{
		ProducedEntryTypes:   produced,
		UnproducedEntryTypes: unproduced,
	}
}

// Publisher is the bus publish surface — satisfied by *bus.Producer.
type Publisher interface {
	Publish(ctx context.Context, e bus.Event) error
}

// NewAnnouncer wires an Announcer. A nil publisher disables announcing, which the
// Folder reports at startup rather than discovering silently (#418's lesson: an
// unwired seam and a healthy one must not look the same).
func NewAnnouncer(store ledger.Store, publisher Publisher, baseCcy string, posture EntrySourcePosture, logger *slog.Logger, now func() time.Time) *Announcer {
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	if baseCcy == "" {
		baseCcy = "USD"
	}
	// AN UNSTATED POSTURE IS A DEFECT, NOT A DEFAULT (#614), so it is said out
	// loud here rather than discovered by a downstream gate refusing an order it
	// cannot explain. There is no safe fallback to substitute: this package cannot
	// see the deployment's config, and inventing "everything is fed" would publish
	// exactly the false assurance the field exists to abolish.
	if publisher != nil && !posture.stated() {
		logger.Error("accounting: announcing cash balances WITHOUT stating what they contain — every " +
			"consumer will read these levels as UNSTATED completeness, and the pre-trade buying-power " +
			"gate cannot tell a spending limit from a missing feed. Pass an EntrySourcePosture built " +
			"from this deployment's entry sources (#614)")
	}
	return &Announcer{store: store, publisher: publisher, baseCcy: baseCcy, posture: posture, logger: logger, now: now}
}

// Records renders portfolioID's cash level as the outbox records that must
// commit with the entry that changed it (#804).
//
// # What this replaces, and what was right about it
//
// It used to be Announce: a publish that ran AFTER the ledger write committed,
// whose error the Folder deliberately discarded. That trade-off was correct and
// is preserved — the ledger is the book of record, this announcement is DERIVED,
// and nacking the fold to retry a publish would turn a broker blip into a
// stalled ledger. A consumer's staleness bound made a lost announcement safe,
// because it ages the balance out to UNKNOWN and the buying-power rule fails
// closed on that.
//
// WHAT WAS MISSING WAS THE RECOVERY. The only thing that re-announced a
// portfolio was the next fold FOR THAT PORTFOLIO — there was no ticker, no
// compensator and no outbox. So one broker blip during one portfolio's fold
// refused every order for it under a buying-power mandate, indefinitely, until
// unrelated activity happened to arrive: a trading outage measured in hours,
// produced by a transient the platform recovered from in seconds. Fail-closed
// was the right direction and the wrong duration.
//
// Now the record commits with the entry and the relay retries the publish on its
// own schedule. The failure it cannot survive moved from "the broker was down"
// to "the database was down", and the database being down already fails the
// fold.
//
// IT READS THROUGH THE STORE IT IS HANDED, not through a.store, and that is the
// whole reason it is a callback. Inside Append's transaction that store SEES THE
// UNCOMMITTED ENTRY, so the level is the one the fold produced. Reading through
// the pool from in there would announce the balance as it was BEFORE the fold,
// every time.
func (a *Announcer) Records(ctx context.Context, st ledger.Store, portfolioID string) ([]outbox.Record, error) {
	if a.publisher == nil || portfolioID == "" {
		return nil, nil
	}
	book, _, err := ledger.MaterializeCurrent(ctx, st, portfolioID)
	if err != nil {
		return nil, fmt.Errorf("announce %s: materialize: %w", portfolioID, err)
	}
	total, ok := dec.ToProtoScaled(book.CashBalance(a.baseCcy))
	if !ok {
		// SCALED, NOT WRAPPING (#94). A balance the platform cannot represent
		// exactly must not be announced as a smaller one: a consumer would compare
		// a wrapped figure against a spending limit and admit an order the fund
		// cannot pay for.
		//
		// IT NOW FAILS THE APPEND, and that is the correct direction rather than a
		// regression. An entry whose resulting balance cannot be stated is one the
		// estate would be told nothing about; refusing it leaves the journal
		// consistent and the fill in the DLQ, where an operator sees it — instead
		// of a committed entry whose level silently never goes out.
		return nil, fmt.Errorf("announce %s: cash balance is not representable as a Decimal", portfolioID)
	}

	perAccount, err := a.byVenueAccount(ctx, st, portfolioID)
	if err != nil {
		return nil, err
	}

	now := a.now().UTC()
	msg := &accountingpb.PortfolioCashBalance{
		PortfolioId:    portfolioID,
		BaseCurrency:   a.baseCcy,
		Total:          total,
		ByVenueAccount: perAccount,
		AsOf:           timestamppb.New(now),
		KnowledgeTime:  timestamppb.New(now),
		Completeness:   a.posture.toProto(),
	}
	// outbox.From resolves the tenant and the lineage off ctx exactly as
	// bus.Producer.stamp would have — the relay publishes from a ticker, long
	// after the fold's context is gone, so a record that did not capture them
	// would publish fine and quietly stop being traceable to the fill that caused
	// it. It REFUSES an empty tenant, and that refusal rolls the entry back with
	// it: a balance the platform cannot announce is one it must not book.
	//
	// THE PARTITION KEY IS THE PORTFOLIO, which is the granularity a consumer
	// folds at (cashview keys its map by portfolio_id) and the granularity
	// Append's advisory lock serializes. Same key implies same lock implies
	// commits that cannot interleave, which is what makes the relay's id order
	// commit order for this FACT.
	rec, err := outbox.From(ctx, bus.Event{
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
	if err != nil {
		return nil, fmt.Errorf("announce %s: capture: %w", portfolioID, err)
	}
	return []outbox.Record{rec}, nil
}

// byVenueAccount folds the journal into per-(exchange account, asset) balances —
// the half execution.ExpectedBalances needs (#418), carried on the same FACT so
// one publisher serves both consumers rather than two drifting apart.
//
// Sorted, so the announcement is byte-stable for a given book: an unsorted map
// range would make every republish look like a change to anything diffing them.
func (a *Announcer) byVenueAccount(ctx context.Context, st ledger.Store, portfolioID string) ([]*accountingpb.VenueAccountCash, error) {
	events, err := st.Journal(ctx, portfolioID)
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
