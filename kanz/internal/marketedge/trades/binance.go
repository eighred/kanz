package trades

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/eighred/kanz/internal/exchange/netdial"
)

// BinanceSource is the live Binance Spot trade tape — a TradeSource over the
// <symbol>@trade stream, compiled only under -tags binance so the default binary
// links no websocket.
//
// The aggressor is derived from the `m` flag, which reports whether the BUYER was
// the market MAKER:
//
//	m == true  ⇒ the buyer rested, so the SELLER crossed ⇒ taker SELL
//	m == false ⇒ the buyer crossed                      ⇒ taker BUY
//
// This inversion is the classic trap in the Binance trade feed: reading `m` as
// "was a buy" flips the sign of every volume-delta signal built on the tape, which
// is exactly the kind of error that looks like alpha until it drains the account.
type BinanceSource struct {
	symbol string
	wsBase string
	httpc  *http.Client
	conn   *websocket.Conn

	heartbeat time.Duration

	// obs is the ingestion-coverage seam (#591). Nil means nothing is attested
	// and every interval this feed covers reads downstream as UNKNOWN.
	obs    Liveness
	stopKA context.CancelFunc
}

// BinanceConfig configures a BinanceSource.
type BinanceConfig struct {
	Symbol     string // venue symbol (BTCUSDT)
	WSBase     string // websocket origin, e.g. wss://stream.binance.com:9443
	DNSTTL     time.Duration
	HTTPClient *http.Client // injected by tests; default is the DNS-bypass client
	// Liveness receives this subscription's coverage attestations. Optional; nil
	// attests nothing, and the intervals read as UNKNOWN rather than as covered.
	Liveness Liveness
	// HeartbeatInterval overrides how often the socket is proven to round-trip.
	// <=0 ⇒ HeartbeatInterval. A TEST SEAM, not an operational knob: the coverage
	// recorder's silence tolerance is one value across every feed, so production
	// must not be able to drift one source's cadence away from the others.
	HeartbeatInterval time.Duration
}

// NewBinanceSource builds the live trade source.
func NewBinanceSource(cfg BinanceConfig) *BinanceSource {
	if cfg.HTTPClient == nil {
		// Zero-Timeout client: the trade stream is long-lived (see netdial).
		cfg.HTTPClient = netdial.NewWebsocketHTTPClient(cfg.DNSTTL)
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = HeartbeatInterval
	}
	return &BinanceSource{
		symbol:    cfg.Symbol,
		wsBase:    strings.TrimSuffix(cfg.WSBase, "/"),
		httpc:     cfg.HTTPClient,
		obs:       cfg.Liveness,
		heartbeat: cfg.HeartbeatInterval,
	}
}

var _ TradeSource = (*BinanceSource)(nil)

// binanceTrade is one <symbol>@trade frame.
type binanceTrade struct {
	Event        string `json:"e"`
	EventMS      int64  `json:"E"`
	Symbol       string `json:"s"`
	TradeID      int64  `json:"t"`
	Price        string `json:"p"`
	Qty          string `json:"q"`
	TradeMS      int64  `json:"T"`
	BuyerIsMaker bool   `json:"m"`
}

