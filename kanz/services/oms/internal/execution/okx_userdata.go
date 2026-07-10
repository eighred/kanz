//go:build okx

package execution

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
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

// okxOrdersMsg is a private "orders"-channel push. Each data element is an order
// state change; a fill carries a non-zero fillSz (the last execution).
type okxOrdersMsg struct {
	Arg struct {
		Channel string `json:"channel"`
	} `json:"arg"`
	Data []struct {
		InstID     string `json:"instId"`
		OrdID      string `json:"ordId"`
		ClOrdID    string `json:"clOrdId"`
		State      string `json:"state"`
		FillSz     string `json:"fillSz"`
		FillPx     string `json:"fillPx"`
		AccFillSz  string `json:"accFillSz"`
		TradeID    string `json:"tradeId"`
		FillFee    string `json:"fillFee"`
		FillFeeCcy string `json:"fillFeeCcy"`
		UTime      string `json:"uTime"`
	} `json:"data"`
}

// OKXUserDataIngester reads the OKX private orders channel and converts each
// fill into an order.v1.OrderFilled / OrderPartiallyFilled FACT — the OKX
// analog of the Binance user-data ingester. fill_id is deterministic
// (instId-tradeId), identical to the OKX synchronous venue path, so downstream
// folds dedup the two.
type OKXUserDataIngester struct {
	stream UserDataStream
	lookup OrderLookup
	pub    Publisher
	venue  string
	tenant string
}

func newOKXUserDataIngester(stream UserDataStream, lookup OrderLookup, pub Publisher, venue, tenant string) *OKXUserDataIngester {
	if venue == "" {
		venue = "OKX"
	}
	return &OKXUserDataIngester{stream: stream, lookup: lookup, pub: pub, venue: venue, tenant: tenant}
}

