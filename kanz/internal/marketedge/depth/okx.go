package depth

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/exchange/netdial"
)

// OKXSource is the live OKX v5 L2 depth feed — a DepthSource over the public
// `books` channel.
//
// OKX is simpler than Binance here: the channel carries an explicit sequence
// chain (seqId / prevSeqId) and pushes its own initial snapshot on subscribe, so
// there is no REST anchor to reconcile against. The rules are:
//
//   - action=snapshot resets the book (on subscribe, and again after a reconnect).
//   - action=update applies iff prevSeqId == the last seqId we folded.
//   - seqId == prevSeqId means "no change" — skip it rather than re-fold.
//   - Any other prevSeqId is a gap. The source drops the connection and
//     resubscribes, which makes OKX re-push a snapshot: we re-anchor rather than
//     fold out of order, because a silently mis-folded book prices every
//     downstream decision wrong.
//
// The connection needs an application-level keepalive: OKX closes a stream idle
// for 30s, so a "ping" text frame goes out every 20s and the "pong" reply is
// skipped by the reader.
type OKXSource struct {
	instrumentID string
	instID       string // venue instrument id, e.g. BTC-USDT
	mic          string
	wsURL        string
	httpc        *http.Client

	conn        *websocket.Conn
	stopKA      context.CancelFunc
	lastSeq     int64 // last seqId folded
	seeded      bool  // a snapshot has been folded; updates before it are dropped
	lastEventMS int64 // Venue time of the last emitted book, retained across reconnects.
}

// OKXConfig configures an OKXSource.
type OKXConfig struct {
	InstrumentID string // canonical Kanz instrument_id (the partition key)
	InstID       string // venue instrument id (BTC-USDT)
	MIC          string // venue code (default "OKX")
	WSURL        string // public websocket, e.g. wss://ws.okx.com:8443/ws/v5/public
	DNSTTL       time.Duration
	HTTPClient   *http.Client // injected by tests; default is the DNS-bypass client
}

// NewOKXSource builds the live depth source.
func NewOKXSource(cfg OKXConfig) *OKXSource {
	if cfg.MIC == "" {
		cfg.MIC = "OKX"
	}
	if cfg.HTTPClient == nil {
		// Zero-Timeout client: the depth stream is long-lived (see netdial).
		cfg.HTTPClient = netdial.NewWebsocketHTTPClient(cfg.DNSTTL)
	}
	return &OKXSource{
		instrumentID: cfg.InstrumentID, instID: cfg.InstID, mic: cfg.MIC,
		wsURL: cfg.WSURL, httpc: cfg.HTTPClient,
	}
}

var _ DepthSource = (*OKXSource)(nil)

// okxBookMessage is one frame of the `books` channel.
type okxBookMessage struct {
	Event  string          `json:"event"` // "subscribe" / "error" on control frames
	Code   string          `json:"code"`
	Msg    string          `json:"msg"`
	Action string          `json:"action"` // "snapshot" | "update"
	Arg    okxBookArg      `json:"arg"`
	Data   []okxBookLevels `json:"data"`
}

type okxBookArg struct {
	Channel string `json:"channel"`
	InstID  string `json:"instId"`
}

type okxBookLevels struct {
	Bids      [][]string `json:"bids"`
	Asks      [][]string `json:"asks"`
	TS        string     `json:"ts"`
	SeqID     int64      `json:"seqId"`
	PrevSeqID int64      `json:"prevSeqId"`
}

// Recv yields the next depth update, re-anchoring on a fresh snapshot whenever
// the venue sequence chain breaks. It never returns a delta the book cannot fold
// in order.
func (s *OKXSource) Recv(ctx context.Context) (Update, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Update{}, err
		}
		if s.conn == nil {
			if err := s.connect(ctx); err != nil {
				return Update{}, err
			}
		}

		msg, err := s.readBook(ctx)
		if err != nil {
			return Update{}, err
		}
		if msg == nil || len(msg.Data) == 0 {
			continue // control frame (subscribe ack, pong) — not book data
		}
		d := msg.Data[0]

		if msg.Action == "update" {
			switch {
			case !s.seeded, d.SeqID == s.lastSeq:
				continue // waiting for a snapshot, or the venue's no-change frame
			case d.PrevSeqID != s.lastSeq:
				s.reset() // a gap needs a fresh snapshot
				continue
			}
		}

		ms, err := strconv.ParseInt(d.TS, 10, 64)
		ts := timestamppb.New(time.UnixMilli(ms))
		if err != nil || ms <= 0 || !ts.IsValid() {
			s.reset()
			return Update{}, fmt.Errorf("okx depth: invalid venue event time at sequence %d", d.SeqID)
		}
		if ms < s.lastEventMS {
			s.reset()
			return Update{}, fmt.Errorf("okx depth: venue event time regressed from %d to %d at sequence %d", s.lastEventMS, ms, d.SeqID)
		}
		prev := s.lastSeq
		s.lastSeq, s.lastEventMS = d.SeqID, ms
		if msg.Action == "snapshot" {
			s.seeded = true
			return Update{Snapshot: s.snapshotProto(d, ts)}, nil
		}
		return Update{Delta: s.deltaProto(d, prev, ts)}, nil
	}
}

