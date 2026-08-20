package alpha

import (
	"github.com/eighred/kanz/internal/marketedge/depth"
	"github.com/eighred/kanz/internal/marketedge/trades"
)

// OKXFeed builds the live OKX v5 feed for one instrument: the public `books` L2
// channel and the `trades` tape. Compiled only under -tags okx, so the default
// binary links no websocket.
//
// It is exported from the boundary so Kanz's open edge and the restricted alpha
// layer construct the SAME feed.
func OKXFeed(cfg VenueFeedConfig) Feed {
	mic := cfg.MIC
	if mic == "" {
		mic = "OKX"
	}
	return Feed{
		InstrumentID: cfg.InstrumentID,
		MIC:          mic,
		Depth: depth.NewOKXSource(depth.OKXConfig{
			InstrumentID: cfg.InstrumentID, InstID: cfg.Symbol, MIC: mic,
			WSURL: cfg.WSBase, DNSTTL: cfg.DNSTTL,
		}),
		Trades: trades.NewOKXSource(trades.OKXConfig{
			InstID: cfg.Symbol, WSURL: cfg.WSBase, DNSTTL: cfg.DNSTTL,
			// The TRADE feed attests, not the book — see BinanceFeed.
			Liveness: cfg.coverageObserver(mic, "okx:trades:"+cfg.Symbol),
		}),
	}
}
