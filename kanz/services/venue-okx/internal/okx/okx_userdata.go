package okx

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/eighred/kanz/internal/fillfact"
	"log/slog"
	"math/big"
	"strconv"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/coder/websocket"

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
//
// THAT LAST SENTENCE WAS UNTRUE FOR AS LONG AS IT STOOD HERE, AND IT COST #923.
// The synchronous path did not mint "<instId>-<tradeId>" — it synthesized ONE
// CUMULATIVE fill per order named "<instId>-<ordId>", a different OKX identifier
// space, so the same execution reached the order aggregate and the position book
// under two names and neither could dedup it. OKXVenue.fills now builds one fill
// per trade from /trade/fills-history and mints the id below, so the two paths
// agree by construction rather than by claim.
type OKXUserDataIngester struct {
	stream UserDataStream
	// orders is the adapter's own order view, READ AND WRITTEN (#904). Read to
	// enrich the push; written so the fill this ingester just published as a FACT
	// is also the fill the adapter believes in — see handle.
	orders OrderTracker
	pub    Publisher
	venue  string
	tenant string
	// refusal is the SHARED answer to an execution report this ingester will not
	// publish (#1045): counter, ERROR log, and a freeze on the adapter's own view.
	// Shared with the Binance connector rather than written out here, because the
	// defect it answers was in both and a per-connector refusal is one that gets
	// improved on one venue.
	refusal ReportRefusal
}

// OKXUserDataConfig configures the ingester. It is a struct rather than the
// positional argument list it replaces for the reason the Binance one is: the
// refusal seam and the logger are the two collaborators a caller can silently
// omit, and a keyed literal makes leaving either out visible in a diff.
type OKXUserDataConfig struct {
	Stream    UserDataStream
	Orders    OrderTracker
	Pub       Publisher
	Venue     string
	Tenant    string
	OnRefused func(mic, orderID, reason string)
	Logger    *slog.Logger
}

func newOKXUserDataIngester(cfg OKXUserDataConfig) *OKXUserDataIngester {
	if cfg.Venue == "" {
		cfg.Venue = "OKX"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &OKXUserDataIngester{
		stream: cfg.Stream, orders: cfg.Orders, pub: cfg.Pub, venue: cfg.Venue, tenant: cfg.Tenant,
		refusal: ReportRefusal{
			Venue: cfg.Venue, Orders: cfg.Orders, OnRefused: cfg.OnRefused, Logger: cfg.Logger,
		},
	}
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
		st, ok := i.orders.Lookup(orderID)
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
		// THE BOUND BETWEEN WHAT WAS ORDERED AND WHAT OKX SAYS IT FILLED (#1045).
		//
		// This was SubDec and a check on its ok, which is not a bound at all: a
		// negative result represents perfectly well and reports success, so an
		// accFillSz of 14 against an order of 10 published an order.order.filled
		// FACT carrying leaves −4 into the position book and the accounting
		// ledger. The OMS order aggregate does not consume that subject, so its
		// OVERFILL refusal was never on this path and nothing else looked.
		//
		// THE WHOLE REPORT IS REFUSED, NOT CLAMPED. Healing leaves to zero would
		// record the order at a size the platform never authorised and destroy the
		// evidence of the disagreement in the same write.
		leaves, lerr := LeavesRemaining(st.GetOrderedQuantity(), accFilled)
		if lerr != nil {
			// Refused, frozen and counted — never a silent drop; see refuse. The
			// loop CONTINUES rather than returning: a returned error tears down the
			// websocket, and this is a standing disagreement about one order, so a
			// reconnect would only re-derive it while every other order's fills go
			// unseen for the duration.
			i.refusal.Refuse(orderID, fmt.Errorf("okx: order %s: %w", orderID, lerr))
			continue
		}
		feeMoney, feeOK := okxFee(d.FillFee, d.FillFeeCcy)
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
		subject := fillfact.SubjectPartiallyFilled
		var payload proto.Message = &orderpb.OrderPartiallyFilled{OrderId: orderID, Fill: fill, State: healed}
		if d.State == "filled" {
			subject = fillfact.SubjectFilled
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
		// AND NOW THIS ADAPTER'S OWN VIEW AGREES WITH THE FACT IT JUST PUBLISHED (#904).
		//
		// healed carries okxStateToProto(d.State) — OKX's OWN state field, so
		// "filled" and "partially_filled" are the venue's verdict and not an
		// inference from accFillSz. A PARTIAL is recorded as PARTIALLY_FILLED,
		// which orderview.Terminal does NOT treat as terminal, so the order stays
		// in Open and the healing watchdog keeps reconciling it. Marking a
		// partially filled order terminal would hide a LIVE order from the
		// watchdog, which is a far worse failure than the unbounded view this
		// closes.
		//
		// AFTER the publish, not before: the view must never claim an order
		// finished on the strength of a FACT that did not reach the bus.
		i.orders.Progressed(healed)
	}
	return nil
}

// okxStateToProto is the ONE table turning an OKX order state into an order.v1
// status. The healing path off the private websocket reads it, and — through
// okxStateWithdrawn — so does the Querier that answers the OMS's crash recovery.
//
// ONE TABLE, DELIBERATELY (#924). A second one inside the query path is how the
// two would come to disagree about what "canceled" means, or how one of them
// would learn a state the other did not.
func okxStateToProto(s string) orderpb.OrderStatus {
	switch s {
	case "live":
		return orderpb.OrderStatus_ORDER_STATUS_ROUTED
	case "partially_filled":
		return orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED
	case "filled":
		return orderpb.OrderStatus_ORDER_STATUS_FILLED
	case "canceled", "mmp_canceled":
		// mmp_canceled is MARKET MAKER PROTECTION pulling the order. It is as
		// terminal and as withdrawn as an ordinary cancel — the order is gone from
		// the book and will not trade again — and it used to fall through to
		// UNSPECIFIED here, so the healing path could not name it either.
		return orderpb.OrderStatus_ORDER_STATUS_CANCELLED
	default:
		return orderpb.OrderStatus_ORDER_STATUS_UNSPECIFIED
	}
}

func uTime(ms string) time.Time { return okxMillis(ms, time.Now) }

// okxMillis reads an OKX millisecond epoch string. fallback supplies the instant
// when OKX sent nothing this connector can read — the venue's own clock is
// always preferred, because a fill adopted on the RECOVERY path is timestamped
// hours after it traded otherwise, in every execution-quality measurement that
// reads it. Injectable rather than time.Now so the connector's clock seam
// reaches here too.
func okxMillis(ms string, fallback func() time.Time) time.Time {
	n, err := strconv.ParseInt(ms, 10, 64)
	if err != nil || n <= 0 {
		return fallback().UTC()
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