// Run reads the stream until ctx is cancelled or the stream errors.
func (i *OKXUserDataIngester) Run(ctx context.Context) error {
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

func (i *OKXUserDataIngester) handle(ctx context.Context, raw []byte) error {
	var msg okxOrdersMsg
	if json.Unmarshal(raw, &msg) != nil || msg.Arg.Channel != "orders" {
		return nil
	}
	for _, d := range msg.Data {
		fsz, ok := new(big.Rat).SetString(d.FillSz)
		if !ok || fsz.Sign() <= 0 {
			continue // not a fill (state change only)
		}
		st, ok := i.lookup.Lookup(d.ClOrdID)
		if !ok {
			continue
		}
		fill := &orderpb.Fill{
			FillId:           d.InstID + "-" + d.TradeID,
			OrderId:          d.ClOrdID,
			InstrumentId:     st.GetInstrumentId(),
			Side:             st.GetSide(),
			Quantity:         parseDec(d.FillSz),
			Price:            parseDec(d.FillPx),
			Fee:              okxWSFee(d.FillFee, d.FillFeeCcy),
			Venue:            i.venue,
			VenueExecutionId: d.TradeID,
			ExecutedAt:       timestamppb.New(uTime(d.UTime)),
		}
		healed := &orderpb.OrderState{
			OrderId: st.GetOrderId(), PortfolioId: st.GetPortfolioId(), InstrumentId: st.GetInstrumentId(),
			Side: st.GetSide(), OrderType: st.GetOrderType(), TimeInForce: st.GetTimeInForce(),
			OrderedQuantity: st.GetOrderedQuantity(), LimitPrice: st.GetLimitPrice(),
			Status:         okxStateToProto(d.State),
			FilledQuantity: parseDec(d.AccFillSz),
			LeavesQuantity: subDec(st.GetOrderedQuantity(), parseDec(d.AccFillSz)),
			Venue:          i.venue,
			AsOf:           timestamppb.New(uTime(d.UTime)),
		}
		subject := "order.order.partially_filled"
		var payload proto.Message = &orderpb.OrderPartiallyFilled{OrderId: d.ClOrdID, Fill: fill, State: healed}
		if d.State == "filled" {
			subject = "order.order.filled"
			payload = &orderpb.OrderFilled{OrderId: d.ClOrdID, Fill: fill, State: healed}
		}
		if err := i.pub.Publish(ctx, bus.Event{
			Subject: subject, EventType: subject,
			EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "order",
			EventTime: time.Now().UTC(), PartitionKey: d.ClOrdID, TenantID: i.tenant,
			CausationID: d.ClOrdID, Payload: payload,
		}); err != nil {
			return err
		}
	}
	return nil
}

func okxWSFee(fee, ccy string) *commonpb.Money {
	if fee == "" || fee == "0" {
		return nil
	}
	amt := parseDec(fee)
	if r := dec.FromProto(amt); r.Sign() < 0 {
		amt = dec.ToProto(r.Neg(r))
	}
	return &commonpb.Money{Amount: amt, CurrencyCode: ccy}
}

func okxStateToProto(s string) orderpb.OrderStatus {
	switch s {
	case "live":
		return orderpb.OrderStatus_ORDER_STATUS_ROUTED
	case "partially_filled":
		return orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED
	case "filled":
		return orderpb.OrderStatus_ORDER_STATUS_FILLED
	case "canceled":
		return orderpb.OrderStatus_ORDER_STATUS_CANCELLED
	default:
		return orderpb.OrderStatus_ORDER_STATUS_UNSPECIFIED
	}
}

func uTime(ms string) time.Time {
	n, err := strconv.ParseInt(ms, 10, 64)
	if err != nil {
		return time.Now().UTC()
	}
	return time.UnixMilli(n).UTC()
}

// --- OKX private websocket transport (coder/websocket) ---

// okxUserDataWS logs in to the OKX v5 private websocket and subscribes to the
// orders channel; Recv returns the next frame. The ingester reconnects on error.
type okxUserDataWS struct {
	wsURL      string
	apiKey     string
	apiSecret  []byte
	passphrase string
	now        func() time.Time
	conn       *websocket.Conn
}

func newOKXUserDataWS(wsURL, apiKey, apiSecret, passphrase string) *okxUserDataWS {
	return &okxUserDataWS{wsURL: wsURL, apiKey: apiKey, apiSecret: []byte(apiSecret), passphrase: passphrase, now: time.Now}
}

// Connect dials, authenticates (login), and subscribes. Returns a keepalive
// stopper.
func (w *okxUserDataWS) Connect(ctx context.Context) (func(), error) {
	conn, _, err := websocket.Dial(ctx, w.wsURL, nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(1 << 20)
	w.conn = conn

	ts := strconv.FormatInt(w.now().Unix(), 10)
	mac := hmac.New(sha256.New, w.apiSecret)
	mac.Write([]byte(ts + "GET" + "/users/self/verify"))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	login, _ := json.Marshal(map[string]any{"op": "login", "args": []map[string]string{
		{"apiKey": w.apiKey, "passphrase": w.passphrase, "timestamp": ts, "sign": sign},
	}})
	if err := conn.Write(ctx, websocket.MessageText, login); err != nil {
		return nil, err
	}
	if _, _, err := conn.Read(ctx); err != nil { // login ack
		return nil, err
	}
	sub, _ := json.Marshal(map[string]any{"op": "subscribe", "args": []map[string]string{
		{"channel": "orders", "instType": "SPOT"},
	}})
	if err := conn.Write(ctx, websocket.MessageText, sub); err != nil {
		return nil, err
	}

	kaCtx, cancel := context.WithCancel(ctx)
	go w.keepAlive(kaCtx)
	return cancel, nil
}

// Recv reads the next frame, skipping OKX heartbeat "pong" frames.
func (w *okxUserDataWS) Recv(ctx context.Context) ([]byte, error) {
	if w.conn == nil {
		return nil, errors.New("okx: user-data stream not connected")
	}
	for {
		_, data, err := w.conn.Read(ctx)
		if err != nil {
			return nil, err
		}
		if string(data) == "pong" {
			continue
		}
		return data, nil
	}
}

func (w *okxUserDataWS) keepAlive(ctx context.Context) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = w.conn.Write(ctx, websocket.MessageText, []byte("ping"))
		}
	}
}
