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
	"log/slog"
	"math/big"
	"sync"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	compliancepb "github.com/kanz-eng/kanz-schemas-go/compliance/v1"
	domainpb "github.com/kanz-eng/kanz-schemas-go/domain/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/proto"

	comp "github.com/eighred/kanz/internal/compliance"
)

// Monitor re-evaluates portfolios on every position change. Goroutine-safe.
type Monitor struct {
	// warnedUngoverned names each ungoverned portfolio once (guarded by mu, which
	// already protects the books below).
	warnedUngoverned map[string]bool

	engine     *comp.Engine
	mandates   comp.MandateSource
	classifier comp.Classifier
	emitter    *Emitter
	recorder   comp.DecisionRecorder
	now        func() time.Time
	logger     *slog.Logger

	mu         sync.Mutex
	books      map[string]map[string]comp.Position // portfolio → instrument → position
	lastStatus map[string]compliancepb.ComplianceStatus
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
		books:            make(map[string]map[string]comp.Position),
		lastStatus:       make(map[string]compliancepb.ComplianceStatus),
		warnedUngoverned: make(map[string]bool),
	}
}

// Handle is the bus.EventHandler for position-changed FACTs. A malformed payload
// is a permanent defect (acked); a transient emit/record failure is returned so
// the FACT is redelivered (the breach must not be lost).
func (m *Monitor) Handle(ctx context.Context, _ *envelopepb.Envelope, payload []byte) error {
	var ps domainpb.PositionState
	if err := proto.Unmarshal(payload, &ps); err != nil {
		m.logger.ErrorContext(ctx, "compliance monitor: malformed PositionState", "err", err)
		return nil
	}
	pid := ps.GetPortfolioId()
	if pid == "" {
		return nil
	}

	trigger, book, asOf := m.applyAndSnapshot(pid, &ps)

	mandate, ok, err := m.mandates.Mandate(ctx, pid, asOf)
	if err != nil {
		return err // transient mandate lookup ⇒ retry
	}
	// The same two states the pre-trade gate distinguishes (EXEC-M14): a portfolio
	// with NO MANDATE is a gap nobody decided on, and it must not look identical to a
	// portfolio somebody deliberately left unconstrained. Said ONCE per portfolio —
	// this handler runs on every position change, and a monitor that floods its own
	// log is a monitor nobody reads.
	if !ok {
		m.mu.Lock()
		first := !m.warnedUngoverned[pid]
		m.warnedUngoverned[pid] = true
		m.mu.Unlock()
		if first {
			m.logger.Warn("UNGOVERNED: no mandate governs this portfolio — nothing is being checked against it",
				"portfolio_id", pid, "fix", "put it under mandate with kanz-mandate")
		}
		return nil
	}
	if len(mandate.GetRules()) == 0 {
		return nil // governed by a mandate that constrains nothing — a choice, not a gap
	}

	res := m.engine.Evaluate(ctx, &comp.Candidate{Book: book, Classifier: m.classifier, AsOf: asOf}, mandate)

	entered := m.recordStatus(pid, res.GetStatus())
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
			m.resetStatus(pid)
			return err
		}
	}
	return nil
}

// applyAndSnapshot folds the position update into the portfolio book and returns
// the breach trigger (POSITION_CHANGE when the quantity moved, else MARKET_MOVE),
// a candidate Book snapshot, and the event's as-of time.
func (m *Monitor) applyAndSnapshot(pid string, ps *domainpb.PositionState) (compliancepb.BreachTrigger, *comp.Book, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	insts := m.books[pid]
	if insts == nil {
		insts = make(map[string]comp.Position)
		m.books[pid] = insts
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
	book := &comp.Book{PortfolioID: pid}
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

// recordStatus updates the portfolio's last status and reports whether this is a
// fresh entry into BREACH (transition from non-breach).
func (m *Monitor) recordStatus(pid string, status compliancepb.ComplianceStatus) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.lastStatus[pid]
	m.lastStatus[pid] = status
	return status == compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH &&
		prev != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH
}

// resetStatus clears a portfolio's recorded status so the next evaluation
// re-emits — used when an emit fails after the status was advanced.
func (m *Monitor) resetStatus(pid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.lastStatus, pid)
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
