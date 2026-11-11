package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/eighred/kanz/internal/fillfact"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/coder/websocket"

	"github.com/eighred/kanz/pkg/bus"
)

// executionReport is the Binance user-data "executionReport" event (the fields
// the ingester reads). Fills are the x=TRADE reports.
type executionReport struct {
	Event         string `json:"e"` // "executionReport"
	Symbol        string `json:"s"`
	ClientOrderID string `json:"c"`
	Side          string `json:"S"`
	ExecType      string `json:"x"` // NEW, TRADE, CANCELED, EXPIRED, REJECTED
	OrderStatus   string `json:"X"` // NEW, PARTIALLY_FILLED, FILLED, ...
	LastQty       string `json:"l"` // last executed quantity
	LastPrice     string `json:"L"` // last executed price
	Commission    string `json:"n"`
	CommissionAst string `json:"N"`
	CumQty        string `json:"z"` // cumulative filled quantity
	OrderQty      string `json:"q"` // order quantity
	TradeID       int64  `json:"t"`
	TransactTime  int64  `json:"T"`
	// EventTime is the stream's OWN clock for this report — when Binance emitted
	// it, not when the trade matched — and it is the ORDERING TOKEN this adapter
	// carries onto its view (#1046).
	//
	// IT IS WHAT order.v1's as_of IS DEFINED AS: "the event_time of the change
	// that produced it". Before this, applyFillToState stamped as_of from T, the
	// TRANSACT time, so the field the view orders by and the field the fill is
	// executed at were the same number and neither meant what its name said. T
	// stays on Fill.executed_at, which is the trade's own instant and the thing
	// TCA and the ledger date the execution by.
	//
	// THERE IS NO SEQUENCE NUMBER TO USE INSTEAD, and that is a property of the
	// venue rather than a shortcut here: Binance's spot executionReport carries no
	// per-order update id — u is the depth stream's field, not this one — so E is
	// the only monotone token the report offers. Decoding a field that is always
	// zero would be worse than none, because a zero token compares equal to every
	// other zero and silently disables the check it looks like it implements.
	//
	// ZERO MEANS THE FRAME DID NOT CARRY ONE, which is why reportAsOf falls back
	// to T rather than stamping the epoch: a view whose as_of is 1970 is one every
	// later report looks newer than, and the ordering gate would never fire again
	// for that order.
	EventTime int64 `json:"E"`
}

// UserDataIngester reads the Binance user-data stream and converts each fill
// (x=TRADE) into an order.v1.OrderFilled / OrderPartiallyFilled FACT fanned onto
// the bus — so a fill the synchronous placement path did not capture (a resting
// limit filling later, a partial over time) still reaches tv-sync/accounting.
// Fills are keyed by a deterministic fill_id (symbol-tradeId) identical to the
// BinanceVenue synchronous path, so downstream folds dedup the two idempotently.
type UserDataIngester struct {
	stream UserDataStream
	// orders is the adapter's own order view, READ AND WRITTEN (#904). Read to
	// enrich the report; written so the fill this ingester just published as a
	// FACT is also the fill the adapter believes in — see handle.
	orders OrderTracker
	pub    Publisher
	venue  string
	tenant string
	// refusal is the SHARED answer to an execution report this ingester will not
	// publish (#1045): counter, ERROR log, and a freeze on the adapter's own view.
	// Shared with the OKX connector rather than written out here, because the
	// defect it answers was in both and a per-connector refusal is one that gets
	// improved on one venue.
	refusal ReportRefusal
	// resolvedThisSession records that this websocket session got at least ONE
	// execution report all the way to a published fill FACT (#1047).
	//
	// IT IS WHAT THE RECONNECT BACK-OFF RESETS ON, and nothing else may reset it.
	// The loop used to reset its delay whenever Connect succeeded, which bounds
	// nothing when the exchange is healthy and the order view is not: every
	// session connected, died on the first report, and re-dialled instantly.
	// Publishing a fill is the one event a store outage cannot fake, so it is the
	// evidence the loop is allowed to treat as progress.
	//
	// Written by handle and read by the run loop, both on the goroutine that
	// calls Run — there is no second writer and no lock.
	resolvedThisSession bool
}

