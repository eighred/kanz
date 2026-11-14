package mark

import (
	"math/big"

	decutil "github.com/eighred/kanz/internal/dec"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

// QuotePrices contains exact, positive live prices. A nil Bid or Ask means that
// side was absent; Mid is the mean of both sides or the sole present side.
type QuotePrices struct {
	Bid, Ask, Mid *big.Rat
}

// QualifyLiveQuote refuses a present non-positive or out-of-domain side, a
// crossed pair, or a quote with no sides. An unusable present side cannot be
// replaced by its peer. Positive locked quotes have a usable mid; an observable
// Touch still requires a strict width. Callers choose whether both sides are
// required: live Mark requires both, while accounting FX permits a sole side.
// Point-in-time market history has different signed/zero observation semantics.
func QualifyLiveQuote(bid, ask *commonpb.Decimal) (QuotePrices, bool) {
	var prices QuotePrices
	if bid != nil {
		p, ok := decutil.FromProtoChecked(bid)
		if !ok || p.Sign() <= 0 {
			return QuotePrices{}, false
		}
		prices.Bid = p
	}
	if ask != nil {
		p, ok := decutil.FromProtoChecked(ask)
		if !ok || p.Sign() <= 0 {
			return QuotePrices{}, false
		}
		prices.Ask = p
	}
	switch {
	case prices.Bid != nil && prices.Ask != nil:
		if prices.Bid.Cmp(prices.Ask) > 0 {
			return QuotePrices{}, false
		}
		prices.Mid = new(big.Rat).Quo(new(big.Rat).Add(prices.Bid, prices.Ask), big.NewRat(2, 1))
	case prices.Bid != nil:
		prices.Mid = new(big.Rat).Set(prices.Bid)
	case prices.Ask != nil:
		prices.Mid = new(big.Rat).Set(prices.Ask)
	default:
		return QuotePrices{}, false
	}
	return prices, true
}
