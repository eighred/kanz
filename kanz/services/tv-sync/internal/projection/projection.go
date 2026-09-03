package projection

import (
	"context"
	"fmt"
	"github.com/eighred/kanz/internal/fillfact"
	"math/big"
	"sort"
	"sync"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/dec"
)

// OMS FACT subjects the projection folds (mirrors services/oms/internal/order).
const (
	evtAccepted        = "order.order.accepted"
	evtRouted          = "order.order.routed"
	evtPartiallyFilled = fillfact.SubjectPartiallyFilled
	evtFilled          = fillfact.SubjectFilled
	evtRejected        = "order.order.rejected"
	evtCancelled       = "order.order.cancelled"
	evtExpired         = "order.order.expired"
)

// Subjects returns the FACT subjects a consumer should subscribe Handle to.
func Subjects() []string {
	return []string{evtAccepted, evtRouted, evtPartiallyFilled, evtFilled, evtRejected, evtCancelled, evtExpired}
}

// MarkSource supplies a mark price for unrealized-P&L computation. nil marks (or
// a nil source) leave unrealized P&L empty — degrade, don't fabricate.
type MarkSource interface {
	Mark(instrument string) *big.Rat
}

// Projection is the zero-truth, multi-tenant read model. Safe for concurrent
// folds and reads.
type Projection struct {
	mu       sync.RWMutex
	accounts map[string]map[string]*account // tenant -> accountID -> account
	now      func() time.Time
	marks    MarkSource
	subs     *subscribers
	log      Log    // durable record of the FACTs folded (EXEC-M21); nil ⇒ memory only
	tenant   string // the tenant this pod is scoped to, when a log is bound
	// bootedFromCheckpoint records whether Rehydrate restored one, so the
	// composition root can report the posture rather than infer it from a timing.
	bootedFromCheckpoint bool
	// seq is the log position of the last fact this projection folded — the
	// watermark a checkpoint is taken at (#809). Guarded by mu, and only ever
	// advanced by a fold that actually happened.
	seq int64
}

// Option configures a Projection.
type Option func(*Projection)

// WithLog makes the projection DURABLE (EXEC-M21).
//
// Without it the book lives only in RAM, and tv-sync's consumer is a durable group: a
// restarted pod resumes at its last ack and never re-reads what it folded, so it comes back
// EMPTY and stays empty. The trader sees a TradingView account with no positions and zero
// P&L while the fund's positions sit open at the exchanges.
//
// The tenant is required and scopes the pod: the log's pool is tenant-scoped by construction
// (internal/pg.NewTenantPool), as every durable service on this platform is, so a FACT for
// another tenant has nowhere to land and is skipped rather than written across the boundary.
func WithLog(log Log, tenant string) Option {
	return func(p *Projection) {
		p.log = log
		p.tenant = tenant
	}
}