// UserDataConfig configures the ingester.
type UserDataConfig struct {
	Stream    UserDataStream
	Orders    OrderTracker
	Pub       Publisher
	Venue     string
	Tenant    string
	OnRefused func(mic, orderID, reason string)
	// OnDropped counts an execution report this ingester could not resolve to an
	// order it holds (#1047) — unknown_order or store_error. Distinct from
	// OnRefused, which counts a report it DID resolve and will not honour.
	OnDropped func(mic, orderID, reason string)
	Logger    *slog.Logger
}

func newUserDataIngester(cfg UserDataConfig) *UserDataIngester {
	if cfg.Venue == "" {
		cfg.Venue = "BINANCE"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &UserDataIngester{
		stream: cfg.Stream, orders: cfg.Orders, pub: cfg.Pub, venue: cfg.Venue, tenant: cfg.Tenant,
		refusal: ReportRefusal{
			Venue: cfg.Venue, Orders: cfg.Orders, OnRefused: cfg.OnRefused,
			OnDropped: cfg.OnDropped, Logger: cfg.Logger,
		},
	}
}

// Run reads the stream until ctx is cancelled or the stream errors. A decode of
// a non-fill frame is ignored; only x=TRADE reports produce FACTs.
func (i *UserDataIngester) Run(ctx context.Context) error {
	// A NEW SESSION HAS PROVED NOTHING YET. The flag is per-session on purpose:
	// one success an hour ago must not excuse a stream that is flapping now.
	i.resolvedThisSession = false
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		raw, err := i.stream.Recv(ctx)
		if err != nil {
			return err
		}
		if err := i.handle(ctx, raw); err != nil {
			return err
		}
	}
}

