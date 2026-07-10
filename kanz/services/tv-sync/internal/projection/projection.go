package projection

import (
	"context"
	"math/big"
	"sort"
	"sync"
	"time"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"

	"github.com/kanz-eng/kanz/services/tv-sync/internal/dec"
)

// OMS FACT subjects the projection folds (mirrors services/oms/internal/order).
const (
	evtAccepted        = "order.order.accepted"
	evtRouted          = "order.order.routed"
	evtPartiallyFilled = "order.order.partially_filled"
	evtFilled          = "order.order.filled"
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
}

// New builds a Projection. now defaults to time.Now; marks may be nil.
func New(now func() time.Time, marks MarkSource) *Projection {
	if now == nil {
		now = time.Now
	}
	return &Projection{accounts: make(map[string]map[string]*account), now: now, marks: marks, subs: newSubscribers()}
}

// Handle is the bus.EventHandler: it folds one order-lifecycle FACT. Malformed
// payloads are acked (nil) — a poison FACT must not wedge the partition; the
// projection is a monitor, not the book of record.
func (p *Projection) Handle(_ context.Context, env *envelopepb.Envelope, payload []byte) error {
	tenant := env.GetTenantId()
	if tenant == "" {
		return nil // untenanted event — cannot scope; skip (monitor only)
	}
	know := p.now().UTC()

	p.mu.Lock()
	deltas := p.fold(tenant, env.GetEventType(), payload, know)
	p.mu.Unlock()

	for _, d := range deltas {
		p.subs.publish(tenant, d)
	}
	return nil
}

// fold applies one FACT under p.mu and returns the deltas to stream.
func (p *Projection) fold(tenant, eventType string, payload []byte, know time.Time) []Delta {
	switch eventType {
	case evtAccepted:
		var ev orderpb.OrderAccepted
		if proto.Unmarshal(payload, &ev) != nil {
			return nil
		}
		return p.upsertOrder(tenant, ev.GetState(), ev.GetState().GetStatus(), "", know)
	case evtPartiallyFilled:
		var ev orderpb.OrderPartiallyFilled
		if proto.Unmarshal(payload, &ev) != nil {
			return nil
		}
		d := p.upsertOrder(tenant, ev.GetState(), ev.GetState().GetStatus(), "", know)
		return append(d, p.appendFill(tenant, ev.GetState().GetPortfolioId(), ev.GetFill(), know)...)
	case evtFilled:
		var ev orderpb.OrderFilled
		if proto.Unmarshal(payload, &ev) != nil {
			return nil
		}
		d := p.upsertOrder(tenant, ev.GetState(), ev.GetState().GetStatus(), "", know)
		return append(d, p.appendFill(tenant, ev.GetState().GetPortfolioId(), ev.GetFill(), know)...)
	case evtRouted:
		var ev orderpb.OrderRouted
		if proto.Unmarshal(payload, &ev) != nil {
			return nil
		}
		return p.transition(tenant, ev.GetOrderId(), orderpb.OrderStatus_ORDER_STATUS_ROUTED, ev.GetVenue(), "", know)
	case evtRejected:
		var ev orderpb.OrderRejected
		if proto.Unmarshal(payload, &ev) != nil {
			return nil
		}
		return p.transition(tenant, ev.GetOrderId(), orderpb.OrderStatus_ORDER_STATUS_REJECTED, "", ev.GetReason(), know)
	case evtCancelled:
		var ev orderpb.OrderCancelled
		if proto.Unmarshal(payload, &ev) != nil {
			return nil
		}
		return p.transition(tenant, ev.GetOrderId(), orderpb.OrderStatus_ORDER_STATUS_CANCELLED, "", "", know)
	case evtExpired:
		var ev orderpb.OrderExpired
		if proto.Unmarshal(payload, &ev) != nil {
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
	folded := foldPositions(visibleExecs(a.execs, asOf))
	var out []PositionDTO
	for inst, ps := range folded {
		if ps.net.Sign() == 0 && ps.realized.Sign() == 0 {
			continue
		}
		dto := PositionDTO{
			Instrument: inst, Side: posSide(ps.net),
			Qty: dec.Str(new(big.Rat).Abs(ps.net)), AvgPrice: dec.Str(ps.avg),
			RealizedPnl: dec.Str(ps.realized),
		}
		if u := ps.unrealized(p.markOf(inst)); u != nil {
			dto.UnrealizedPnl = dec.Str(u)
		}
		out = append(out, dto)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Instrument < out[j].Instrument })
	return out
}

func (p *Projection) stateLocked(a *account, asOf time.Time) StateDTO {
	folded := foldPositions(visibleExecs(a.execs, asOf))
	realized, unrealized := new(big.Rat), new(big.Rat)
	open := 0
	for inst, ps := range folded {
		realized.Add(realized, ps.realized)
		if ps.net.Sign() != 0 {
			open++
		}
		if u := ps.unrealized(p.markOf(inst)); u != nil {
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
