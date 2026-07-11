//go:build okx

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
	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/internal/exchange/netdial"
	"github.com/kanz-eng/kanz/services/market-ingest/internal/dec"
)

// OKXSource is the live OKX v5 L2 depth feed — a DepthSource over the public
// `books` channel, compiled only under -tags okx so the default binary links no
// websocket.
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

	conn     *websocket.Conn
	stopKA   context.CancelFunc
	lastSeq  int64 // last seqId folded
	seeded   bool  // a snapshot has been folded; updates before it are dropped
	depthCap int
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

		if msg.Action == "snapshot" {
			s.lastSeq, s.seeded = d.SeqID, true
			return Update{Snapshot: s.snapshotProto(d)}, nil
		}
		// action == "update"
		switch {
		case !s.seeded:
			continue // an update before the snapshot — nothing to fold onto
		case d.SeqID == s.lastSeq:
			continue // OKX signals "no change in depth" by repeating the seqId
		case d.PrevSeqID != s.lastSeq:
			// A hole in the chain. Reconnect to force a fresh snapshot rather than
			// fold out of order.
			s.reset()
			continue
		}

		prev := s.lastSeq
		s.lastSeq = d.SeqID
		return Update{Delta: s.deltaProto(d, prev)}, nil
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
	return &msg, nil
}

func (s *OKXSource) snapshotProto(d okxBookLevels) *marketpb.OrderBookSnapshot {
	return &marketpb.OrderBookSnapshot{
		InstrumentId:       s.instrumentID,
		Symbol:             s.instID,
		Mic:                s.mic,
		EventTime:          timestamppb.New(okxTime(d.TS)),
		LastUpdateSequence: uint64(d.SeqID),
		Bids:               okxLevels(d.Bids),
		Asks:               okxLevels(d.Asks),
	}
}

func (s *OKXSource) deltaProto(d okxBookLevels, prev int64) *marketpb.OrderBookDelta {
	return &marketpb.OrderBookDelta{
		InstrumentId:        s.instrumentID,
		Symbol:              s.instID,
		Mic:                 s.mic,
		EventTime:           timestamppb.New(okxTime(d.TS)),
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
		out = append(out, &marketpb.PriceLevel{Price: dec.ToProto(price), Size: dec.ToProto(size)})
	}
	return out
}

// okxTime parses the venue's millisecond epoch string. An unparseable ts falls
// back to now — the sequence chain, not the clock, is what orders the fold.
func okxTime(ts string) time.Time {
	ms, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || ms <= 0 {
		return time.Now().UTC()
	}
	return time.UnixMilli(ms).UTC()
}