func (i *UserDataIngester) handle(ctx context.Context, raw []byte) error {
	var rep executionReport
	if json.Unmarshal(raw, &rep) != nil || rep.Event != "executionReport" || rep.ExecType != "TRADE" {
		return nil // not a fill — ignore
	}
	st, ok, lerr := i.orders.Lookup(rep.ClientOrderID)
	if lerr != nil {
		// AN UNREADABLE ORDER VIEW IS NOT "NOT OUR ORDER" (#1047).
		//
		// These two answers were one value. A store failure came back from Lookup
		// as the same (nil, false) an order this adapter does not hold comes back
		// as, and the skip below consumed the report — so a Postgres blip in this
		// adapter deleted real executions from the order.order.filled stream: no
		// FACT, so the position book never books the position and the ledger never
		// journals the cash, and nothing downstream can notice because there is
		// nothing to notice.
		//
		// THE FRAME IS NOT SWALLOWED. Unlike the over-fill refusal below — a
		// standing disagreement about ONE order, where returning an error would
		// hot-loop the reconnect and cost every other order's fills — a store
		// failure is process-wide: every order's Lookup is failing, so there are no
		// other orders' fills to preserve by continuing. Returning ends this
		// websocket run and reconnects, which is the same answer the conversion
		// refusals above give, and it puts the fault where the connector's run loop
		// can see it instead of absorbing it frame by frame.
		//
		// IT DOES NOT QUARANTINE. The freeze states that a specific order is
		// contradicted; here the adapter cannot confirm it even holds the order,
		// the write would go to the store whose read just failed, and a store
		// outage would otherwise freeze the entire in-flight book on a transient
		// fault. See execution.ReportRefusal.Dropped.
		i.refusal.Dropped(rep.ClientOrderID, DropStoreError)
		return fmt.Errorf("binance: order %s: the adapter's order view could not be read, so this "+
			"execution report cannot be resolved to an order and must not be treated as another "+
			"account's: %w", rep.ClientOrderID, lerr)
	}
	if !ok {
		// The view ANSWERED and does not hold this order — not ours, or not yet
		// admitted. Still a skip, and still the right one: a shared exchange
		// account and a second replica both produce these routinely. Counted so
		// the store_error arm above has a baseline to be read against.
		i.refusal.Dropped(rep.ClientOrderID, DropUnknownOrder)
		return nil
	}
	// EVERY NUMBER ON THIS FILL CONVERTS BEFORE ANY OF IT IS PUBLISHED (#94).
	//
	// These become an order.order.filled FACT the ledger and position book fold.
	// parseDec answered an unparseable string with ZERO and wrapped a large one,
	// so a garbled LastPrice published a fill at price 0. Returning the error
	// refuses to publish that instead.
	//
	// WHAT "RETURNING THE ERROR" ACTUALLY DOES, STATED CORRECTLY (#1046). This
	// comment used to say it NACKS the report. It does not, and nothing on this
	// path can: there is no acknowledgement to withhold. handle returns to Run,
	// Run returns to runUserData, which logs "user-data stream ended;
	// reconnecting", grows execution.UserDataBackoff and re-dials. Binance replays
	// NOTHING on a new listenKey stream, so THIS report is gone. The recovery is
	// the reconciler's next pass over Open orders and the OMS sweep, and the sweep
	// only adopts an execution while its order is still non-terminal.
	//
	// The trade is still the right way round — a fill published at price 0 is a
	// wrong number in the ledger, which is worse than a missing one the healing
	// watchdog can re-derive — but it is a trade against a WEAKER recovery than
	// the sentence it replaced claimed, and sizing it wrongly is how a stale
	// comment justifying a trade-off costs something (see the same correction in
	// the OKX ingester).
	//
	// LastQty (l), NOT CumQty (z), and that distinction is load-bearing: the OMS
	// order aggregate ADDS fill.quantity to filled_quantity, so publishing the
	// cumulative here double-counts every partially filled order. Pinned by
	// TestUserData_TwoSequentialPartialsPublishIncrementsNotCumulatives.
	qty, qok := parseDec(rep.LastQty)
	px, pok := parseDec(rep.LastPrice)
	if !qok || !pok {
		return fmt.Errorf("binance: order %s fill is not representable as a Decimal (lastQty=%q lastPrice=%q)",
			rep.ClientOrderID, rep.LastQty, rep.LastPrice)
	}
	feeMoney, feeOK := reportFee(rep)
	if !feeOK {
		return fmt.Errorf("binance: order %s commission %q %s is not representable as a Decimal",
			rep.ClientOrderID, rep.Commission, rep.CommissionAst)
	}
	healed, herr := applyFillToState(st, rep)
	if herr != nil {
		// A REFUSED REPORT IS FROZEN AND COUNTED, NEVER JUST DROPPED (#1045).
		//
		// The report does not become a fill FACT — publishing it would put a
		// quantity the platform never authorised into the position book and the
		// accounting ledger, and on this path nothing downstream would refuse it,
		// because the OMS order aggregate does not consume the fill subject.
		//
		// But dropping it and reading the next frame would trade a wrong number
		// for a missing execution, which is worse: the fund holds a position and
		// no record says so. So the refusal leaves three marks — the counter an
		// alert can watch, an ERROR an operator can read, and a quarantine on
		// this adapter's own view that orderview.Dispatch will refuse to work
		// over. Only then is the frame let go.
		//
		// THE STREAM IS NOT TORN DOWN FOR IT, unlike the conversion refusals
		// above. Those are per-message and a reconnect re-reads venue truth; this
		// one is a standing disagreement about ONE order, so returning an error
		// would drop the websocket — and every other order's fills with it — on
		// every repeat report, in a reconnect loop that fixes nothing.
		i.refusal.Refuse(rep.ClientOrderID, herr)
		return nil
	}
	fill := &orderpb.Fill{
		FillId:           fmt.Sprintf("%s-%d", rep.Symbol, rep.TradeID),
		OrderId:          rep.ClientOrderID,
		InstrumentId:     st.GetInstrumentId(),
		Side:             st.GetSide(),
		Quantity:         qty,
		Price:            px,
		Fee:              feeMoney,
		Venue:            i.venue,
		VenueExecutionId: strconv.FormatInt(rep.TradeID, 10),
		ExecutedAt:       timestamppb.New(time.UnixMilli(rep.TransactTime).UTC()),
	}
	subject := fillfact.SubjectPartiallyFilled
	var payload proto.Message = &orderpb.OrderPartiallyFilled{OrderId: rep.ClientOrderID, Fill: fill, State: healed}
	if rep.OrderStatus == "FILLED" {
		subject = fillfact.SubjectFilled
		payload = &orderpb.OrderFilled{OrderId: rep.ClientOrderID, Fill: fill, State: healed}
	}
	if err := i.pub.Publish(ctx, bus.Event{
		Subject: subject, EventType: subject,
		EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "order",
		EventTime: time.Now().UTC(), PartitionKey: rep.ClientOrderID, TenantID: i.tenant,
		CausationID: rep.ClientOrderID,
		Payload:     payload,
	}); err != nil {
		return err
	}
	// AND NOW THIS ADAPTER'S OWN VIEW AGREES WITH THE FACT IT JUST PUBLISHED (#904).
	//
	// healed carries binanceStatusToProto(rep.OrderStatus) — Binance's OWN X
	// field, so FILLED and PARTIALLY_FILLED are the venue's verdict and not an
	// inference from quantities. A PARTIAL is recorded as PARTIALLY_FILLED, which
	// orderview.Terminal does NOT treat as terminal, so the order stays in Open
	// and the healing watchdog keeps reconciling it. Marking a partially filled
	// order terminal would hide a LIVE order from the watchdog, which is a far
	// worse failure than the unbounded view this closes.
	//
	// AFTER the publish, not before: the view must never claim an order finished
	// on the strength of a FACT that did not reach the bus. A publish error ends
	// the session and re-dials (it does not nack — see the conversion refusal
	// above), and the reconciler re-reads venue truth on its next pass.
	//
	// Progressed MAY REFUSE THIS, and that is by design (#1046): healed carries
	// the venue's own cumulative and event time, and orderview.Progress will not
	// fold in a report older than the last one it recorded. The refusal is
	// counted on kanz_venue_orderview_stale_reports_total and logged; the FACT
	// above is already on the bus and is unaffected, because the fill's quantity
	// is an INCREMENT and downstream dedups on fill_id.
	i.orders.Progressed(healed)
	// THE ONE THING THE RECONNECT BACK-OFF MAY RESET ON (#1047) — see
	// resolvedThisSession.
	i.resolvedThisSession = true
	return nil
}

