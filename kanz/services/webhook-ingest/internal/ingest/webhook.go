// Package ingest is webhook-ingest's core: it authenticates a TradingView
// webhook, records the StrategySignal FACT, resolves the signal's size to an
// absolute quantity against live fund state, and fans it out into N
// order.v1.SubmitOrder COMMANDS per the fund's venue-allocation policy — the
// same commands the OMS already consumes. It executes nothing itself.
package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	signalpb "github.com/eighred/kanz/kanz-schemas-go/signal/v1"
)

// Webhook is the JSON body a TradingView Pine alert posts. Every numeric field
// is a STRING (exact decimal), never a JSON number — no float ever enters the
// pipeline (the zero-float rule).
type Webhook struct {
	StrategyID string `json:"strategy_id"`
	FundID     string `json:"fund_id"`
	Symbol     string `json:"symbol"`
	Action     string `json:"action"`      // buy | sell | close
	Size       string `json:"size"`        // interpreted per SizeType
	SizeType   string `json:"size_type"`   // absolute_qty | pct_of_equity | quote_notional
	Leverage   string `json:"leverage"`    // "1" for spot; optional (default "1")
	MarginMode string `json:"margin_mode"` // "" | cross | isolated
	OrderType  string `json:"order_type"`  // market | limit; optional (default market)
	LimitPrice string `json:"limit_price"` // required when order_type=limit
	// Nonce makes each alert unique; it drives replay defense and the
	// idempotency key so a re-delivered webhook never fires a second time.
	Nonce string `json:"nonce"`
	// TS is the time the strategy fired this alert (RFC3339). REQUIRED.
	//
	// It stopped being advisory in #416. The platform compares it to now and
	// refuses an alert too old to act on — a delayed delivery would otherwise
	// execute at the size the strategy chose for a price that has since moved.
	// An alert with no ts has no age, so it cannot be shown to be current and is
	// refused; an unparseable one is a 400 rather than a silent nil.
	//
	// WEBHOOK_INGEST_REQUIRE_SIGNAL_TS=false relaxes that for a sender being
	// onboarded that cannot stamp its alerts yet.
	TS string `json:"ts"`
}

// parseWebhook decodes and structurally validates the body. It rejects unknown
// fields loudly — a malformed alert is a defect, not something to guess at.
func parseWebhook(raw []byte) (*Webhook, error) {
	dcdr := json.NewDecoder(strings.NewReader(string(raw)))
	dcdr.DisallowUnknownFields()
	var wh Webhook
	if err := dcdr.Decode(&wh); err != nil {
		return nil, fmt.Errorf("malformed webhook body: %w", err)
	}
	if wh.StrategyID == "" || wh.FundID == "" || wh.Symbol == "" || wh.Nonce == "" {
		return nil, errors.New("strategy_id, fund_id, symbol, and nonce are required")
	}
	if wh.Leverage == "" {
		wh.Leverage = "1"
	}
	if wh.OrderType == "" {
		wh.OrderType = "market"
	}
	return &wh, nil
}

// --- enum mapping (string → proto), each rejecting the unspecified case ---

func parseAction(s string) (signalpb.SignalAction, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "buy":
		return signalpb.SignalAction_SIGNAL_ACTION_BUY, nil
	case "sell":
		return signalpb.SignalAction_SIGNAL_ACTION_SELL, nil
	case "close":
		return signalpb.SignalAction_SIGNAL_ACTION_CLOSE, nil
	default:
		return signalpb.SignalAction_SIGNAL_ACTION_UNSPECIFIED, fmt.Errorf("unknown action %q", s)
	}
}

func parseSizeType(s string) (signalpb.SizeType, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "absolute_qty", "qty", "absolute":
		return signalpb.SizeType_SIZE_TYPE_ABSOLUTE_QTY, nil
	case "pct_of_equity", "pct", "percent":
		return signalpb.SizeType_SIZE_TYPE_PCT_OF_EQUITY, nil
	case "quote_notional", "notional", "quote":
		return signalpb.SizeType_SIZE_TYPE_QUOTE_NOTIONAL, nil
	default:
		return signalpb.SizeType_SIZE_TYPE_UNSPECIFIED, fmt.Errorf("unknown size_type %q", s)
	}
}

func parseMarginMode(s string) (signalpb.MarginMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "spot", "none":
		return signalpb.MarginMode_MARGIN_MODE_UNSPECIFIED, nil
	case "cross":
		return signalpb.MarginMode_MARGIN_MODE_CROSS, nil
	case "isolated":
		return signalpb.MarginMode_MARGIN_MODE_ISOLATED, nil
	default:
		return signalpb.MarginMode_MARGIN_MODE_UNSPECIFIED, fmt.Errorf("unknown margin_mode %q", s)
	}
}
