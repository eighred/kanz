package balancerecon

import (
	"context"
	"math/big"
	"sync"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
)

// Subject is where the book of record announces portfolio cash, including the
// per-exchange-account breakdown this view reads (#450).
//
// DECLARED HERE AND IN accounting's consume.SubjectPortfolioCash and the OMS's
// cashview.Subject, because Go's internal-package rule forbids one service
// importing another's internals. test/arch/cash_subject_agreement_test.go fails
// the build if they disagree — a typo would otherwise leave this adapter
// subscribed to a subject nobody publishes, and the only symptom would be
// reconciliation quietly never running, which is #418 exactly.
const Subject = "accounting.balance.portfolio"

// DefaultMaxAge bounds how old an announcement may be and still be treated as
// this account's balance.
//
// A BALANCE THAT CANNOT BE SHOWN TO BE CURRENT IS NOT A BALANCE. Announcements
// are derived state and can be lost — accounting publishes them after the ledger
// write they describe, and a publish failure must never fail that write. Serving
// the last one forever would compare the exchange against a figure from before
// whatever moved it, and report a break that is really staleness.
//
// Aging out yields UNKNOWN, which skips the asset rather than comparing it
// against zero. Same shape as the OMS's cashview bound and #416's signal
// freshness: an input that cannot be shown current is refused, not trusted.
const DefaultMaxAge = 15 * time.Minute

// View is one venue account's expected balances, folded from the book of
// record's announcements (#450). It satisfies execution.ExpectedBalances.
//
// THE ADAPTER MUST NOT COMPUTE A BALANCE. accounting's ledger is the only fold
// that takes trade legs, cash movements and corporate actions bitemporally with
// restatements; a second computation here would drift, and reconciliation would
// then compare two of Kanz's own numbers against the exchange and report
// whichever disagreed. This only remembers a level somebody else computed.
//
// CORPORATE ACTIONS ARE NOT IN THIS BALANCE (#588), and this is the consumer
// where that matters most, because the exchange DOES see them. Accounting's fold
// pays dividend, coupon and merger cash correctly and has never been given one:
// accounting.v1.CorporateAction has no publisher anywhere in the module. So when
// #418's comparison is finally armed, an account that has been paid a dividend
// will hold cash the exchange knows about and the book does not, and this will
// report a BREAK THAT IS NOT A BREAK — an exchange discrepancy in the shape of an
// unfed feed. The standing instinct on a break is that the exchange is the
// authority and the book must be adjusted; here that would be right by accident
// and wrong as a rule, because the adjustment would have no announcement behind
// it and no ex-date to restate against.
//
// Check kanz_accounting_entry_source_wired{type="corporate_action"} on the
// accounting deployment before treating any cash break as a fill problem.
//
// It is scoped to ONE account — the one this adapter's credential spends from.
// An announcement carries every account a portfolio holds cash in, and taking
// the wrong one would reconcile this exchange against another exchange's
// collateral.
type View struct {
	account string
	maxAge  time.Duration
	now     func() time.Time

	mu     sync.RWMutex
	assets map[string]*big.Rat
	asOf   time.Time
	seen   bool

	onStale func(age time.Duration)
}

var _ execution.ExpectedBalances = (*View)(nil)

// ViewOption customizes a View.
type ViewOption func(*View)

// WithViewMaxAge overrides DefaultMaxAge. Non-positive ⇒ no bound, which is only
// right in a test.
func WithViewMaxAge(d time.Duration) ViewOption { return func(v *View) { v.maxAge = d } }

// WithViewClock injects the clock (tests).
func WithViewClock(now func() time.Time) ViewOption {
	return func(v *View) {
		if now != nil {
			v.now = now
		}
	}
}

// WithViewOnStale is called when a lookup finds the balance too old to use.
func WithViewOnStale(fn func(age time.Duration)) ViewOption {
	return func(v *View) { v.onStale = fn }
}

// NewView returns a view for one exchange account. Until an announcement
// arrives every asset is UNKNOWN, so reconciliation skips rather than reporting
// the whole account as a break on the first pass.
func NewView(account string, opts ...ViewOption) *View {
	v := &View{account: account, maxAge: DefaultMaxAge, now: time.Now, assets: map[string]*big.Rat{}}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

// Handle folds an accounting.v1.PortfolioCashBalance announcement, keeping only
// the entries for this view's account. It is a bus.EventHandler.
//
// A LEVEL, NOT A DELTA: this REPLACES the account's assets rather than merging
// into them. Merging would leave an asset that fell to zero showing its last
// non-zero figure forever, because a zero balance is announced by ABSENCE from
// the map — and reconciliation would then report a permanent phantom break.
func (v *View) Handle(_ context.Context, _ *envelopepb.Envelope, payload []byte) error {
	var msg accountingpb.PortfolioCashBalance
	if err := proto.Unmarshal(payload, &msg); err != nil {
		// A permanent defect: nacking would replay the same bad bytes forever,
		// while the balance simply ages out to unknown and reconciliation skips.
		return nil
	}
	// OUT-OF-DOMAIN EXPONENTS ARE REFUSED BEFORE ANY NUMBER IS READ (#95):
	// dec.FromProto materialises 10^abs(exponent).
	if _, in := dec.InDomainDeep(&msg); !in {
		return nil
	}

	next := map[string]*big.Rat{}
	for _, e := range msg.GetByVenueAccount() {
		if e.GetVenueAccountId() != v.account || e.GetAsset() == "" {
			continue
		}
		next[e.GetAsset()] = dec.FromProto(e.GetAmount())
	}
	// AN ANNOUNCEMENT THAT MENTIONS THIS ACCOUNT NOWHERE STILL COUNTS. It says the
	// portfolio holds nothing here, which is a fact and not a gap — recording it
	// is what lets reconciliation notice the exchange holding something we do not
	// think we have.
	v.mu.Lock()
	v.assets = next
	v.asOf = msg.GetAsOf().AsTime()
	v.seen = true
	v.mu.Unlock()
	return nil
}

// Balance satisfies execution.ExpectedBalances: the amount of asset this account
// is believed to hold, or ok=false when UNKNOWN.
//
// UNKNOWN IS NOT ZERO, and the reconcilers depend on the difference: zero would
// make every asset the exchange holds a discrepancy the first time this ran.
//
// A KNOWN-EMPTY ACCOUNT ANSWERS ZERO, TRUE. Once an announcement has arrived,
// an asset absent from it is genuinely believed to be zero — and that is exactly
// the case worth reconciling, because the exchange holding something we think we
// do not is the discrepancy that matters most.
func (v *View) Balance(asset string) (*big.Rat, bool) {
	v.mu.RLock()
	seen, asOf := v.seen, v.asOf
	amount, held := v.assets[asset]
	v.mu.RUnlock()

	if !seen {
		return nil, false
	}
	if v.maxAge > 0 {
		if age := v.now().UTC().Sub(asOf); age > v.maxAge {
			if v.onStale != nil {
				v.onStale(age)
			}
			return nil, false
		}
	}
	if !held {
		return new(big.Rat), true
	}
	return new(big.Rat).Set(amount), true
}