// applyFillToState folds the report's cumulative fill into a fresh OrderState
// (Kanz static terms + exchange dynamic fields).
//
// It returns an error rather than healing the order in two cases, and they are
// different findings: a quantity that will not convert (#94), and a venue
// cumulative LARGER than the ordered quantity (#1045). The second used to be no
// case at all — leaves came from a bare subtraction that represents a negative
// result and reports success — so a cumulative 14 against an order of 10 healed
// the order to filled 14, leaves −4, and published it. Both are now
// LeavesRemaining's answer, and the caller refuses the whole report.
func applyFillToState(st *orderpb.OrderState, rep executionReport) (*orderpb.OrderState, error) {
	cum, cok := parseDec(rep.CumQty)
	if !cok {
		return nil, fmt.Errorf("binance: order %s cumulative filled quantity %q is not representable as a Decimal",
			rep.ClientOrderID, rep.CumQty)
	}
	ordered := st.GetOrderedQuantity()
	leaves, lerr := leavesRemaining(ordered, cum)
	if lerr != nil {
		return nil, fmt.Errorf("binance: order %s: %w", rep.ClientOrderID, lerr)
	}
	return &orderpb.OrderState{
		OrderId: st.GetOrderId(), PortfolioId: st.GetPortfolioId(), InstrumentId: st.GetInstrumentId(),
		Side: st.GetSide(), OrderType: st.GetOrderType(), TimeInForce: st.GetTimeInForce(),
		OrderedQuantity: ordered, LimitPrice: st.GetLimitPrice(),
		Status:         binanceStatusToProto(rep.OrderStatus),
		FilledQuantity: cum, LeavesQuantity: leaves,
		// THE VENUE'S OWN ORDERING TOKEN, not this process's clock and not the
		// trade time (#1046). orderview.Progress compares it against the token of
		// the last report it folded in and refuses one that is older, so a replay
		// after a reconnect cannot run this order's status, price and quantities
		// backwards. See executionReport.EventTime.
		AsOf: timestamppb.New(reportAsOf(rep)),
	}, nil
}

