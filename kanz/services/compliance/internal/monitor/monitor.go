// Package monitor is the COMP-01d post-trade compliance monitor. It consumes
// the position-changed FACTs the risk engine already ingests
// (domain.v1.PositionState on risk.position.changed), maintains a per-portfolio
// book, and continuously re-evaluates each portfolio's mandate. When a book that
// was clean crosses into BREACH — typically a passive, market-move-induced
// breach (a position drifted past a concentration cap with no trade) — it emits
// a FACT-grade compliance.v1.ComplianceBreach (never shed) that AUTO-01 consumes
// to halt/escalate, and records the decision (COMP-01e).
//
// Only the transition INTO breach emits, so a persistently breaching book does
// not spam the stream; the FACT carries the full ComplianceResult so a consumer
// never re-evaluates.
package monitor

import (
	"context"
	"errors"
	"log/slog"
	"math/big"
	"sync"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/proto"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/bus"
)

// bookKey identifies a book by (tenant, portfolio).
//
// portfolio_id alone was the key, and it is a CALLER-CHOSEN string: two tenants
// each running a portfolio called "growth" shared one book here, so their
// positions summed into one NAV and each breach transition suppressed the
// other's (#243). The mandate lookup is tenant-scoped now, and a book keyed
// more loosely than the mandate it is evaluated against would put the collision
// back one layer down.
type bookKey struct{ tenant, portfolio string }

// book is everything this monitor holds for one (tenant, portfolio): the
// holdings, the last verdict, and whether the portfolio has already been named
// as ungoverned.
//
// THE THREE USED TO BE THREE MAPS KEYED THE SAME WAY, and that is why the
// portfolio half of #810 existed. Positions, lastStatus and warnedUngoverned all
// grew by (tenant, portfolio) and only one of them had a delete( anywhere, so
// evicting a dead portfolio meant remembering to evict it from three places and
// getting the order right relative to evaluate() — which writes lastStatus
// AFTER the fold that would have dropped it. One struct means one lifetime and
// one delete, and the three cannot drift.
type book struct {
	positions  map[string]comp.Position // instrument → position
	lastStatus compliancepb.ComplianceStatus
	// warnedUngoverned records that this book has already been named in a "not
	// being checked" message.
	warnedUngoverned bool
	// emptiedAt is when positions last became empty, and the zero time while the
	// book holds anything. It is what bookIdleRetention is measured from.
	emptiedAt time.Time
}

// Monitor re-evaluates portfolios on every position change. Goroutine-safe.
type Monitor struct {
	onDroppedRecord func()

	engine     *comp.Engine
	mandates   comp.MandateSource
	classifier comp.Classifier
	emitter    *Emitter
	recorder   comp.DecisionRecorder
	cash       comp.CashSource
	marks      comp.MarkSource
	now        func() time.Time
	logger     *slog.Logger

	mu    sync.Mutex
	books map[bookKey]*book // (tenant, portfolio) → book
	// lastBookGC rate-limits the idle sweep; see gcBooksLocked.
	lastBookGC time.Time
}

// NewMonitor wires the monitor. A nil engine defaults to comp.NewEngine(nil); a
// nil recorder disables decision logging.
// MonitorOption customizes a Monitor.
type MonitorOption func(*Monitor)

// WithDroppedRecordObserver counts post-trade decision records that never
// reached the audit trail.
//
// BEST-EFFORT IS RIGHT AND UNCOUNTED WAS NOT (#622). audit.go argues the
// swallow — "an audit-sink outage must not become a trading outage" — and it is
// correct. What it left behind was a log line, and this failure has a shape that
// makes that insufficient: EventTime is stamped from Result.GetEvaluatedAt() and
// the producer REFUSES a zero, so an upstream that forgets to stamp it makes
// EVERY record fail rather than some. A sink failing every time looks exactly
// like a sink that is quiet, which is #245 — the audit trail empty while trading
// continues and every probe green.
//
// Nil ⇒ not counted; the recorder still logs each failure.
func WithDroppedRecordObserver(fn func()) MonitorOption {
	return func(m *Monitor) { m.onDroppedRecord = fn }
}

