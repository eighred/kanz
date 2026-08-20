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

	"github.com/eighred/kanz/internal/exchange/netdial"
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

	// obs is the ingestion-coverage seam (#591). Nil means nothing is attested
	// and every interval this feed covers reads downstream as UNKNOWN.
	obs       Liveness
	heartbeat time.Duration
}

// OKXConfig configures an OKXSource.
type OKXConfig struct {
	InstID     string // venue instrument id (BTC-USDT)
	WSURL      string // public websocket, e.g. wss://ws.okx.com:8443/ws/v5/public
	DNSTTL     time.Duration
	HTTPClient *http.Client // injected by tests; default is the DNS-bypass client
	// Liveness receives this subscription's coverage attestations. Optional; nil
	// attests nothing, and the intervals read as UNKNOWN rather than as covered.
	Liveness Liveness
	// HeartbeatInterval overrides the keepalive cadence. <=0 ⇒ HeartbeatInterval.
	// A TEST SEAM, not an operational knob — see BinanceConfig.
	HeartbeatInterval time.Duration
}

// NewOKXSource builds the live trade source.
func NewOKXSource(cfg OKXConfig) *OKXSource {
	if cfg.HTTPClient == nil {
		// Zero-Timeout client: the trade stream is long-lived (see netdial).
		cfg.HTTPClient = netdial.NewWebsocketHTTPClient(cfg.DNSTTL)
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = HeartbeatInterval
	}
	return &OKXSource{
		instID: cfg.InstID, wsURL: cfg.WSURL, httpc: cfg.HTTPClient,
		obs: cfg.Liveness, heartbeat: cfg.HeartbeatInterval,
	}
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
				s.reportDown(err)
				return Trade{}, err
			}
		}
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			s.reset()
			err = fmt.Errorf("okx trades: read: %w", err)
			s.reportDown(err)
			return Trade{}, err
		}
		// A FRAME IS A FRAME (#591). The keepalive pong two lines down, and the
		// subscribe ack below it, are the evidence that matters: they arrive
		// whether or not anything traded, which is what lets a QUIET minute be
		// attested as observed rather than as a hole. Reporting liveness only for
		// executions would tie coverage to trading activity and re-derive the
		// ambiguity the record exists to resolve.
		s.reportLive()
		if string(data) == "pong" {
			continue
		}
		var msg okxTradeMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return Trade{}, fmt.Errorf("okx trades: decode: %w", err)
		}
		if msg.Event == "error" {
			err := fmt.Errorf("okx trades: subscribe rejected %s: %s", msg.Code, msg.Msg)
			// A REJECTED SUBSCRIBE IS A BREAK, not a live socket. The connection is
			// up and answering, so the frame above looked like liveness — but this
			// feed is delivering nothing, and attesting coverage for it would vouch
			// for a subscription the venue refused.
			s.reportDown(err)
			return Trade{}, err
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
	// HeartbeatInterval, not a local 20s any more: the coverage recorder's
	// silence tolerance is one value for every feed, so the cadence that decides
	// whether a quiet minute is attestable has to be one value too (#591).
	t := time.NewTicker(s.heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := conn.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
				// NOT REPORTED AS Down, and NOT reported as Live either. The write
				// succeeding would only prove the local send buffer accepted it; the
				// evidence is the PONG, which the Recv loop reads and reports. The
				// reader also owns the Down, with the error that actually killed the
				// connection.
				return
			}
		}
	}
}

// reportLive attests that the subscription was alive just now.
func (s *OKXSource) reportLive() {
	if s.obs != nil {
		s.obs.Live(time.Now().UTC())
	}
}

// reportDown attests that the subscription FAILED just now.
func (s *OKXSource) reportDown(err error) {
	if s.obs != nil {
		s.obs.Down(time.Now().UTC(), err)
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
