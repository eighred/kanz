//go:build binance || okx

package execution

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	"github.com/kanz-eng/kanz/services/oms/internal/dec"
)

// ErrRateLimited is returned when the local weight budget is exhausted before a
// REST call — the caller backs off and raises a structural alert rather than
// firing the request and risking an exchange ban. It is never a fabricated fill.
// Shared across exchange connectors.
var ErrRateLimited = errors.New("exchange: local rate-limit budget exhausted")

// APIError is a typed exchange error body (code + message).
type APIError struct {
	Code int
	Msg  string
}

func (e *APIError) Error() string { return fmt.Sprintf("exchange error %d: %s", e.Code, e.Msg) }

// SymbolMapper maps a canonical Kanz instrument_id to an exchange's symbol
// (e.g. "BTC-USD" -> "BTCUSDT" on Binance, "BTC-USDT" on OKX). An unmapped
// instrument cannot be traded on that venue.
type SymbolMapper interface {
	Symbol(instrumentID string) (string, bool)
}

// StaticSymbolMap is a fixed instrument→symbol map.
type StaticSymbolMap map[string]string

func (m StaticSymbolMap) Symbol(id string) (string, bool) { s, ok := m[id]; return s, ok }

// VenueSettings is the public configuration the composition root supplies to
// build an exchange venue without touching the internal REST client / rate
// bucket. Shared across Binance and OKX; Passphrase is OKX-only.
type VenueSettings struct {
	MIC          string
	BaseURL      string
	APIKey       string
	APISecret    string
	Passphrase   string            // OKX API passphrase (unused by Binance)
	Symbols      map[string]string // instrument_id -> exchange symbol
	WeightBudget int               // per-window REST weight/request budget
	OnThrottle   func()            // structural-alert hook on budget exhaustion
	DNSTTL       time.Duration     // DNS cache TTL for the bypass dialer (default 5m)
}

// formatDec renders a common.v1.Decimal as a plain decimal string for an
// exchange API. NOTE: exchange lot/price-filter precision rounding is a
// hardening item — testnet tolerates unrounded values; production binds the
// symbol filters before send.
func formatDec(d *commonpb.Decimal) string {
	s := dec.FromProto(d).FloatString(8)
	for len(s) > 0 && s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] == '.' {
		s = s[:len(s)-1]
	}
	if s == "" {
		s = "0"
	}
	return s
}

// parseDec parses an exchange decimal string into an exact common.v1.Decimal.
func parseDec(s string) *commonpb.Decimal {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return &commonpb.Decimal{}
	}
	return dec.ToProto(r)
}
