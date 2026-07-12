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

	"github.com/kanz-eng/kanz/internal/exchange/netdial"
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
}

// BinanceConfig configures a BinanceSource.
type BinanceConfig struct {
	Symbol     string // venue symbol (BTCUSDT)
	WSBase     string // websocket origin, e.g. wss://stream.binance.com:9443
	DNSTTL     time.Duration
	HTTPClient *http.Client // injected by tests; default is the DNS-bypass client
}

// NewBinanceSource builds the live trade source.
func NewBinanceSource(cfg BinanceConfig) *BinanceSource {
	if cfg.HTTPClient == nil {
		// Zero-Timeout client: the trade stream is long-lived (see netdial).
		cfg.HTTPClient = netdial.NewWebsocketHTTPClient(cfg.DNSTTL)
	}
	return &BinanceSource{
		symbol: cfg.Symbol,
		wsBase: strings.TrimSuffix(cfg.WSBase, "/"),
		httpc:  cfg.HTTPClient,
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
				return Trade{}, err
			}
		}
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			s.conn = nil // force a reconnect on the next Recv
			return Trade{}, fmt.Errorf("binance trades: read: %w", err)
		}
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
	return nil
}

// Close releases the stream.
func (s *BinanceSource) Close() error {
	if s.conn == nil {
		return nil
	}
	err := s.conn.Close(websocket.StatusNormalClosure, "")
	s.conn = nil
	return err
}
