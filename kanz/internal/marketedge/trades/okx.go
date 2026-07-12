package trades

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"github.com/kanz-eng/kanz/internal/exchange/netdial"
)

// OKXSource is the live OKX v5 trade tape — a TradeSource over the public
// `trades` channel, compiled only under -tags okx so the default binary links no
// websocket.
//
// OKX is simpler than Binance here: it reports the aggressor directly as
// side="buy"/"sell" (the TAKER side), so there is no maker/taker inversion to get
// wrong. An unrecognised side folds as SideUnknown and contributes to neither
// volume — never guessed.
type OKXSource struct {
	instID string
	wsURL  string
	httpc  *http.Client

	conn    *websocket.Conn
	stopKA  context.CancelFunc
	pending []Trade // one frame can carry several executions
}

// OKXConfig configures an OKXSource.
type OKXConfig struct {
	InstID     string // venue instrument id (BTC-USDT)
	WSURL      string // public websocket, e.g. wss://ws.okx.com:8443/ws/v5/public
	DNSTTL     time.Duration
	HTTPClient *http.Client // injected by tests; default is the DNS-bypass client
}

// NewOKXSource builds the live trade source.
func NewOKXSource(cfg OKXConfig) *OKXSource {
	if cfg.HTTPClient == nil {
		// Zero-Timeout client: the trade stream is long-lived (see netdial).
		cfg.HTTPClient = netdial.NewWebsocketHTTPClient(cfg.DNSTTL)
	}
	return &OKXSource{instID: cfg.InstID, wsURL: cfg.WSURL, httpc: cfg.HTTPClient}
}

var _ TradeSource = (*OKXSource)(nil)

type okxTradeArg struct {
	Channel string `json:"channel"`
	InstID  string `json:"instId"`
}

type okxTradeMessage struct {
	Event string      `json:"event"`
	Code  string      `json:"code"`
	Msg   string      `json:"msg"`
	Arg   okxTradeArg `json:"arg"`
	Data  []okxTrade  `json:"data"`
}

type okxTrade struct {
	InstID  string `json:"instId"`
	TradeID string `json:"tradeId"`
	Px      string `json:"px"`
	Sz      string `json:"sz"`
	Side    string `json:"side"` // the TAKER side: "buy" | "sell"
	TS      string `json:"ts"`
}

// Recv yields the next trade. One websocket frame may carry several executions,
// so they are buffered and drained one at a time.
func (s *OKXSource) Recv(ctx context.Context) (Trade, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Trade{}, err
		}
		if len(s.pending) > 0 {
			t := s.pending[0]
			s.pending = s.pending[1:]
			return t, nil
		}
		if s.conn == nil {
			if err := s.connect(ctx); err != nil {
				return Trade{}, err
			}
		}
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			s.reset()
			return Trade{}, fmt.Errorf("okx trades: read: %w", err)
		}
		if string(data) == "pong" {
			continue
		}
		var msg okxTradeMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return Trade{}, fmt.Errorf("okx trades: decode: %w", err)
		}
		if msg.Event == "error" {
			return Trade{}, fmt.Errorf("okx trades: subscribe rejected %s: %s", msg.Code, msg.Msg)
		}
		if msg.Event != "" {
			continue // subscribe ack
		}
		for _, d := range msg.Data {
			price, okP := new(big.Rat).SetString(d.Px)
			size, okS := new(big.Rat).SetString(d.Sz)
			if !okP || !okS {
				continue // unparseable — drop rather than guess
			}
			s.pending = append(s.pending, Trade{
				Price: price, Size: size,
				TakerSide: okxSide(d.Side),
				EventTime: okxTime(d.TS),
			})
		}
	}
}

func (s *OKXSource) connect(ctx context.Context) error {
	conn, _, err := websocket.Dial(ctx, s.wsURL, &websocket.DialOptions{HTTPClient: s.httpc})
	if err != nil {
		return fmt.Errorf("okx trades: dial: %w", err)
	}
	conn.SetReadLimit(1 << 20)

	sub, _ := json.Marshal(map[string]any{
		"op":   "subscribe",
		"args": []okxTradeArg{{Channel: "trades", InstID: s.instID}},
	})
	if err := conn.Write(ctx, websocket.MessageText, sub); err != nil {
		_ = conn.Close(websocket.StatusInternalError, "subscribe failed")
		return fmt.Errorf("okx trades: subscribe %s: %w", s.instID, err)
	}
	s.conn = conn

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
				return // the reader surfaces the dead connection
			}
		}
	}
}

func (s *OKXSource) reset() {
	if s.stopKA != nil {
		s.stopKA()
		s.stopKA = nil
	}
	if s.conn != nil {
		_ = s.conn.Close(websocket.StatusNormalClosure, "")
		s.conn = nil
	}
}

// Close releases the stream.
func (s *OKXSource) Close() error {
	s.reset()
	return nil
}

// okxSide maps the venue's taker side. An unrecognised value is UNKNOWN and
// contributes to neither volume — never guessed into a direction.
func okxSide(s string) Side {
	switch s {
	case "buy":
		return SideBuy
	case "sell":
		return SideSell
	default:
		return SideUnknown
	}
}

// okxTime parses the venue's millisecond epoch string.
func okxTime(ts string) time.Time {
	ms, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || ms <= 0 {
		return time.Now().UTC()
	}
	return time.UnixMilli(ms).UTC()
}