// WithCashSource and WithMarkSource are what let this monitor answer "how
// levered is this book" at all (#787).
//
// WITHOUT THEM IT CANNOT, and could not before they existed. The position spine
// carries holdings and no portfolio equity, so the book built from it is a
// positions total — and gross exposure is the sum of the same values, which made
// a max_gross_leverage cap score exactly 1.0 for any long-only book and bind on
// nothing (#780). Since #786 that is a refusal rather than a silent pass, which
// is honest and is still not enforcement.
//
// With both wired the monitor joins the same three inputs the OMS pre-trade gate
// does — holdings, live marks, announced cash — through the same
// comp.JoinEquity, and a leverage cap becomes checkable AFTER the trade as well
// as before it. That matters because leverage is precisely a limit a book
// breaches WITHOUT TRADING: a drawdown on a financed book raises it with no
// order involved, and this monitor is the only thing watching for that.
//
// Either being nil leaves the book on its positions-only basis, which
// LeverageRule refuses with the reason recorded. Nil is a legal posture — a
// deployment with no price feed or no book of record — and it is not silent.
func WithCashSource(c comp.CashSource) MonitorOption {
	return func(m *Monitor) { m.cash = c }
}

// WithMarkSource supplies the reference prices the book is valued at. See
// WithCashSource: the two are needed together, because equity is marked
// positions plus cash and half of that is not a basis.
func WithMarkSource(mk comp.MarkSource) MonitorOption {
	return func(m *Monitor) { m.marks = mk }
}

