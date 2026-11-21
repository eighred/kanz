package spotbooks

import (
	"strings"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/risk/scenario"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"google.golang.org/protobuf/proto"
)

func ValidCheck(r *querypb.SpotBookCheckRequest) bool {
	ids := r.GetEventIds()
	if len(ids) != 2 || ids[0] == ids[1] || r.MaxAgeMs == 0 || time.Duration(r.MaxAgeMs)*time.Millisecond > scenario.SpotBookMaxAge {
		return false
	}
	for _, id := range ids {
		if len(id) == 0 || len(id) > 128 || id != strings.TrimSpace(id) {
			return false
		}
	}
	return true
}

// Check compares the two priced books with a single observation taken after
// pricing. Balances, fees, fills and trading permissions are outside this check.
func Check(current *querypb.SpotBookObservation, priced *querypb.SpotDepthScenario, req *querypb.SpotBookCheckRequest) *querypb.SpotBookCheckResult {
	out := &querypb.SpotBookCheckResult{State: querypb.SpotBookCheckState_SPOT_BOOK_CHECK_STATE_UNVERIFIED,
		CheckedAt: current.GetAsOf(), MaxAgeMs: req.GetMaxAgeMs()}
	if !ValidCheck(req) || len(priced.GetVenues()) != 2 || current.GetAsOf().CheckValid() != nil ||
		priced.GetAsOf().CheckValid() != nil || current.AsOf.AsTime().Before(priced.AsOf.AsTime()) {
		return out
	}
	if current.Refusal == querypb.SpotBookRefusal_SPOT_BOOK_REFUSAL_STALE {
		out.State = querypb.SpotBookCheckState_SPOT_BOOK_CHECK_STATE_STALE
		return out
	}
	if current.Status != "SUPPORTED" || len(current.Venues) != 2 {
		return out
	}
	stale := false
	for i, v := range priced.Venues {
		b := v.GetBook()
		c := current.Venues[0]
		if c.GetBook().GetMic() != b.GetMic() {
			c = current.Venues[1]
		}
		now := c.GetBook()
		if b == nil || now == nil || b.Mic != now.Mic || b.InstrumentId != now.InstrumentId || b.Symbol != now.Symbol ||
			v.ProductType != current.ProductType || v.BaseAssetRef != current.BaseAssetRef || v.QuoteAssetRef != current.QuoteAssetRef {
			return out
		}
		if now.EventTime.AsTime().After(b.EventTime.AsTime()) ||
			(proto.Equal(now.EventTime, b.EventTime) && now.LastUpdateSequence > b.LastUpdateSequence) {
			out.State = querypb.SpotBookCheckState_SPOT_BOOK_CHECK_STATE_SUPERSEDED
			return out
		}
		if req.EventIds[i] != c.EventId || !proto.Equal(v.KnownAt, c.KnownAt) ||
			!proto.Equal(b.EventTime, now.EventTime) || b.LastUpdateSequence != now.LastUpdateSequence ||
			!sameLevels(b.Bids, now.Bids) || !sameLevels(b.Asks, now.Asks) {
			return out
		}
		if current.AsOf.AsTime().Sub(now.EventTime.AsTime()) > time.Duration(req.MaxAgeMs)*time.Millisecond {
			stale = true
		}
	}
	out.State = querypb.SpotBookCheckState_SPOT_BOOK_CHECK_STATE_CURRENT
	if stale {
		out.State = querypb.SpotBookCheckState_SPOT_BOOK_CHECK_STATE_STALE
	}
	return out
}

func sameLevels(a, b []*marketpb.PriceLevel) bool {
	if len(a) != len(b) {
		return false
	}
	for i, l := range a {
		if dec.Cmp(l.GetPrice(), b[i].GetPrice()) != 0 || dec.Cmp(l.GetSize(), b[i].GetSize()) != 0 {
			return false
		}
	}
	return true
}
