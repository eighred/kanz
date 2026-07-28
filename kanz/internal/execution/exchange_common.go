package execution

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/dec"
)

// sleep waits d or until ctx is cancelled — the connectors' reconnect backoff.
// Sleep is a context-aware sleep shared by the exchange connectors.
func Sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// capDur clamps d to max.
// CapDur caps a backoff duration.
func CapDur(d, max time.Duration) time.Duration {
	if d > max {
		return max
	}
	return d
}

// Correcting-FACT subjects (M3.4). StateHealed/BalanceReconciled ride the
// envelope with QUALITY_FLAG_REVISED; downstream folders (the journal, tv-sync)
// apply them to restore parity — the reconciler never edits state directly.
// Shared so both exchange reconcilers publish on one subject.
const (
	SubjectStateHealed  = "order.order.healed"
	SubjectBalanceRecon = "accounting.balance.reconciled"
)

// ErrRateLimited is returned when the local weight budget is exhausted before a
// REST call — the caller backs off and raises a structural alert rather than
// firing the request and risking an exchange ban. It is never a fabricated fill.
// Shared across exchange connectors.
var ErrRateLimited = errors.New("exchange: local rate-limit budget exhausted")

// ErrEgressDenied is returned when the venue rejects the request at the
// authentication/authorization boundary (HTTP 401/403) — the signature is
// well-formed but refused, the signature that a production API key locked to our
// dedicated Tokyo/London egress IPs is being used from an unauthorized
// environment (IP allowlist miss). It is a hard, non-retryable structural fault:
// the caller must alert and stop, never retry into a ban. Distinct from a
// transient 5xx (retryable) and from ErrRateLimited (local backpressure).
var ErrEgressDenied = errors.New("exchange: request denied at auth boundary (egress IP not authorized?)")

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
	MIC string
	// Account is the EXCHANGE ACCOUNT the APIKey below belongs to — the OKX
	// sub-account, the Binance account. It is the collateral boundary: everything
	// this adapter fills is margined and liquidated against it, for every portfolio
	// whose orders reach it. The adapter holds the credential, so the adapter is the
	// only thing that can honestly say which account it trades.
	Account      string
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
// FormatDec renders an exact decimal for an exchange wire field.
func FormatDec(d *commonpb.Decimal) string {
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
// ParseDec parses an exchange decimal string into common.v1.Decimal.
func ParseDec(s string) *commonpb.Decimal {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return &commonpb.Decimal{}
	}
	return dec.ToProto(r)
}

// subDec returns a − b as an exact common.v1.Decimal.
// SubDec subtracts two exact decimals.
func SubDec(a, b *commonpb.Decimal) *commonpb.Decimal {
	return dec.ToProto(new(big.Rat).Sub(dec.FromProto(a), dec.FromProto(b)))
}