// connect dials the public stream and subscribes to the instrument's book.
func (s *OKXSource) connect(ctx context.Context) error {
	conn, _, err := websocket.Dial(ctx, s.wsURL, &websocket.DialOptions{HTTPClient: s.httpc})
	if err != nil {
		return fmt.Errorf("okx depth: dial: %w", err)
	}
	conn.SetReadLimit(4 << 20)

	sub, _ := json.Marshal(map[string]any{
		"op":   "subscribe",
		"args": []okxBookArg{{Channel: "books", InstID: s.instID}},
	})
	if err := conn.Write(ctx, websocket.MessageText, sub); err != nil {
		_ = conn.Close(websocket.StatusInternalError, "subscribe failed")
		return fmt.Errorf("okx depth: subscribe %s: %w", s.instID, err)
	}
	s.conn = conn
	s.seeded = false // the fresh subscribe re-pushes a snapshot

	// Application-level keepalive: OKX drops a stream idle for 30s.
	kaCtx, cancel := context.WithCancel(ctx)
	s.stopKA = cancel
	go s.keepAlive(kaCtx, conn)
	return nil
}

func (s *OKXSource) keepAlive(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := conn.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
				return // the reader will surface the dead connection
			}
		}
	}
}

// reset tears the connection down so the next Recv reconnects and re-anchors.
func (s *OKXSource) reset() {
	if s.stopKA != nil {
		s.stopKA()
		s.stopKA = nil
	}
	if s.conn != nil {
		_ = s.conn.Close(websocket.StatusNormalClosure, "resubscribing to re-anchor the book")
		s.conn = nil
	}
	s.seeded = false
}

// Close releases the stream.
func (s *OKXSource) Close() error {
	s.reset()
	return nil
}

// readBook reads the next frame. It returns (nil, nil) for a control frame — a
// subscribe ack, an error event, or the "pong" keepalive reply — which the caller
// skips.
func (s *OKXSource) readBook(ctx context.Context) (*okxBookMessage, error) {
	_, data, err := s.conn.Read(ctx)
	if err != nil {
		s.reset() // force a reconnect + re-anchor on the next Recv
		return nil, fmt.Errorf("okx depth: read: %w", err)
	}
	if string(data) == "pong" {
		return nil, nil
	}
	var msg okxBookMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("okx depth: decode frame: %w", err)
	}
	if msg.Event == "error" {
		return nil, fmt.Errorf("okx depth: subscribe rejected %s: %s", msg.Code, msg.Msg)
	}
	if msg.Event != "" {
		return nil, nil // subscribe ack
	}
	if msg.Arg.Channel != "books" || msg.Arg.InstID == "" || msg.Arg.InstID != s.instID {
		s.reset()
		return nil, fmt.Errorf("okx depth: unexpected book identity %q/%q", msg.Arg.Channel, msg.Arg.InstID)
	}
	if msg.Action != "snapshot" && msg.Action != "update" {
		s.reset()
		return nil, fmt.Errorf("okx depth: unknown book action %q", msg.Action)
	}
	return &msg, nil
}

func (s *OKXSource) snapshotProto(d okxBookLevels, ts *timestamppb.Timestamp) *marketpb.OrderBookSnapshot {
	return &marketpb.OrderBookSnapshot{
		InstrumentId:       s.instrumentID,
		Symbol:             s.instID,
		Mic:                s.mic,
		EventTime:          ts,
		LastUpdateSequence: uint64(d.SeqID),
		Bids:               okxLevels(d.Bids),
		Asks:               okxLevels(d.Asks),
	}
}

func (s *OKXSource) deltaProto(d okxBookLevels, prev int64, ts *timestamppb.Timestamp) *marketpb.OrderBookDelta {
	return &marketpb.OrderBookDelta{
		InstrumentId:        s.instrumentID,
		Symbol:              s.instID,
		Mic:                 s.mic,
		EventTime:           ts,
		FirstUpdateSequence: uint64(d.SeqID), // OKX carries no range — one id per frame
		LastUpdateSequence:  uint64(d.SeqID),
		PrevUpdateSequence:  uint64(prev),
		Bids:                okxLevels(d.Bids),
		Asks:                okxLevels(d.Asks),
	}
}

// okxLevels converts OKX ["price","size","liqOrders","numOrders"] tuples to exact
// PriceLevels (only the first two fields are depth). A size of 0 is preserved —
// it is the venue's "remove this level" instruction. Prices/sizes are parsed as
// exact big.Rat: no float ever touches depth.
func okxLevels(raw [][]string) []*marketpb.PriceLevel {
	out := make([]*marketpb.PriceLevel, 0, len(raw))
	for _, lvl := range raw {
		if len(lvl) < 2 {
			continue
		}
		price, okP := new(big.Rat).SetString(lvl[0])
		size, okS := new(big.Rat).SetString(lvl[1])
		if !okP || !okS {
			continue // unparseable level — drop it rather than guess a price
		}
		// SCALED, AND A LEVEL THAT STILL WILL NOT CONVERT IS DROPPED (#94/#189).
		//
		// This is the venue INGRESS, upstream of the book, and the book's read seam
		// hands exact rationals to pkg/alpha — so a wrong number here is not an
		// observability problem, it prices and sizes a real order. dec.ToProto
		// wrapped above roughly 92.2 billion units at scale 8: implausible for a
		// price, ORDINARY for a size, since a depth level holding trillions of
		// tokens is normal on this venue. A wrapped size is worse than a missing
		// one because it is perfectly representable — the refusal added to
		// translate.toDec (#187) cannot catch it, and nothing downstream can tell
		// it from real liquidity.
		//
		// ToProtoScaled converts those levels correctly, so the drop below is only
		// the genuinely unrepresentable residual, and it takes the SAME action as
		// the unparseable case above: a level this code cannot read is a level the
		// book must not carry. That understates depth, which sizes orders small —
		// the safe direction.
		priceD, okPD := dec.ToProtoScaled(price)
		sizeD, okSD := dec.ToProtoScaled(size)
		if !okPD || !okSD {
			continue
		}
		out = append(out, &marketpb.PriceLevel{Price: priceD, Size: sizeD})
	}
	return out
}
