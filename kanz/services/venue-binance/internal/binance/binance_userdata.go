package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/eighred/kanz/internal/fillfact"
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
}

// UserDataConfig configures the ingester.
type UserDataConfig struct {
	Stream UserDataStream
	Orders OrderTracker
	Pub    Publisher
	Venue  string
	Tenant string
}

func newUserDataIngester(cfg UserDataConfig) *UserDataIngester {
	if cfg.Venue == "" {
		cfg.Venue = "BINANCE"
	}
	return &UserDataIngester{stream: cfg.Stream, orders: cfg.Orders, pub: cfg.Pub, venue: cfg.Venue, tenant: cfg.Tenant}
}

// Run reads the stream until ctx is cancelled or the stream errors. A decode of
// a non-fill frame is ignored; only x=TRADE reports produce FACTs.
func (i *UserDataIngester) Run(ctx context.Context) error {
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
	st, ok := i.orders.Lookup(rep.ClientOrderID)
	if !ok {
		return nil // unknown order (not ours / not yet admitted) — skip
	}
	// EVERY NUMBER ON THIS FILL CONVERTS BEFORE ANY OF IT IS PUBLISHED (#94).
	//
	// These become an order.order.filled FACT the ledger and position book fold.
	// parseDec answered an unparseable string with ZERO and wrapped a large one,
	// so a garbled LastPrice published a fill at price 0. Returning the error nacks
	// the report instead; the reconciler re-reads venue truth.
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
	healed, hok := applyFillToState(st, rep)
	if !hok {
		return fmt.Errorf("binance: order %s healed quantities are not representable as a Decimal (cumQty=%q)",
			rep.ClientOrderID, rep.CumQty)
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
	// on the strength of a FACT that did not reach the bus. A publish error nacks
	// the report and the reconciler re-reads venue truth.
	i.orders.Progressed(healed)
	return nil
}

// applyFillToState folds the report's cumulative fill into a fresh OrderState
// (Kanz static terms + exchange dynamic fields). ok=false when a quantity will
// not convert (#94) — the caller refuses the whole report rather than healing the
// order to a size nothing traded.
func applyFillToState(st *orderpb.OrderState, rep executionReport) (*orderpb.OrderState, bool) {
	cum, cok := parseDec(rep.CumQty)
	if !cok {
		return nil, false
	}
	ordered := st.GetOrderedQuantity()
	leaves, lok := subDec(ordered, cum)
	if !lok {
		return nil, false
	}
	return &orderpb.OrderState{
		OrderId: st.GetOrderId(), PortfolioId: st.GetPortfolioId(), InstrumentId: st.GetInstrumentId(),
		Side: st.GetSide(), OrderType: st.GetOrderType(), TimeInForce: st.GetTimeInForce(),
		OrderedQuantity: ordered, LimitPrice: st.GetLimitPrice(),
		Status:         binanceStatusToProto(rep.OrderStatus),
		FilledQuantity: cum, LeavesQuantity: leaves,
		AsOf: timestamppb.New(time.UnixMilli(rep.TransactTime).UTC()),
	}, true
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