// reportAsOf is the instant this execution report is EFFECTIVE at — Binance's
// event time E, falling back to the transact time T when a frame carries no E.
//
// ONE FUNCTION BECAUSE THE FALLBACK IS THE WHOLE POINT. Written inline it would
// be written twice, and the copy that forgot the fallback would stamp the view
// with the Unix epoch on any frame without an E — a value every subsequent
// report is newer than, which disables the ordering gate for that order without
// disabling anything visible.
func reportAsOf(rep executionReport) time.Time {
	ms := rep.EventTime
	if ms == 0 {
		ms = rep.TransactTime
	}
	return time.UnixMilli(ms).UTC()
}

// reportFee reads the commission off a fill report. nil Money means NO FEE, so
// ok=false is a separate answer for "there is a commission and it could not be
// read" (#94) — collapsing them would silently drop a real cost.
func reportFee(rep executionReport) (*commonpb.Money, bool) {
	if rep.Commission == "" || rep.Commission == "0" {
		return nil, true
	}
	amt, ok := parseDec(rep.Commission)
	if !ok {
		return nil, false
	}
	return &commonpb.Money{Amount: amt, CurrencyCode: rep.CommissionAst}, true
}

// --- concrete Binance user-data websocket transport (coder/websocket) ---

// binanceUserDataWS maintains the 24/7 user-data websocket: it obtains a
// listenKey via signed REST, dials wss://<base>/ws/<listenKey>, and keeps the
// key alive. Recv returns the next frame; the caller (UserDataIngester) reconnects
// on error.
type binanceUserDataWS struct {
	restBase string
	wsBase   string
	apiKey   string
	httpc    *http.Client
	conn     *websocket.Conn
}

func newBinanceUserDataWS(restBase, wsBase, apiKey string, httpc *http.Client) *binanceUserDataWS {
	if httpc == nil {
		httpc = &http.Client{Timeout: 10 * time.Second}
	}
	return &binanceUserDataWS{restBase: restBase, wsBase: wsBase, apiKey: apiKey, httpc: httpc}
}

// Connect obtains a listenKey and dials the stream. Returns a keepalive stopper.
func (w *binanceUserDataWS) Connect(ctx context.Context) (func(), error) {
	listenKey, err := w.listenKey(ctx)
	if err != nil {
		return nil, err
	}
	conn, _, err := websocket.Dial(ctx, w.wsBase+"/ws/"+listenKey, nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(1 << 20)
	w.conn = conn

	// Keepalive: Binance expires a listenKey after 60m; PUT every 30m.
	kaCtx, cancel := context.WithCancel(ctx)
	go w.keepAlive(kaCtx, listenKey)
	return cancel, nil
}

// Recv reads the next websocket frame.
func (w *binanceUserDataWS) Recv(ctx context.Context) ([]byte, error) {
	if w.conn == nil {
		return nil, errors.New("binance: user-data stream not connected")
	}
	_, data, err := w.conn.Read(ctx)
	return data, err
}

func (w *binanceUserDataWS) listenKey(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.restBase+"/api/v3/userDataStream", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-MBX-APIKEY", w.apiKey)
	resp, err := w.httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		ListenKey string `json:"listenKey"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.ListenKey == "" {
		return "", errors.New("binance: empty listenKey")
	}
	return out.ListenKey, nil
}

func (w *binanceUserDataWS) keepAlive(ctx context.Context, listenKey string) {
	t := time.NewTicker(30 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			req, _ := http.NewRequestWithContext(ctx, http.MethodPut,
				w.restBase+"/api/v3/userDataStream?"+url.Values{"listenKey": {listenKey}}.Encode(), nil)
			req.Header.Set("X-MBX-APIKEY", w.apiKey)
			if resp, err := w.httpc.Do(req); err == nil {
				_ = resp.Body.Close()
			}
		}
	}
}

// resolvedAReport reports whether this session published at least one fill FACT.
// The reconnect loop resets its back-off on this and on nothing else.
func (i *UserDataIngester) resolvedAReport() bool { return i.resolvedThisSession }
