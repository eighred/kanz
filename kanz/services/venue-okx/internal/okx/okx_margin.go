package okx

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"time"
)

// OKX's margin state for this adapter's account (#408, control 1).
//
// # Every number here is OKX's, and where OKX is silent so is this
//
// The unified account endpoint reports the maintenance margin requirement (mmr)
// and OKX's own margin ratio (mgnRatio); the positions endpoint reports a
// liquidation price (liqPx) per open position. None of it is recomputed from
// Kanz's book, and NOTHING IS SUBSTITUTED WHEN A FIELD IS EMPTY — which is the
// normal answer for an account in cash mode, where OKX returns "" for every
// margin field. An empty string parsed as zero would say "this account needs no
// collateral and liquidates at zero", which is the confident zero #408's ruling
// singles out as the one that costs the fund its collateral.
//
// # What is proven here and what is not
//
// The DECISION, the SHAPE and the PARSE are certified against recorded response
// shapes in okx_margin_test.go — including OKX's own documented empty-field
// behaviour, which is the case that matters. THE LIVE CALL IS NOT EXERCISED:
// this estate holds no OKX credentials (#70 is blocked-external), so no test
// here has ever seen the real endpoint answer. What that leaves unproven is
// narrow and worth naming: that these are the field names and units OKX serves
// TODAY, and that the account this adapter holds is in a mode that populates
// them at all. Both are venue wire facts, and #529's lesson is that venue wire
// facts are only knowable by asking the venue.
//
// A WRONG FIELD NAME FAILS SAFE HERE, and that is deliberate rather than lucky:
// an unmarshalled field that does not exist stays the zero string, which becomes
// nil, which becomes UNKNOWN, which every control in the set refuses on. The
// failure mode of this being wrong is a margin control that will not pass, not
// one that passes on a number nobody checked.

// okxAccountMargin is the margin half of GET /api/v5/account/balance.
//
// SAME ENDPOINT THE BALANCE RECONCILER ALREADY CALLS, decoded into its own
// struct rather than widening okxBalances. That struct is load-bearing for cash
// reconciliation, and a decode shape shared between "what do we hold" and "what
// must we post" is one edit away from a change made for one breaking the other.
type okxAccountMargin struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data []struct {
		// UTime is OKX's own update time for the account state, epoch
		// milliseconds as a string. IT IS THE OBSERVATION TIME, and preferring it
		// over our fetch clock is what makes the freshness bound measure how
		// recently OKX LOOKED rather than how recently we asked.
		UTime string `json:"uTime"`
		// MMR is the maintenance margin requirement in the account's valuation
		// currency. "" in cash mode and on any account OKX does not margin.
		MMR string `json:"mmr"`
		// MgnRatio is OKX's margin ratio. IT RISES AS THE ACCOUNT GETS SAFER —
		// the opposite direction from a "used margin / equity" ratio — and
		// nothing here normalises it, because normalising means asserting a
		// venue's convention on its behalf.
		MgnRatio string `json:"mgnRatio"`
	} `json:"data"`
}

// okxPositionsResp is GET /api/v5/account/positions.
type okxPositionsResp struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data []struct {
		InstID string `json:"instId"`
		// LiqPx is OKX's liquidation price for the position. "" for a position
		// OKX does not compute one for — reported as an exclusion rather than
		// dropped, so an operator can see WHICH position's liquidation distance
		// is unknowable.
		LiqPx string `json:"liqPx"`
		UTime string `json:"uTime"`
	} `json:"data"`
}