// New builds a Projection. now defaults to time.Now; marks may be nil.
func New(now func() time.Time, marks MarkSource, opts ...Option) *Projection {
	if now == nil {
		now = time.Now
	}
	p := &Projection{accounts: make(map[string]map[string]*account), now: now, marks: marks, subs: newSubscribers()}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Rehydrate rebuilds the book from the durable fact log (EXEC-M21).
//
// It replays the FACTs this tenant folded, in the order it folded them, through the SAME
// fold — so the rebuilt book is the book the dead pod had, not an approximation of it.
//
// WHY THIS LOG IS REQUIRED, stated accurately. The EXECUTION stream ages off at 24h
// (infra/nats/bootstrap-job.yaml: `ensure_stream EXECUTION "execution.>,strategy.>,order.>"
// 24h`), and no other table persists a fill — so after a day the bus cannot rebuild this.
//
// This comment used to add "nothing archives it". That was FALSE, and false in the
// direction that matters: the archiver mirrors execution.>, strategy.> AND order.> to
// Kafka, retained 30d (services/archiver/internal/config/config.go DefaultSubjects;
// infra/kafka/topics-job.yaml `order.order ... 2592000000`). A comment UNDERSTATING
// durability is not a harmless slip here — DR classification decisions read this file, and
// "nothing archives it" argues for treating the loss as unrecoverable when a 30-day copy
// exists (#149).
//
// The log is still required, for the honest reason rather than the dramatic one: that
// archive is the DR and audit path, not a rehydration source. It carries raw estate-wide
// FACTs, not this tenant's folded book, and nothing in this service reads it. "No reader is
// wired to the archive" is the argument; "no copy exists" was never true.
//
// Deltas are discarded: rehydration happens BEFORE the pod reports ready, so there is no
// subscriber to stream them to, and replaying a month of history to a live TradingView
// session would look like a month of trades happening at once.
//
// Call it before serving. A pod that has not finished rebuilding does not know what the fund
// holds, and reporting an empty account is worse than reporting nothing at all.
func (p *Projection) Rehydrate(ctx context.Context) error {
	if p.log == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	// THE CHECKPOINT FIRST, THEN THE TAIL (#809). Boot used to unmarshal and fold
	// every fact the fund had ever produced before the pod could report ready, so
	// startup grew with total history and each OOM kill cost longer than the last.
	//
	// A MISSING CHECKPOINT REPLAYS EVERYTHING. That is the pre-#809 boot and it
	// reaches the identical view — a fresh deployment and a truncated checkpoint
	// table are both this case. The failure that would matter is the opposite one:
	// treating an absent checkpoint as a restored empty account, which is exactly
	// EXEC-M21's defect (a trader looking at zero P&L while positions sat open).
	// So a load error is FATAL rather than degraded: booting from nothing when a
	// checkpoint exists but could not be read would silently re-fold from zero,
	// which is correct but slow, while booting from a PARTIAL one would not be.
	var after int64
	if blob, seq, ok, err := p.log.LoadCheckpoint(ctx); err != nil {
		return fmt.Errorf("tv-sync: read fold checkpoint: %w", err)
	} else if ok {
		if err := p.restore(blob); err != nil {
			return fmt.Errorf("tv-sync: restore fold checkpoint at seq %d: %w", seq, err)
		}
		after = seq
		p.bootedFromCheckpoint = true
	}

	return p.log.Replay(ctx, after, func(f Fact) error {
		p.fold(p.tenant, f.EventType, f.Payload, f.Knowledge)
		p.seq = f.Seq
		return nil
	})
}

// Handle is the bus.EventHandler: it folds one order-lifecycle FACT. Malformed
// payloads are acked (nil) — a poison FACT must not wedge the partition; the
// projection is a monitor, not the book of record.
//
// The FACT is DURABLY RECORDED BEFORE IT IS FOLDED (EXEC-M21). The order is the whole
// point: fold-then-record would put a FACT in RAM that a crash could lose, while the durable
// consumer group — which acks on return — would never send it again. Recording first means
// the fold is either durable or retried, never neither.
func (p *Projection) Handle(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	tenant := env.GetTenantId()
	if tenant == "" {
		return nil // untenanted event — cannot scope; skip (monitor only)
	}
	know := p.now().UTC()

	var appended int64
	if p.log != nil {
		if tenant != p.tenant {
			// Not this pod's tenant. The log is scoped to one tenant (as every durable
			// service on this platform is), so this FACT has nowhere to land — and folding
			// it into memory only would put it in a book no restart could rebuild.
			return nil
		}
		fresh, seq, err := p.log.Append(ctx, Fact{
			EventID: env.GetEventId(), EventType: env.GetEventType(),
			Payload: payload, Knowledge: know,
		})
		if err != nil {
			// NACK. Never fold what we could not durably record: the consumer group would
			// ack it, never resend it, and the next restart would lose it silently — the
			// original bug, one layer down.
			return err
		}
		appended = seq
		if !fresh {
			// Already folded — a redelivery after a lost ack, or a FACT this pod replayed at
			// boot. Folding it again would double a fill, and with it the position and the
			// realized P&L the Broker API reports.
			return nil
		}
	}

	p.mu.Lock()
	deltas := p.fold(tenant, env.GetEventType(), payload, know)
	// THE WATERMARK ADVANCES WITH THE FOLD, not with the append. A checkpoint
	// names a prefix of the log that this view has actually absorbed; recording a
	// seq for a fact that was appended and not folded would let the next boot skip
	// it (#809).
	if appended > 0 {
		p.seq = appended
	}
	p.mu.Unlock()

	for _, d := range deltas {
		p.subs.publish(tenant, d)
	}
	return nil
}

// decodeFact unmarshals a FACT payload and reports whether it is safe to fold.
//
// IT REFUSES AN OUT-OF-DOMAIN DECIMAL EXACTLY AS IT REFUSES A MALFORMED
// PAYLOAD (#95), and for a stronger reason than wrongness. Decimal.exponent is
// an unvalidated wire field; this projection reads it through orderDTOAsOf and
// feeRat, which return bare strings and rationals with nowhere to put an error,
// and dec.FromProto materialises 10^abs(exponent). A fill carrying
// {1, 2000000000} would not render a wrong quantity in the Broker API — it
// would never return, holding p.mu, and every subsequent fold and every reader
// blocks behind it. The projection stops, permanently, on one message.
//
// dec.InDomainDeep walks the WHOLE decoded FACT rather than the fields this
// file happens to read today, so a Decimal added to the schema later is covered
// without anyone remembering to come back here.
//
// The refused FACT is not lost: Handle appended it to the durable log before
// folding, so it remains auditable and the drop is recoverable — which is why
// matching the malformed path is safe here rather than merely convenient.
func decodeFact(payload []byte, ev proto.Message) bool {
	if proto.Unmarshal(payload, ev) != nil {
		return false
	}
	_, in := dec.InDomainDeep(ev)
	return in
}

// fold applies one FACT under p.mu and returns the deltas to stream.
func (p *Projection) fold(tenant, eventType string, payload []byte, know time.Time) []Delta {
	switch eventType {
	case evtAccepted:
		var ev orderpb.OrderAccepted
		if !decodeFact(payload, &ev) {
			return nil
		}
		return p.upsertOrder(tenant, ev.GetState(), ev.GetState().GetStatus(), "", know)
	case evtPartiallyFilled:
		var ev orderpb.OrderPartiallyFilled
		if !decodeFact(payload, &ev) {
			return nil
		}
		d := p.upsertOrder(tenant, ev.GetState(), ev.GetState().GetStatus(), "", know)
		return append(d, p.appendFill(tenant, ev.GetState().GetPortfolioId(), ev.GetFill(), know)...)
	case evtFilled:
		var ev orderpb.OrderFilled
		if !decodeFact(payload, &ev) {
			return nil
		}
		d := p.upsertOrder(tenant, ev.GetState(), ev.GetState().GetStatus(), "", know)
		return append(d, p.appendFill(tenant, ev.GetState().GetPortfolioId(), ev.GetFill(), know)...)
	case evtRouted:
		var ev orderpb.OrderRouted
		if !decodeFact(payload, &ev) {
			return nil
		}
		return p.transition(tenant, ev.GetOrderId(), orderpb.OrderStatus_ORDER_STATUS_ROUTED, ev.GetVenue(), "", know)
	case evtRejected:
		var ev orderpb.OrderRejected
		if !decodeFact(payload, &ev) {
			return nil
		}
		return p.transition(tenant, ev.GetOrderId(), orderpb.OrderStatus_ORDER_STATUS_REJECTED, "", ev.GetReason(), know)
	case evtCancelled:
		var ev orderpb.OrderCancelled
		if !decodeFact(payload, &ev) {
			return nil
		}
		return p.transition(tenant, ev.GetOrderId(), orderpb.OrderStatus_ORDER_STATUS_CANCELLED, "", "", know)
	case evtExpired:
		var ev orderpb.OrderExpired
		if !decodeFact(payload, &ev) {
			return nil
		}
		return p.transition(tenant, ev.GetOrderId(), orderpb.OrderStatus_ORDER_STATUS_EXPIRED, "", "", know)
	default:
		return nil
	}
}

func (p *Projection) acct(tenant, id string, create bool) *account {
	byAcct := p.accounts[tenant]
	if byAcct == nil {
		if !create {
			return nil
		}
		byAcct = make(map[string]*account)
		p.accounts[tenant] = byAcct
	}
	a := byAcct[id]
	if a == nil && create {
		a = newAccount(tenant, id)
		byAcct[id] = a
	}
	return a
}

func (p *Projection) upsertOrder(tenant string, st *orderpb.OrderState, status orderpb.OrderStatus, reason string, know time.Time) []Delta {
	if st == nil || st.GetPortfolioId() == "" || st.GetOrderId() == "" {
		return nil
	}
	a := p.acct(tenant, st.GetPortfolioId(), true)
	venue := lastVenue(a.orders[st.GetOrderId()])
	a.orders[st.GetOrderId()] = append(a.orders[st.GetOrderId()], orderRev{
		state: proto.Clone(st).(*orderpb.OrderState), status: status, venue: venue,
		effective: st.GetAsOf().AsTime(), knowledge: know,
	})
	return []Delta{{Kind: "order", Account: a.id, Payload: orderDTO(a.orders[st.GetOrderId()], "")}}
}

// transition records a status change that carried no full OrderState (routed /
// reject / cancel / expire), carrying forward the last known state. A FACT for
// an order the projection never admitted is ignored (a pre-admission reject has
// no account context on the wire).
func (p *Projection) transition(tenant, orderID string, status orderpb.OrderStatus, venue, reason string, know time.Time) []Delta {
	byAcct := p.accounts[tenant]
	for _, a := range byAcct {
		revs := a.orders[orderID]
		if len(revs) == 0 {
			continue
		}
		last := revs[len(revs)-1]
		v := last.venue
		if venue != "" {
			v = venue
		}
		a.orders[orderID] = append(revs, orderRev{
			state: last.state, status: status, reason: reason, venue: v,
			effective: know, knowledge: know,
		})
		return []Delta{{Kind: "order", Account: a.id, Payload: orderDTO(a.orders[orderID], "")}}
	}
	return nil
}

func (p *Projection) appendFill(tenant, accountID string, f *orderpb.Fill, know time.Time) []Delta {
	if f == nil || accountID == "" {
		return nil
	}
	a := p.acct(tenant, accountID, true)
	// Idempotent fold: a fill arrives twice — once from the synchronous venue
	// placement path, once as the async user-data websocket echo — under the same
	// deterministic fill_id. Fold it once, never double-ledger (M3.7).
	if fid := f.GetFillId(); fid != "" {
		if a.seenFills[fid] {
			return nil
		}
		a.seenFills[fid] = true
	}
	ex := execution{
		fillID: f.GetFillId(), orderID: f.GetOrderId(), instrument: f.GetInstrumentId(),
		venue: f.GetVenue(), side: f.GetSide(),
		qty: dec.FromProto(f.GetQuantity()), price: dec.FromProto(f.GetPrice()),
		fee: feeRat(f), effective: f.GetExecutedAt().AsTime(), knowledge: know,
	}
	a.execs = append(a.execs, ex)
	// Maintain the as-of-now fold in the same order the full fold would visit
	// (#995). This is the whole of the O(N²) fix: the arithmetic is unchanged,
	// it simply runs once per execution rather than once per execution per read.
	applyExecution(a.livePos(ex.instrument), ex)
	return []Delta{
		{Kind: "execution", Account: a.id, Payload: executionDTO(ex)},
		{Kind: "position", Account: a.id, Payload: p.positionsLocked(a, time.Time{})},
		{Kind: "state", Account: a.id, Payload: p.stateLocked(a, time.Time{})},
	}
}

// --- reads (tenant-scoped, bitemporal) ---

// Accounts lists a tenant's accounts.
func (p *Projection) Accounts(tenant string) []AccountDTO {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []AccountDTO
	for id := range p.accounts[tenant] {
		out = append(out, AccountDTO{ID: id, Name: id, Currency: "USD"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Positions returns net positions as known at asOf (zero ⇒ latest).
func (p *Projection) Positions(tenant, accountID string, asOf time.Time) ([]PositionDTO, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	a := p.readAcct(tenant, accountID)
	if a == nil {
		return nil, false
	}
	return p.positionsLocked(a, asOf), true
}

// State returns the account balance/equity/P&L summary as of asOf.
func (p *Projection) State(tenant, accountID string, asOf time.Time) (*StateDTO, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	a := p.readAcct(tenant, accountID)
	if a == nil {
		return nil, false
	}
	s := p.stateLocked(a, asOf)
	return &s, true
}

// Orders returns each order's state as known at asOf.
func (p *Projection) Orders(tenant, accountID string, asOf time.Time) ([]OrderDTO, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	a := p.readAcct(tenant, accountID)
	if a == nil {
		return nil, false
	}
	var out []OrderDTO
	for _, revs := range a.orders {
		if dto, ok := orderDTOAsOf(revs, asOf); ok {
			out = append(out, dto)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OrderID < out[j].OrderID })
	return out, true
}

// Executions returns the execution log as known at asOf, optionally filtered to
// one instrument.
func (p *Projection) Executions(tenant, accountID, instrument string, asOf time.Time) ([]ExecutionDTO, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	a := p.readAcct(tenant, accountID)
	if a == nil {
		return nil, false
	}
	var out []ExecutionDTO
	for _, e := range visibleExecs(a.execs, asOf) {
		if instrument != "" && e.instrument != instrument {
			continue
		}
		out = append(out, executionDTO(e))
	}
	return out, true
}

func (p *Projection) readAcct(tenant, accountID string) *account {
	if byAcct := p.accounts[tenant]; byAcct != nil {
		return byAcct[accountID]
	}
	return nil
}

func (p *Projection) positionsLocked(a *account, asOf time.Time) []PositionDTO {
	folded := p.foldFor(a, asOf)
	var out []PositionDTO
	for inst, ps := range folded {
		if ps.Qty.Sign() == 0 && ps.Realized.Sign() == 0 {
			continue
		}
		dto := PositionDTO{
			Instrument: inst, Side: posSide(ps.Qty),
			Qty: dec.Str(new(big.Rat).Abs(ps.Qty)), AvgPrice: dec.Str(ps.AvgCost),
			RealizedPnl: dec.Str(ps.Realized),
		}
		if u := ps.Unrealized(p.markOf(inst)); u != nil {
			dto.UnrealizedPnl = dec.Str(u)
		}
		out = append(out, dto)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Instrument < out[j].Instrument })
	return out
}

func (p *Projection) stateLocked(a *account, asOf time.Time) StateDTO {
	folded := p.foldFor(a, asOf)
	realized, unrealized := new(big.Rat), new(big.Rat)
	open := 0
	for inst, ps := range folded {
		realized.Add(realized, ps.Realized)
		if ps.Qty.Sign() != 0 {
			open++
		}
		if u := ps.Unrealized(p.markOf(inst)); u != nil {
			unrealized.Add(unrealized, u)
		}
	}
	// Balance = realized P&L (start cash is 0 in M1 sim; a start-cash seam binds
	// the accounting NAV projection later). Equity = balance + unrealized.
	equity := new(big.Rat).Add(realized, unrealized)
	return StateDTO{
		AccountID: a.id, Currency: "USD",
		Balance: dec.Str(realized), Equity: dec.Str(equity),
		RealizedPnl: dec.Str(realized), UnrealizedPnl: dec.Str(unrealized),
		OpenPositions: open,
	}
}

func (p *Projection) markOf(instrument string) *big.Rat {
	if p.marks == nil {
		return nil
	}
	return p.marks.Mark(instrument)
}

// Subscribe registers a streaming subscriber for one tenant's account deltas.
// The returned cancel unregisters it.
func (p *Projection) Subscribe(tenant, accountID string) (<-chan Delta, func()) {
	return p.subs.subscribe(tenant, accountID)
}

// --- helpers ---

// foldFor returns the position fold an as-of read should see (#995).
//
// A ZERO asOf IS THE LIVE READ and gets the maintained fold. Anything else is a
// bitemporal question — "what did we believe at that instant" — which only the
// retained history can answer, so it still walks it. That path is an operator
// query rather than the fill path, so its cost is paid by the caller who asked
// for it instead of by every trader watching a position.
//
// THE RESULT IS SHARED, NOT COPIED, and that is safe rather than lucky: both
// callers only read it. positionsLocked copies through new(big.Rat).Abs before
// touching a quantity, stateLocked accumulates into fresh rats, and
// costbasis.Lot.Unrealized allocates its result. A future reader that mutates
// what it is handed here would corrupt the live fold for every subsequent read,
// so if this ever needs to hand out a mutable view it must clone.
func (p *Projection) foldFor(a *account, asOf time.Time) map[string]*posState {
	if asOf.IsZero() {
		return a.live
	}
	return foldPositions(visibleExecs(a.execs, asOf))
}

func visibleExecs(execs []execution, asOf time.Time) []execution {
	if asOf.IsZero() {
		return execs
	}
	out := make([]execution, 0, len(execs))
	for _, e := range execs {
		if !e.knowledge.After(asOf) {
			out = append(out, e)
		}
	}
	return out
}

func posSide(net *big.Rat) string {
	switch {
	case net.Sign() > 0:
		return "long"
	case net.Sign() < 0:
		return "short"
	default:
		return "flat"
	}
}

func lastVenue(revs []orderRev) string {
	if len(revs) == 0 {
		return ""
	}
	return revs[len(revs)-1].venue
}

func feeRat(f *orderpb.Fill) *big.Rat {
	if f.GetFee() == nil {
		return new(big.Rat)
	}
	return dec.FromProto(f.GetFee().GetAmount())
}

func orderDTO(revs []orderRev, _ string) OrderDTO {
	dto, _ := orderDTOAsOf(revs, time.Time{})
	return dto
}

func orderDTOAsOf(revs []orderRev, asOf time.Time) (OrderDTO, bool) {
	var cur *orderRev
	for i := range revs {
		if asOf.IsZero() || !revs[i].knowledge.After(asOf) {
			cur = &revs[i]
		}
	}
	if cur == nil {
		return OrderDTO{}, false
	}
	st := cur.state
	dto := OrderDTO{
		OrderID: st.GetOrderId(), Instrument: st.GetInstrumentId(), Venue: cur.venue,
		Side: sideString(st.GetSide()), Type: typeString(st.GetOrderType()),
		Status:    statusString(cur.status),
		Qty:       dec.Str(dec.FromProto(st.GetOrderedQuantity())),
		FilledQty: dec.Str(dec.FromProto(st.GetFilledQuantity())),
		LeavesQty: dec.Str(dec.FromProto(st.GetLeavesQuantity())),
		UpdatedAt: cur.knowledge.UTC().Format(time.RFC3339Nano),
		// EMPTY FOR AN ORDINARY ORDER, and omitempty keeps it off the wire — so
		// a client that has never heard of parent orders sees exactly what it saw
		// before, and one that has can group a schedule's slices instead of
		// showing them as unrelated rows (#484).
		ParentOrderID: st.GetParentOrderId(),
	}
	if st.GetAverageFillPrice() != nil {
		dto.AvgFillPrice = dec.Str(dec.FromProto(st.GetAverageFillPrice()))
	}
	if st.GetLimitPrice() != nil {
		dto.LimitPrice = dec.Str(dec.FromProto(st.GetLimitPrice()))
	}
	return dto, true
}

func executionDTO(e execution) ExecutionDTO {
	dto := ExecutionDTO{
		FillID: e.fillID, OrderID: e.orderID, Instrument: e.instrument, Venue: e.venue,
		Side: sideString(e.side), Qty: dec.Str(e.qty), Price: dec.Str(e.price),
		ExecutedAt: e.effective.UTC().Format(time.RFC3339Nano),
	}
	if e.fee != nil && e.fee.Sign() != 0 {
		dto.Fee = dec.Str(e.fee)
	}
	return dto
}

// BootedFromCheckpoint reports whether the last Rehydrate resumed from a fold
// checkpoint rather than replaying the whole fact log (#809).
//
// FALSE IS A LEGITIMATE POSTURE, not a failure: a fresh deployment has no
// checkpoint and a truncated table has none either, and both reach the identical
// view. It is worth reporting because a long-lived deployment that keeps booting
// false has a checkpoint loop that is not running, and the rebuild time beside it
// is the evidence.
func (p *Projection) BootedFromCheckpoint() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.bootedFromCheckpoint
}
