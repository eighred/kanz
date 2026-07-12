package alpha

import (
	"github.com/kanz-eng/kanz/internal/marketedge/depth"
	"github.com/kanz-eng/kanz/internal/marketedge/trades"
)

// BinanceFeed builds the live Binance Spot feed for one instrument: the
// <symbol>@depth L2 stream (anchored on the REST depth snapshot) and the
// <symbol>@trade tape. Compiled only under -tags binance, so the default binary
// links no websocket.
//
// It is exported from the boundary so Kanz's open edge and the restricted alpha
// layer construct the SAME feed — the code that touches the market is identical
// either way, and only the decision-making differs.
func BinanceFeed(cfg VenueFeedConfig) Feed {
	mic := cfg.MIC
	if mic == "" {
		mic = "BINANCE"
	}
	return Feed{
		InstrumentID: cfg.InstrumentID,
		MIC:          mic,
		Depth: depth.NewBinanceSource(depth.BinanceConfig{
			InstrumentID: cfg.InstrumentID, Symbol: cfg.Symbol, MIC: mic,
			WSBase: cfg.WSBase, RESTBase: cfg.RESTBase,
			Limit: cfg.DepthLimit, DNSTTL: cfg.DNSTTL,
		}),
		Trades: trades.NewBinanceSource(trades.BinanceConfig{
			Symbol: cfg.Symbol, WSBase: cfg.WSBase, DNSTTL: cfg.DNSTTL,
		}),
	}
}
