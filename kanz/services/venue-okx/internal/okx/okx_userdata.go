package okx

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/coder/websocket"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/bus"
)

// okxOrdersMsg is a private "orders"-channel push. Each data element is an order
// state change; a fill carries a non-zero fillSz (the last execution).
type okxOrdersMsg struct {
	Arg struct {
		Channel string `json:"channel"`
	} `json:"arg"`
	Data []struct {
		InstID  string `json:"instId"`
		OrdID   string `json:"ordId"`
		ClOrdID string `json:"clOrdId"`
		// AlgoClOrdID IS OUR ID WHEN THE ORDER CAME FROM A TRIGGER (#485).
		//
		// A conditional or trigger order placed on /trade/order-algo does not
		// become the order that fills — it CREATES one when it fires, and OKX
		// gives that order a clOrdId OF ITS OWN, prefixed "O". Ours is not lost:
		// it rides on algoClOrdId. Measured against the demo API on 2026-08-15 by
		// firing a real trigger and reading the order back:
		//
		//	ordId        3833848820050333696
		//	clOrdId      O3833848819892766720   <- OKX's, not ours
		//	algoClOrdId  kanzfire1786759936     <- ours
		//
		// Without this field the fill would arrive keyed by an id this platform has
		// never seen, Lookup would miss, and a real execution would go unbooked
		// while the venue reported success.
		AlgoClOrdID string `json:"algoClOrdId"`
		State       string `json:"state"`
		FillSz      string `json:"fillSz"`
		FillPx      string `json:"fillPx"`
		AccFillSz   string `json:"accFillSz"`
		TradeID     string `json:"tradeId"`
		FillFee     string `json:"fillFee"`
		FillFeeCcy  string `json:"fillFeeCcy"`
		UTime       string `json:"uTime"`
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
		// OUR ID, WHICHEVER FIELD CARRIES IT. algoClOrdId when the order was
		// created by a trigger firing, clOrdId when we placed it directly. Never
		// both, and never OKX's own generated clOrdId — see AlgoClOrdID above.
		orderID := d.ClOrdID
		if d.AlgoClOrdID != "" {
			orderID = d.AlgoClOrdID
		}
		st, ok := i.lookup.Lookup(orderID)
		if !ok {
			continue
		}
		// EVERY NUMBER ON THIS FILL IS CONVERTED BEFORE ANY OF IT IS PUBLISHED (#94).
		//
		// These become an order.order.filled FACT the ledger and the position book
		// fold. ParseDec used to answer an unparseable string with ZERO and to wrap
		// a large one, so a garbled FillPx published a fill at price 0 and a
		// trillion-unit AccFillSz published 77662796314.5224192. Returning the error
		// nacks the websocket message instead; the reconciler re-reads venue truth.
		qty, qok := ParseDec(d.FillSz)
		px, pok := ParseDec(d.FillPx)
		accFilled, aok := ParseDec(d.AccFillSz)
		if !qok || !pok || !aok {
			return fmt.Errorf("okx: order %s fill is not representable as a Decimal "+
				"(fillSz=%q fillPx=%q accFillSz=%q)", orderID, d.FillSz, d.FillPx, d.AccFillSz)
		}
		leaves, lok := SubDec(st.GetOrderedQuantity(), accFilled)
		if !lok {
			return fmt.Errorf("okx: order %s leaves quantity is not representable as a Decimal", orderID)
		}
		feeMoney, feeOK := okxWSFee(d.FillFee, d.FillFeeCcy)
		if !feeOK {
			return fmt.Errorf("okx: order %s fee %q %s is not representable as a Decimal",
				orderID, d.FillFee, d.FillFeeCcy)
		}
		fill := &orderpb.Fill{
			FillId:           d.InstID + "-" + d.TradeID,
			OrderId:          orderID,
			InstrumentId:     st.GetInstrumentId(),
			Side:             st.GetSide(),
			Quantity:         qty,
			Price:            px,
			Fee:              feeMoney,
			Venue:            i.venue,
			VenueExecutionId: d.TradeID,
			ExecutedAt:       timestamppb.New(uTime(d.UTime)),
		}
		healed := &orderpb.OrderState{
			OrderId: st.GetOrderId(), PortfolioId: st.GetPortfolioId(), InstrumentId: st.GetInstrumentId(),
			Side: st.GetSide(), OrderType: st.GetOrderType(), TimeInForce: st.GetTimeInForce(),
			OrderedQuantity: st.GetOrderedQuantity(), LimitPrice: st.GetLimitPrice(),
			Status:         okxStateToProto(d.State),
			FilledQuantity: accFilled,
			LeavesQuantity: leaves,
			Venue:          i.venue,
			AsOf:           timestamppb.New(uTime(d.UTime)),
		}
		subject := "order.order.partially_filled"
		var payload proto.Message = &orderpb.OrderPartiallyFilled{OrderId: orderID, Fill: fill, State: healed}
		if d.State == "filled" {
			subject = "order.order.filled"
			payload = &orderpb.OrderFilled{OrderId: orderID, Fill: fill, State: healed}
		}
		if err := i.pub.Publish(ctx, bus.Event{
			Subject: subject, EventType: subject,
			EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "order",
			EventTime: time.Now().UTC(), PartitionKey: orderID, TenantID: i.tenant,
			CausationID: orderID, Payload: payload,
		}); err != nil {
			return err
		}
	}
	return nil
}

// okxWSFee reads the fee off a user-data fill. nil Money means NO FEE, so
// ok=false is a separate answer for "there is a fee and it could not be read"
// (#94) — collapsing them would silently drop a real cost.
func okxWSFee(fee, ccy string) (*commonpb.Money, bool) {
	if fee == "" || fee == "0" {
		return nil, true
	}
	amt, ok := ParseDec(fee)
	if !ok {
		return nil, false
	}
	if r := dec.FromProto(amt); r.Sign() < 0 {
		neg, nok := dec.ToProtoScaled(r.Neg(r))
		if !nok {
			return nil, false
		}
		amt = neg
	}
	return &commonpb.Money{Amount: amt, CurrencyCode: ccy}, true
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
