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

// Monitor re-evaluates portfolios on every position change. Goroutine-safe.
type Monitor struct {
	// warnedUngoverned names each ungoverned (tenant, portfolio) once (guarded by
	// mu, which already protects the books below).
	warnedUngoverned map[bookKey]bool

	engine     *comp.Engine
	mandates   comp.MandateSource
	classifier comp.Classifier
	emitter    *Emitter
	recorder   comp.DecisionRecorder
	now        func() time.Time
	logger     *slog.Logger

	mu         sync.Mutex
	books      map[bookKey]map[string]comp.Position // (tenant, portfolio) → instrument → position
	lastStatus map[bookKey]compliancepb.ComplianceStatus
}

// NewMonitor wires the monitor. A nil engine defaults to comp.NewEngine(nil); a
// nil recorder disables decision logging.
func NewMonitor(engine *comp.Engine, mandates comp.MandateSource, classifier comp.Classifier, emitter *Emitter, recorder comp.DecisionRecorder, logger *slog.Logger) *Monitor {
	if engine == nil {
		engine = comp.NewEngine(nil)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Monitor{
		engine:           engine,
		mandates:         mandates,
		classifier:       classifier,
		emitter:          emitter,
		recorder:         recorder,
		now:              time.Now,
		logger:           logger,
		books:            make(map[bookKey]map[string]comp.Position),
		lastStatus:       make(map[bookKey]compliancepb.ComplianceStatus),
		warnedUngoverned: make(map[bookKey]bool),
	}
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

	mandate, ok, err := m.mandates.Mandate(ctx, key.tenant, pid, asOf)
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

	res := m.engine.Evaluate(ctx, &comp.Candidate{Book: book, Classifier: m.classifier, AsOf: asOf}, mandate)

	entered := m.recordStatus(key, res.GetStatus())
	if res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH || !entered {
		return nil // not a new breach
	}

	if m.recorder != nil {
		_ = m.recorder.Record(ctx, comp.DecisionRecord{
			Phase:   comp.PhasePostTrade,
			Result:  res,
			Allowed: false,
			Issuer:  "compliance:monitor",
			Trigger: trigger.String(),
		})
	}
	if m.emitter != nil {
		if err := m.emitter.EmitBreach(ctx, res, trigger, asOf); err != nil {
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
// a candidate Book snapshot, and the event's as-of time.
func (m *Monitor) applyAndSnapshot(key bookKey, ps *domainpb.PositionState) (compliancepb.BreachTrigger, *comp.Book, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	insts := m.books[key]
	if insts == nil {
		insts = make(map[string]comp.Position)
		m.books[key] = insts
	}
	prev, had := insts[ps.GetInstrumentId()]
	trigger := compliancepb.BreachTrigger_BREACH_TRIGGER_MARKET_MOVE
	if !had || !sameQuantity(prev.Quantity, ps.GetQuantity()) {
		trigger = compliancepb.BreachTrigger_BREACH_TRIGGER_POSITION_CHANGE
	}
	insts[ps.GetInstrumentId()] = comp.Position{
		InstrumentID: ps.GetInstrumentId(),
		Quantity:     ps.GetQuantity(),
		MarketValue:  ps.GetMarketValue(),
	}

	// Build the candidate book. NAV is the net market value of the holdings — a
	// funded-book proxy, since position FACTs carry no portfolio equity (the
	// same proxy the OMS book source uses). Summed with exact decimal addition.
	book := &comp.Book{PortfolioID: key.portfolio}
	var nav *commonpb.Decimal
	for _, p := range insts {
		book.Positions = append(book.Positions, p)
		if p.MarketValue != nil {
			nav = addDecimal(nav, p.MarketValue.GetAmount())
		}
	}
	book.NAV = &commonpb.Money{Amount: nav, CurrencyCode: navCurrency(insts)}
	return trigger, book, ps.GetAsOf().AsTime()
}

// recordStatus updates the book's last status and reports whether this is a
// fresh entry into BREACH (transition from non-breach).
func (m *Monitor) recordStatus(key bookKey, status compliancepb.ComplianceStatus) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.lastStatus[key]
	m.lastStatus[key] = status
	return status == compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH &&
		prev != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH
}

// resetStatus clears a book's recorded status so the next evaluation re-emits —
// used when an emit fails after the status was advanced.
func (m *Monitor) resetStatus(key bookKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.lastStatus, key)
}

// firstUngoverned reports whether this book has not yet been named in a
// "not being checked" message, and records that it now has.
func (m *Monitor) firstUngoverned(key bookKey) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.warnedUngoverned[key] {
		return false
	}
	m.warnedUngoverned[key] = true
	return true
}

func sameQuantity(a, b *commonpb.Decimal) bool {
	return ratFromDecimal(a).Cmp(ratFromDecimal(b)) == 0
}

func navCurrency(insts map[string]comp.Position) string {
	for _, p := range insts {
		if p.MarketValue.GetCurrencyCode() != "" {
			return p.MarketValue.GetCurrencyCode()
		}
	}
	return ""
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
