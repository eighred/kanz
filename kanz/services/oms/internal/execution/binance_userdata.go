//go:build binance

package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/coder/websocket"

	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/services/oms/internal/dec"
)

// OrderLookup enriches an exchange execution report (which carries only the
// clientOrderId = our order_id, and the symbol) with Kanz's order context —
// portfolio_id, instrument_id, static terms — so the ingester can emit a proper
// order.v1 FACT. Bound to the OMS order store in production.
type OrderLookup interface {
	Lookup(orderID string) (*orderpb.OrderState, bool)
}

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
	lookup OrderLookup
	pub    Publisher
	venue  string
	tenant string
}

// UserDataStream is the transport seam: it yields raw user-data frames. The
// concrete Binance websocket implementation is binanceUserDataWS; tests inject a
// fake, so the conversion is certified without a network.
type UserDataStream interface {
	Recv(ctx context.Context) ([]byte, error)
}

// UserDataConfig configures the ingester.
type UserDataConfig struct {
	Stream UserDataStream
	Lookup OrderLookup
	Pub    Publisher
	Venue  string
	Tenant string
}

func newUserDataIngester(cfg UserDataConfig) *UserDataIngester {
	if cfg.Venue == "" {
		cfg.Venue = "BINANCE"
	}
	return &UserDataIngester{stream: cfg.Stream, lookup: cfg.Lookup, pub: cfg.Pub, venue: cfg.Venue, tenant: cfg.Tenant}
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
	st, ok := i.lookup.Lookup(rep.ClientOrderID)
	if !ok {
		return nil // unknown order (not ours / not yet admitted) — skip
	}
	fill := &orderpb.Fill{
		FillId:           fmt.Sprintf("%s-%d", rep.Symbol, rep.TradeID),
		OrderId:          rep.ClientOrderID,
		InstrumentId:     st.GetInstrumentId(),
		Side:             st.GetSide(),
		Quantity:         parseDec(rep.LastQty),
		Price:            parseDec(rep.LastPrice),
		Fee:              reportFee(rep),
		Venue:            i.venue,
		VenueExecutionId: strconv.FormatInt(rep.TradeID, 10),
		ExecutedAt:       timestamppb.New(time.UnixMilli(rep.TransactTime).UTC()),
	}
	healed := applyFillToState(st, rep)

	subject := "order.order.partially_filled"
	var payload proto.Message = &orderpb.OrderPartiallyFilled{OrderId: rep.ClientOrderID, Fill: fill, State: healed}
	if rep.OrderStatus == "FILLED" {
		subject = "order.order.filled"
		payload = &orderpb.OrderFilled{OrderId: rep.ClientOrderID, Fill: fill, State: healed}
	}
	return i.pub.Publish(ctx, bus.Event{
		Subject: subject, EventType: subject,
		EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "order",
		EventTime: time.Now().UTC(), PartitionKey: rep.ClientOrderID, TenantID: i.tenant,
		CausationID: rep.ClientOrderID,
		Payload:     payload,
	})
}

// applyFillToState folds the report's cumulative fill into a fresh OrderState
// (Kanz static terms + exchange dynamic fields).
func applyFillToState(st *orderpb.OrderState, rep executionReport) *orderpb.OrderState {
	cum := parseDec(rep.CumQty)
	ordered := st.GetOrderedQuantity()
	return &orderpb.OrderState{
		OrderId: st.GetOrderId(), PortfolioId: st.GetPortfolioId(), InstrumentId: st.GetInstrumentId(),
		Side: st.GetSide(), OrderType: st.GetOrderType(), TimeInForce: st.GetTimeInForce(),
		OrderedQuantity: ordered, LimitPrice: st.GetLimitPrice(),
		Status:         binanceStatusToProto(rep.OrderStatus),
		FilledQuantity: cum, LeavesQuantity: subDec(ordered, cum),
		AsOf: timestamppb.New(time.UnixMilli(rep.TransactTime).UTC()),
	}
}

// subDec returns a − b as an exact common.v1.Decimal.
func subDec(a, b *commonpb.Decimal) *commonpb.Decimal {
	return dec.ToProto(new(big.Rat).Sub(dec.FromProto(a), dec.FromProto(b)))
}

func reportFee(rep executionReport) *commonpb.Money {
	if rep.Commission == "" || rep.Commission == "0" {
		return nil
	}
	return &commonpb.Money{Amount: parseDec(rep.Commission), CurrencyCode: rep.CommissionAst}
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