// MarginState implements execution.VenueMarginSource against OKX.
//
// TWO CALLS, AND A FAILURE OF THE SECOND IS NOT A FAILURE OF THE FIRST. The
// account-level maintenance margin and margin ratio are the numbers a pre-trade
// gate reads; the per-position liquidation prices are the numbers an operator
// reads. Returning an error because the positions call was rate-limited would
// throw away a maintenance margin OKX had already given us and turn a partial
// answer into a total refusal — which, under a fail-closed gate, is a trading
// halt caused by the less important of the two endpoints. So the positions leg
// degrades to "no positions reported", which the reporter renders as coverage,
// not as a claim that the account holds none.
//
// The account leg is different: without it there is no observation at all, and
// an error is the honest answer.
func (c *okxREST) MarginState(ctx context.Context) (VenueMargin, error) {
	account, err := c.exchangeAccount(ctx)
	if err != nil {
		return VenueMargin{}, err
	}
	support := marginSupport(account.AccountLevel)

	if !c.buckets.allow(familyUnverified, 1) {
		c.onThrottle()
		return VenueMargin{}, ErrRateLimited
	}
	raw, err := c.signedRequest(ctx, http.MethodGet, "/api/v5/account/balance", nil)
	if err != nil {
		return VenueMargin{}, err
	}
	var acct okxAccountMargin
	if err := json.Unmarshal(raw, &acct); err != nil {
		return VenueMargin{}, fmt.Errorf("okx: decode account margin: %w", err)
	}
	if acct.Code != "0" {
		return VenueMargin{}, &APIError{Code: atoiSafe(acct.Code), Msg: acct.Msg}
	}

	// FETCH TIME IS THE FALLBACK, NEVER THE PREFERENCE. It is an UPPER bound on
	// how recently OKX looked, so using it where uTime exists would make a figure
	// look fresher than it is by however long the account sat unchanged — the one
	// direction of error a freshness bound cannot survive.
	out := VenueMargin{
		ObservedAt:               c.now().UTC(),
		MaintenanceMarginSupport: support,
		MarginRatioSupport:       support,
	}
	if len(acct.Data) > 0 {
		d := acct.Data[0]
		out.MaintenanceMargin = parseVenueRat(d.MMR)
		out.MarginRatio = parseVenueRat(d.MgnRatio)
		if ts, ok := parseVenueMillis(d.UTime); ok {
			out.ObservedAt = ts
		}
	}

	if !c.buckets.allow(familyUnverified, 1) {
		c.onThrottle()
		return out, nil
	}
	praw, err := c.signedRequest(ctx, http.MethodGet, "/api/v5/account/positions", nil)
	if err != nil {
		return out, nil
	}
	var pos okxPositionsResp
	if err := json.Unmarshal(praw, &pos); err != nil || pos.Code != "0" {
		return out, nil
	}
	for _, p := range pos.Data {
		if p.InstID == "" {
			continue
		}
		out.Positions = append(out.Positions, VenuePositionMargin{
			Symbol:           p.InstID,
			LiquidationPrice: parseVenueRat(p.LiqPx),
		})
	}
	return out, nil
}

// marginSupport interprets OKX acctLv. Spot/simple accounts cannot take margin
// and their empty mmr/mgnRatio fields are UNSUPPORTED, not a failed observation.
// Every margin-capable mode says the fields are supported; an absent value in
// those modes remains UNKNOWN and is recorded as incomplete coverage.
func marginSupport(accountLevel string) SupportStatus {
	switch accountLevel {
	case "1":
		return SupportUnsupported
	case "2", "3", "4":
		return SupportSupported
	default:
		return SupportUnknown
	}
}

// parseVenueRat converts an exchange decimal string EXACTLY, or reports UNKNOWN.
//
// nil FOR ANYTHING THAT IS NOT A NUMBER, which is the whole reason this is not
// ParseDec: that helper answers an unparseable string with ZERO, and a zero
// margin figure is a claim rather than an absence. OKX returns "" for every
// margin field on an account it does not margin, so the empty case is not a
// corner — it is the answer this adapter will get most often until an account is
// switched to a margin mode.
//
// A NEGATIVE MMR OR LIQUIDATION PRICE IS ALSO REFUSED. Neither quantity is
// meaningful below zero on any venue, so a negative one is a field this code has
// misread rather than a state the exchange is in, and refusing it leaves UNKNOWN
// — which fails closed — instead of feeding a nonsense number to a gate.
func parseVenueRat(s string) *big.Rat {
	if s == "" {
		return nil
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() < 0 {
		return nil
	}
	return r
}

// parseVenueMillis converts OKX's epoch-milliseconds string to a UTC time.
//
// A NON-POSITIVE OR UNPARSEABLE STAMP IS REFUSED rather than becoming the Unix
// epoch: 1970 is not merely a wrong observation time, it is one that would be
// judged stale by every bound and would therefore silently disable the account's
// margin controls while looking like ordinary staleness.
func parseVenueMillis(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil || ms <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(ms).UTC(), true
}