func NewMonitor(engine *comp.Engine, mandates comp.MandateSource, classifier comp.Classifier, emitter *Emitter, recorder comp.DecisionRecorder, logger *slog.Logger, opts ...MonitorOption) *Monitor {
	if engine == nil {
		engine = comp.NewEngine(nil)
	}
	if logger == nil {
		logger = slog.Default()
	}
	m := &Monitor{
		engine:     engine,
		mandates:   mandates,
		classifier: classifier,
		emitter:    emitter,
		recorder:   recorder,
		now:        time.Now,
		logger:     logger,
		books:      make(map[bookKey]*book),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Handle is the bus.EventHandler for position-changed FACTs. A malformed payload
// is a permanent defect (acked); a transient emit/record failure is returned so
// the FACT is redelivered (the breach must not be lost).
func (m *Monitor) Handle(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	var ps domainpb.PositionState
	if err := proto.Unmarshal(payload, &ps); err != nil {
		m.logger.ErrorContext(ctx, "compliance monitor: malformed PositionState", "err", err)
		return nil
	}
	// An out-of-domain exponent is refused exactly like a malformed payload (#95):
	// the position's quantity and value are converted downstream with the unbounded
	// dec.FromProto, and a breach check that never returns is a breach check that
	// never fires — the monitor would look healthy while evaluating nothing.
	if field, in := dec.InDomainDeep(&ps); !in {
		m.logger.ErrorContext(ctx, "compliance monitor: PositionState carries an out-of-domain exponent — refusing",
			"field", field, "portfolio_id", ps.GetPortfolioId())
		return nil
	}
	pid := ps.GetPortfolioId()
	if pid == "" {
		return nil
	}
	// WHOSE position is this? The FACT itself carries no tenant, so it comes off
	// the envelope — the OMS position projector stamps its own OMS_TENANT on
	// every one (services/oms/internal/position/projector.go). Today that is
	// __system__ for the whole estate, which the registry resolves through its
	// shared-bucket branch; see comp.MandateRegistry.Mandate.
	key := bookKey{tenant: env.GetTenantId(), portfolio: pid}

	trigger, book, asOf := m.applyAndSnapshot(key, &ps)
	// A PORTFOLIO THAT NOW HOLDS NOTHING IS NOT EVALUATED, which is EXEC-M18's
	// existing rule reaching the FACT path because #810 made it reachable there.
	// Reevaluate has always refused to evaluate a portfolio the position spine has
	// not described, on the grounds that an empty book passes every concentration
	// limit there is; the FACT path never met one, because a closed position was
	// kept in the map and left the book non-empty.
	//
	// IT IS NOT A COSMETIC EXTENSION. Under a leverage cap an empty book is worse
	// than clean, it is a BREACH: with no holdings there is no base currency,
	// comp.equityFromMarks refuses to sum anything into equity, and LeverageRule
	// fails closed with "leverage cannot be verified". Evaluating the book of a
	// portfolio that just went flat would therefore emit a breach FACT — which
	// AUTO-01 halts and escalates on — for a fund holding nothing at all.
	if book == nil {
		return nil
	}
	return m.evaluate(ctx, key, book, trigger, asOf)
}

// evaluate runs one book against its mandate and emits the transition into
// BREACH. It is shared by every path that can move a book: a position FACT, a
// cash announcement, and the periodic re-evaluation sweep (#787).
//
// IT IS FACTORED OUT RATHER THAN COPIED because the three paths differ only in
// what WOKE them. The mandate lookup, the two terminal refusals, the ungoverned
// warning, the transition test, the record and the emit are identical, and a
// sweep that re-implemented any of them would drift from the FACT path — most
// dangerously on the transition test, where a second copy of lastStatus handling
// would re-emit a breach the FACT path had already reported.
//
// # Two clocks, and they are not the same question (#917, #930)
//
// observedAt is WHEN THE THING BEING EVALUATED WAS OBSERVED: the position FACT's
// as_of on the FACT path, and now on the two paths that are woken by something
// other than an observation. It stamps the breach FACT, the decision record and
// the result's evaluated_at, and it has to stay the observation's own time or a
// breach stops being attributable to what the system actually saw.
//
// EVERYTHING THIS FUNCTION LOOKS UP IS LOOKED UP AT NOW — the mandate below, and
// the reference data the rule engine classifies with (Candidate.ClassifyAsOf).
// Both are properties of the CURRENT world applied to the CURRENT book, and both
// were asked at observedAt until the boot replay showed what that costs. They
// failed differently, which is why they are two issues: the backdated mandate
// lookup found NOTHING and skipped the portfolio (#917), while the backdated
// reference-data lookup found a record dated after the question, refused it, and
// turned that refusal into a false BREACH (#930).
//
// THE MANDATE IS RESOLVED AT NOW. The book being evaluated is the CURRENT one —
// the holdings, marks and cash in force at this instant — so the mandate that
// governs it is the one in force at this instant too. Resolving it at observedAt
// asked the registry a question it cannot answer: it is armed from a COMPACTED
// stream serving one message per subject, so it holds the version in force plus
// the ones SCHEDULED and no superseded version to find (#884, #916). A backdated
// lookup therefore returns either the same answer as now or, when the mandate's
// effective_at is LATER than the FACT's as_of, NOTHING — and a miss here is
// UNGOVERNED, which declines to evaluate. This path boots on
// DeliverLastPerSubject over the compacted POSITION stream, where the as_of is as
// old as the portfolio's last fill, so a portfolio whose mandate is newer than
// its last trade was skipped by this control entirely — silently, and for as long
// as it did not trade.
//
// THE REFERENCE DATA IS READ AT NOW for the same reason, and it is the same boot
// replay that reaches it. refdata.Cache holds one snapshot per instrument and
// REFUSES a record whose own as_of is after the question's, so a classifier asked
// at a month-old fill resolves nothing for any instrument refreshed since — and
// comp.unresolvedDimension turns that into a violation, which is the right answer
// to "the dimension is dark" and the wrong answer to "the question was backdated".
// A SECTOR, ISSUER or ASSET_CLASS cap therefore breached on a book that was
// inside it, on every restart, and AUTO-01 halts on that FACT. Asking at now does
// NOT weaken the refusal: a dimension the master genuinely cannot resolve is
// unresolved at now too, and still breaches.
func (m *Monitor) evaluate(ctx context.Context, key bookKey, book *comp.Book, trigger compliancepb.BreachTrigger, observedAt time.Time) error {
	// THE TENANT COMES FROM THE BOOK, NOT FROM THE CALLER'S CONTEXT (#787).
	//
	// Emitter.EmitBreach sets no Event.TenantID and compliance configures no
	// producer-level fallback, so the tenant it publishes under is whatever is on
	// the ctx — which bus.Consumer stamps from the inbound envelope. Two of the
	// three paths into this function are NOT inbound deliveries: the sweep runs on
	// a ticker and Reevaluate can be called from anywhere, so their ctx carries no
	// tenant and every breach they found would fail to publish with "tenant_id
	// required". That is the same shape that once crash-looped the OMS.
	//
	// key.tenant is the authoritative answer on every path — Handle derives it
	// from the envelope, and the decision record below has always used it in
	// preference to the context for exactly that reason. Stamping it here makes
	// each path self-contained instead of dependent on how it was woken.
	ctx = bus.WithTenantID(ctx, key.tenant)
	pid := key.portfolio
	// AT NOW, NEVER AT observedAt — see the two-clocks note above (#917).
	mandate, ok, err := m.mandates.Mandate(ctx, key.tenant, pid, m.now().UTC())
	if err != nil {
		// A tenant the registry cannot resolve is TERMINAL — retrying re-reads the
		// same ambiguous data forever, and a redelivery loop on a position FACT is
		// how a monitor stops monitoring everything else. Refuse to evaluate,
		// loudly, and ack. Nothing is silently declared clean: the portfolio's
		// last status is left where it was, so the next unambiguous evaluation
		// still sees the transition.
		if errors.Is(err, comp.ErrMandateTenantUnresolved) {
			if m.firstUngoverned(key) {
				m.logger.ErrorContext(ctx, "NOT CHECKING this portfolio: cannot determine whose mandate governs it",
					"tenant_id", key.tenant, "portfolio_id", pid, "err", err)
			}
			return nil
		}
		// EQUALLY TERMINAL, and it must be listed here rather than fall to the
		// retry below (#619). A mandate that failed to apply sits on a COMPACTED
		// subject, so a redelivery re-reads the same bytes and fails identically —
		// returning err would put this monitor in the exact redelivery loop the
		// branch above exists to prevent, and one broken mandate would stop the
		// monitor evaluating every OTHER portfolio.
		//
		// Nothing is declared clean: the portfolio's last status is left where it
		// was, so a republished mandate still sees the transition.
		if errors.Is(err, comp.ErrMandateUnreadable) {
			if m.firstUngoverned(key) {
				m.logger.ErrorContext(ctx, "NOT CHECKING this portfolio: its published mandate could "+
					"not be applied, and the mandate stream is compacted, so nothing will check it "+
					"until the mandate is republished",
					"tenant_id", key.tenant, "portfolio_id", pid, "err", err)
			}
			return nil
		}
		return err // transient mandate lookup ⇒ retry
	}
	// The same two states the pre-trade gate distinguishes (EXEC-M14): a portfolio
	// with NO MANDATE is a gap nobody decided on, and it must not look identical to a
	// portfolio somebody deliberately left unconstrained. Said ONCE per (tenant,
	// portfolio) — this handler runs on every position change, and a monitor that
	// floods its own log is a monitor nobody reads.
	if !ok {
		if m.firstUngoverned(key) {
			m.logger.Warn("UNGOVERNED: no mandate governs this portfolio — nothing is being checked against it",
				"tenant_id", key.tenant, "portfolio_id", pid,
				"fix", "put it under mandate with `kanz-mandate --tenant "+key.tenant+"`")
		}
		return nil
	}
	if len(mandate.GetRules()) == 0 {
		return nil // governed by a mandate that constrains nothing — a choice, not a gap
	}

	res := m.engine.Evaluate(ctx, &comp.Candidate{
		Book:       book,
		Classifier: m.classifier,
		// THE STAMP, and it stays the observation's own time — evaluated_at is what
		// makes a breach attributable to what the system saw and when (#917).
		AsOf: observedAt,
		// THE REFERENCE-DATA CLOCK, and it is NOT observedAt (#930). See the
		// two-clocks note above: the book is the current one, so the classification
		// that applies to it is the current one.
		ClassifyAsOf: m.now().UTC(),
	}, mandate)

	entered := m.recordStatus(key, res.GetStatus())
	if res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH || !entered {
		return nil // not a new breach
	}

	if m.recorder != nil {
		// COUNTED, NOT IGNORED (#622). The error is still not returned — an
		// audit-sink outage must not become a trading outage — but a decision
		// that never reached the trail is now visible as a number rather than
		// only as a log line somebody has to be looking for.
		recordErr := m.recorder.Record(ctx, comp.DecisionRecord{
			Phase: comp.PhasePostTrade,
			// FROM THE BOOK'S KEY, not from the context (#713). The tenant is on
			// ctx here — this runs inside the inbound delivery — and passing it
			// explicitly costs nothing and makes the record self-contained, which
			// is what an asynchronous recorder needs and what a replayed one
			// cannot otherwise reconstruct.
			TenantID: key.tenant,
			Result:   res,
			Allowed:  false,
			Issuer:   "compliance:monitor",
			Trigger:  trigger.String(),
		})
		if recordErr != nil && m.onDroppedRecord != nil {
			m.onDroppedRecord()
		}
	}
	if m.emitter != nil {
		if err := m.emitter.EmitBreach(ctx, res, trigger, observedAt); err != nil {
			// Roll back the status so the next delivery re-emits — a dropped
			// breach FACT is worse than a duplicate.
			m.resetStatus(key)
			return err
		}
	}
	return nil
}

// applyAndSnapshot folds the position update into the portfolio book and returns
// the breach trigger (POSITION_CHANGE when the quantity moved, else MARKET_MOVE),
// a candidate Book snapshot, and the event's as-of time. A nil Book means the
// portfolio holds nothing after the fold — see Handle for why that is not
// evaluated.
func (m *Monitor) applyAndSnapshot(key bookKey, ps *domainpb.PositionState) (compliancepb.BreachTrigger, *comp.Book, time.Time) {
	trigger, book := m.applyLocked(key, ps)
	if book == nil {
		return trigger, nil, ps.GetAsOf().AsTime()
	}
	// OUTSIDE THE LOCK. See snapshotLocked: the join reaches into the cash view
	// and the mark fold, each with its own lock.
	comp.JoinEquity(book, m.cash, m.marks)
	return trigger, book, ps.GetAsOf().AsTime()
}

func (m *Monitor) applyLocked(key bookKey, ps *domainpb.PositionState) (compliancepb.BreachTrigger, *comp.Book) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.gcBooksLocked()

	b := m.books[key]
	if b == nil {
		b = &book{positions: make(map[string]comp.Position)}
		m.books[key] = b
	}
	prev, had := b.positions[ps.GetInstrumentId()]
	trigger := compliancepb.BreachTrigger_BREACH_TRIGGER_MARKET_MOVE
	if !had || !sameQuantity(prev.Quantity, ps.GetQuantity()) {
		trigger = compliancepb.BreachTrigger_BREACH_TRIGGER_POSITION_CHANGE
	}
	// A CLOSED POSITION LEAVES THE BOOK HERE (#810). It used to be written in and
	// filtered back out by heldPositions at read time, which made it INVISIBLE
	// rather than merely retained: nothing in any output counted it, and every
	// subsequent position FACT still paid to copy it. The book therefore grew with
	// the instruments the portfolio had EVER held, so a strategy that rotates
	// positions degraded the monitor monotonically — and because the POSITION
	// stream is compacted and this consumer boots on DeliverLastPerSubject, a
	// restart REBUILT the whole accumulated set rather than clearing it.
	if flatPosition(ps) {
		delete(b.positions, ps.GetInstrumentId())
	} else {
		b.positions[ps.GetInstrumentId()] = comp.Position{
			InstrumentID: ps.GetInstrumentId(),
			Quantity:     ps.GetQuantity(),
			MarketValue:  ps.GetMarketValue(),
		}
	}
	if len(b.positions) == 0 {
		if b.emptiedAt.IsZero() {
			b.emptiedAt = m.now()
		}
		// THE LAST VERDICT GOES WITH THE LAST HOLDING, and forgetting it here is
		// what keeps the next breach emittable. Handle does not evaluate an empty
		// book, so a portfolio that was BREACHING when its final position closed
		// would otherwise carry that status until something evaluated it again —
		// and the transition test in recordStatus would then see BREACH → BREACH
		// and emit NOTHING when the portfolio re-opened straight back into the same
		// breach. A swallowed breach FACT is the one outcome this monitor exists to
		// prevent. Clearing it is not declaring the book clean: nothing is emitted
		// and nothing is recorded, the portfolio simply has no verdict because it
		// has no holdings to have one about.
		b.lastStatus = compliancepb.ComplianceStatus_COMPLIANCE_STATUS_UNSPECIFIED
		return trigger, nil
	}
	b.emptiedAt = time.Time{}

	return trigger, m.snapshotLocked(key)
}

// flatPosition reports whether this FACT says the portfolio no longer holds the
// instrument at all, so the entry can be dropped rather than kept as a
// zero-weight ghost.
//
// # Each arm is a claim, and the third one is the trap
//
//   - THE PRODUCER MUST HAVE STATED A QUANTITY. A PositionState with no quantity
//     is not a flat position, it is a producer that did not say — and
//     comp.equityFromMarks refuses on exactly that ("carries no quantity, so it
//     cannot be valued"). Evicting it would delete the evidence for a refusal
//     that is doing its job.
//   - THE QUANTITY MUST BE EXACTLY ZERO. Not "worth nothing today": a holding
//     marked at zero can still have a quantity, and comp.equityFromMarks values
//     every position at quantity × the LIVE mark, so a position with a stale
//     zero market value and a real quantity contributes to equity as soon as the
//     mark fold has a price for it. Dropping that one would understate the
//     leverage denominator — the fail-open direction #760 already cost this
//     package once.
//   - THE MARKET VALUE MUST NOT ASSERT SOMETHING ELSE. A quantity of zero and a
//     non-zero market value is a contradiction, and the honest response to a
//     contradiction is to keep it where the rules can see it, not to resolve it
//     silently in the direction that frees memory.
//
// An ABSENT market value is not consulted, and that is deliberate rather than an
// oversight of comp's unmarked-holding refusal (#760/#806). That refusal exists
// because an unpriced holding could be any size in either direction; a holding
// of quantity zero is worth zero at every price there is, so no rule's reasoning
// about it can differ. Restating comp's "is this mark usable" test here would
// also be the fourth copy of the enumeration that test/arch's
// a_usable_mark_covers_every_money_field_test.go exists to stop.
func flatPosition(ps *domainpb.PositionState) bool {
	if ps.GetQuantity() == nil {
		return false
	}
	return ratFromDecimal(ps.GetQuantity()).Sign() == 0 &&
		ratFromDecimal(ps.GetMarketValue().GetAmount()).Sign() == 0
}

// bookIdleRetention is how long a book that holds NOTHING is kept before the
// (tenant, portfolio) entry itself is dropped, and bookGCInterval bounds how
// often the sweep that does it runs.
//
// # Why a portfolio is evicted on a timer and a position is not
//
// They are different lifetimes and the evidence for each is different. A
// position FACT stating quantity zero is the position spine SAYING the holding
// is closed, so the entry can go immediately. Nothing ever says "this portfolio
// is gone" — a book that holds nothing today may be one a strategy flattened
// between trades — so the only available evidence is that nothing has referred
// to it for a while.
//
// The retention is not zero, for a reason that has nothing to do with memory:
// evaluate() names an ungoverned portfolio ONCE, and that promise is kept by a
// flag inside the book. Dropping the book the instant it empties would rearm the
// warning, so a single-instrument portfolio flipping in and out of a position
// would reprint "UNGOVERNED" on every round trip — and a monitor that floods its
// own log is a monitor nobody reads, which is the reason the once-only rule
// exists. Fifteen minutes is far longer than any round trip and far shorter than
// a pod's life.
//
// # Why a book that still HOLDS something is never evicted, at any age
//
// This map is the monitor's only copy of the portfolio's holdings; it is rebuilt
// from position FACTs and from nothing else. Evicting a live book would not free
// stale data, it would make the next evaluation run against a book missing
// holdings nobody removed — a smaller book passes concentration and understates
// gross leverage, so the failure would be fail-OPEN and silent, which is worse
// than the leak it saved. The bound is therefore structural: one entry per
// portfolio that holds something, plus those emptied inside the window. A
// producer inventing portfolio ids is the estate-wide unbounded-key shape #842
// tracks and is not something this monitor can safely paper over.
const (
	bookIdleRetention = 15 * time.Minute
	bookGCInterval    = time.Minute
)

// gcBooksLocked drops books that have held nothing for longer than
// bookIdleRetention. Caller must hold m.mu.
//
// LAZY AND RATE-LIMITED, the shape internal/risk/state's dedupWindow.gc already
// uses: it runs from the position-FACT path so the bound exists in the type
// rather than in whatever the composition root remembered to wire, and the
// interval keeps an O(books) walk off the per-event path. A monitor receiving no
// FACTs at all sweeps nothing and also grows nothing.
func (m *Monitor) gcBooksLocked() {
	now := m.now()
	if now.Sub(m.lastBookGC) < bookGCInterval {
		return
	}
	m.lastBookGC = now
	cutoff := now.Add(-bookIdleRetention)
	for k, b := range m.books {
		if len(b.positions) == 0 && !b.emptiedAt.IsZero() && b.emptiedAt.Before(cutoff) {
			delete(m.books, k)
		}
	}
}

// snapshotLocked builds the candidate book from the positions held for key. The
// caller must hold m.mu.
//
// THE BOOK IT RETURNS IS A PROXY, and says so. NAV here is the net market value
// of the holdings and nothing else, because position FACTs carry no portfolio
// equity — and gross exposure is the sum of the same values, so handing this to
// LeverageRule as though it were equity made a max_gross_leverage cap score 1.0
// for any long-only book and bind on nothing (#780).
//
// The caller upgrades it through comp.JoinEquity, OUTSIDE this lock. That
// placement is deliberate: JoinEquity calls into the cash view and the mark
// fold, each of which takes its own lock, and reaching into two foreign locks
// while holding the one that guards every book in this process is how a monitor
// stops monitoring anything.
// ONE PASS, ONE ALLOCATION, and both halves of that were measured (#810). The
// copy runs on every position FACT, and it used to scan the map three times —
// once for the positions and the NAV, and twice more for navCurrency, whose
// result was thrown away and recomputed for the NAV's own currency code — while
// growing book.Positions from nil, which reallocates and re-copies as it doubles.
// Preallocating to len(positions) and folding the currency into the same pass
// took the 50-holding snapshot from 59 allocations to 53 and the 1000-holding one
// from 1013 to 1003, on top of the far larger win from evicting the flats that
// made those maps big in the first place.
//
// It also settles a disagreement the two navCurrency calls could produce: Go
// randomizes map iteration per range, so on a book holding more than one currency
// the BaseCurrency and the NAV's currency code were each "the first non-empty one
// seen" in a DIFFERENT order, and could name different currencies on the same
// snapshot. One pass makes them the same value by construction.
func (m *Monitor) snapshotLocked(key bookKey) *comp.Book {
	b := m.books[key]
	var positions map[string]comp.Position
	if b != nil {
		positions = b.positions
	}
	out := make([]comp.Position, 0, len(positions))
	var nav *commonpb.Decimal
	// THE CURRENCY THE HOLDINGS ARE IN. It was left unset while nothing read it;
	// comp.JoinEquity does, and refuses to net cash against positions without it
	// rather than assuming they match (#787).
	currency := ""
	for _, p := range positions {
		out = append(out, p)
		if p.MarketValue != nil {
			nav = addDecimal(nav, p.MarketValue.GetAmount())
		}
		if currency == "" {
			currency = p.MarketValue.GetCurrencyCode()
		}
	}
	return &comp.Book{
		PortfolioID:  key.portfolio,
		BaseCurrency: currency,
		NAVBasis:     comp.NAVBasisGrossPositions,
		Positions:    out,
		NAV:          &commonpb.Money{Amount: nav, CurrencyCode: currency},
	}
}

// snapshot builds a book and joins its equity, taking and releasing the lock.
func (m *Monitor) snapshot(key bookKey) *comp.Book {
	m.mu.Lock()
	book := m.snapshotLocked(key)
	m.mu.Unlock()
	comp.JoinEquity(book, m.cash, m.marks)
	return book
}

// ReevaluateAll re-runs every book this monitor holds against its mandate,
// against the marks and cash in force NOW (#787).
//
// # Why a monitor driven only by position FACTs is not a monitor
//
// Everything above this line is woken by a position change. That is enough for
// the limits that move when the BOOK moves — a concentration cap crossed by a
// fill — and it is not enough for the limits that move when the MARKET moves.
// Leverage is the clearest case: a financed book that falls 20% raises its gross
// leverage with no order placed anywhere, and until this existed there was no
// path by which anything noticed. The package doc calls that "a passive,
// market-move-induced breach" and names it as the reason the monitor exists; the
// position spine simply never delivered one, because the OMS position book
// values holdings at average cost and republishes only when a fill lands.
//
// # Why a sweep and not a subscription to market data
//
// The mark fold IS subscribed (the service folds market.*.trade and
// market.*.quote), so this process already knows every price as it arrives. What
// it must not do is re-evaluate every portfolio holding an instrument on every
// tick: that couples compliance evaluation to market-data volume, which is the
// one rate on this platform nobody controls. A sweep decouples them — the work
// per interval is bounded by the number of BOOKS, not the number of ticks — and
// it costs at most one interval of latency on a passive breach, which is the
// right trade for a control that exists to catch drift rather than to gate an
// order.
//
// # It cannot double-emit
//
// Every path goes through evaluate, which emits only on the TRANSITION into
// BREACH (recordStatus). A book already breaching when the sweep reaches it
// emits nothing; a book the FACT path has just reported emits nothing here. That
// is why the sweep re-uses evaluate rather than carrying its own emit.
//
// Errors are collected and returned as one, so a single portfolio whose mandate
// lookup is transiently failing does not stop the rest of the sweep.
func (m *Monitor) ReevaluateAll(ctx context.Context) error {
	m.mu.Lock()
	keys := make([]bookKey, 0, len(m.books))
	for k := range m.books {
		keys = append(keys, k)
	}
	m.mu.Unlock()

	var firstErr error
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := m.Reevaluate(ctx, key.tenant, key.portfolio); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Reevaluate re-runs one portfolio against its mandate with the marks and cash
// in force now. It is what a cash announcement triggers — cash is half of
// equity, so a drawdown on a margin loan moves leverage with no position FACT
// behind it — and what ReevaluateAll calls per book.
//
// A portfolio this monitor holds no positions for is a no-op rather than an
// empty book: an empty book passes every concentration limit there is, and
// evaluating one because an announcement mentioned a portfolio the position
// spine has never described would report it clean (EXEC-M18).
func (m *Monitor) Reevaluate(ctx context.Context, tenant, portfolio string) error {
	key := bookKey{tenant: tenant, portfolio: portfolio}
	m.mu.Lock()
	b, known := m.books[key]
	held := known && len(b.positions) > 0
	m.mu.Unlock()
	// HOLDS NOTHING IS THE SAME ANSWER AS NEVER DESCRIBED, and since #810 the two
	// are also the same state: a book whose last holding closed is emptied rather
	// than left full of zero-weight ghosts, so this test has to read the holdings
	// and not merely the key. Reading the key alone would have re-admitted exactly
	// what EXEC-M18 rules out — evaluating an empty book, which passes every
	// concentration limit there is.
	if !held {
		return nil
	}
	// MARKET_MOVE is the honest trigger for both callers. Nothing traded: the
	// book's value moved, or the cash behind it did. The schema's other value
	// (POSITION_CHANGE) would claim a fill that did not happen.
	return m.evaluate(ctx, key, m.snapshot(key),
		compliancepb.BreachTrigger_BREACH_TRIGGER_MARKET_MOVE, m.now().UTC())
}

// recordStatus updates the book's last status and reports whether this is a
// fresh entry into BREACH (transition from non-breach).
// A BOOK THE SWEEP HAS ALREADY DROPPED IS TREATED AS ONE THAT HAS NEVER BEEN
// SEEN, in whichever direction is safe. Here that is "this IS a transition", so
// a breach is emitted rather than swallowed: a duplicate breach FACT is the cost
// EmitBreach's rollback below already accepts, and a dropped one is not.
func (m *Monitor) recordStatus(key bookKey, status compliancepb.ComplianceStatus) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	entering := status == compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH
	b := m.books[key]
	if b == nil {
		return entering
	}
	prev := b.lastStatus
	b.lastStatus = status
	return entering && prev != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH
}

// resetStatus clears a book's recorded status so the next evaluation re-emits —
// used when an emit fails after the status was advanced.
func (m *Monitor) resetStatus(key bookKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b := m.books[key]; b != nil {
		b.lastStatus = compliancepb.ComplianceStatus_COMPLIANCE_STATUS_UNSPECIFIED
	}
}

// firstUngoverned reports whether this book has not yet been named in a
// "not being checked" message, and records that it now has. A book the sweep has
// dropped warns again — see bookIdleRetention for why the window is long enough
// that this is not the flood the once-only rule exists to prevent.
func (m *Monitor) firstUngoverned(key bookKey) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.books[key]
	if b == nil {
		return true
	}
	if b.warnedUngoverned {
		return false
	}
	b.warnedUngoverned = true
	return true
}