// Recv yields the next trade, reconnecting on a dropped stream.
func (s *BinanceSource) Recv(ctx context.Context) (Trade, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Trade{}, err
		}
		if s.conn == nil {
			if err := s.connect(ctx); err != nil {
				s.reportDown(err)
				return Trade{}, err
			}
		}
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			s.reset() // force a reconnect on the next Recv
			err = fmt.Errorf("binance trades: read: %w", err)
			s.reportDown(err)
			return Trade{}, err
		}
		// A FRAME IS A FRAME. Any read that returned proves the socket is
		// round-tripping, including the control frames the decode below discards —
		// so liveness is reported here, before anything looks at what arrived.
		// Reporting it after the "is this a trade" filter would tie coverage to
		// trading activity, which is exactly the conflation the record exists to
		// end.
		s.reportLive()
		var t binanceTrade
		if err := json.Unmarshal(data, &t); err != nil {
			return Trade{}, fmt.Errorf("binance trades: decode: %w", err)
		}
		if t.Event != "trade" {
			continue // a control frame — not an execution
		}
		price, okP := new(big.Rat).SetString(t.Price)
		size, okS := new(big.Rat).SetString(t.Qty)
		if !okP || !okS {
			continue // unparseable — drop rather than guess a price
		}
		// See the type doc: `m` reports the BUYER as maker, so it means taker SELL.
		side := SideBuy
		if t.BuyerIsMaker {
			side = SideSell
		}
		ms := t.TradeMS
		if ms == 0 {
			ms = t.EventMS
		}
		return Trade{
			Price: price, Size: size, TakerSide: side,
			EventTime: time.UnixMilli(ms).UTC(),
		}, nil
	}
}

func (s *BinanceSource) connect(ctx context.Context) error {
	stream := strings.ToLower(s.symbol) + "@trade"
	conn, _, err := websocket.Dial(ctx, s.wsBase+"/ws/"+stream, &websocket.DialOptions{HTTPClient: s.httpc})
	if err != nil {
		return fmt.Errorf("binance trades: dial %s: %w", stream, err)
	}
	conn.SetReadLimit(1 << 20)
	s.conn = conn

	// THE HEARTBEAT IS WHAT MAKES A QUIET MINUTE ATTESTABLE (#591). Binance
	// needs no application keepalive to stay connected — the server pings and
	// coder/websocket answers underneath Read — so this goroutine exists purely
	// to produce EVIDENCE. Without it the only liveness signal on this feed would
	// be trades, and a coverage record built on trades reports a quiet market as
	// an unobserved one, which is the conflation the record exists to end.
	//
	// It is also the only thing that detects a half-open socket here: Read blocks
	// forever on a TCP connection that died silently, and a feed that is neither
	// erroring nor delivering is the failure this platform is least equipped to
	// see.
	kaCtx, cancel := context.WithCancel(ctx)
	s.stopKA = cancel
	go s.keepAlive(kaCtx, conn)
	return nil
}

// keepAlive proves the socket round-trips while the market is quiet.
//
// Conn.Ping SENDS A PING AND WAITS FOR THE PONG, so a nil return is end-to-end
// evidence rather than a local write succeeding — which is the difference
// between attesting that we were connected and attesting that the venue was
// still talking to us. It must run concurrently with Read (the pong is read by
// the Recv loop), which is exactly the shape here.
func (s *BinanceSource) keepAlive(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(s.heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pingCtx, cancel := context.WithTimeout(ctx, HeartbeatTimeout)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				// NOT REPORTED AS Down. The reader owns that call: it is about to
				// surface the dead connection with the error that actually killed
				// it, and two reports would count one break twice. Going quiet is
				// enough — uncredited time is time nobody vouches for.
				return
			}
			s.reportLive()
		}
	}
}

// reportLive attests that the subscription was alive just now.
func (s *BinanceSource) reportLive() {
	if s.obs != nil {
		s.obs.Live(time.Now().UTC())
	}
}

// reportDown attests that the subscription FAILED just now.
func (s *BinanceSource) reportDown(err error) {
	if s.obs != nil {
		s.obs.Down(time.Now().UTC(), err)
	}
}

// reset tears down the connection and its heartbeat together. They must go
// together: a keepalive left running against a closed connection reports Live
// forever off a socket nobody is reading.
func (s *BinanceSource) reset() {
	if s.stopKA != nil {
		s.stopKA()
		s.stopKA = nil
	}
	s.conn = nil
}

// Close releases the stream.
func (s *BinanceSource) Close() error {
	if s.conn == nil {
		s.reset()
		return nil
	}
	err := s.conn.Close(websocket.StatusNormalClosure, "")
	s.reset()
	return err
}