func sameQuantity(a, b *commonpb.Decimal) bool {
	return ratFromDecimal(a).Cmp(ratFromDecimal(b)) == 0
}

// --- local exact-decimal helpers (monitor-only) ----------------------------

func ratFromDecimal(d *commonpb.Decimal) *big.Rat {
	r := new(big.Rat)
	if d == nil {
		return r
	}
	r.SetInt64(d.GetCoefficient())
	exp := d.GetExponent()
	if exp == 0 {
		return r
	}
	n := int64(exp)
	if n < 0 {
		n = -n
	}
	scale := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(n), nil))
	if exp > 0 {
		return r.Mul(r, scale)
	}
	return r.Quo(r, scale)
}

// addDecimal returns a+b exactly, aligning to the smaller exponent. nil is zero.
func addDecimal(a, b *commonpb.Decimal) *commonpb.Decimal {
	if a == nil {
		a = &commonpb.Decimal{}
	}
	if b == nil {
		b = &commonpb.Decimal{}
	}
	exp := a.GetExponent()
	if b.GetExponent() < exp {
		exp = b.GetExponent()
	}
	pow := func(n int32) int64 {
		p := int64(1)
		for ; n > 0; n-- {
			p *= 10
		}
		return p
	}
	return &commonpb.Decimal{
		Coefficient: a.GetCoefficient()*pow(a.GetExponent()-exp) + b.GetCoefficient()*pow(b.GetExponent()-exp),
		Exponent:    exp,
	}
}
