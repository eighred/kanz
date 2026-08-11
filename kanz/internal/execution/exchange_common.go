package execution

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

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

	// Instruments enumerates the whole mapping (#406).
	//
	// The set was previously readable only one id at a time, by a caller that
	// already knew the id — so "what can this deployment trade?" had no answer
	// short of reading a manifest, and no picker could offer a choice of pairs.
	// It is the same un-askable configuration Describe was added to fix for the
	// exchange account.
	Instruments() []InstrumentSymbol
}

// InstrumentSymbol is one tradeable pair: what this platform calls it, and what
// the exchange calls the same thing.
type InstrumentSymbol struct {
	// InstrumentID is the canonical id an order carries, e.g. "BTC-USD".
	InstrumentID string
	// VenueSymbol is the exchange's name for it, e.g. "BTCUSDT".
	//
	// THE TWO DISAGREE TODAY: canonical "BTC-USD" maps to a USDT-quoted symbol on
	// both live venues, so the platform trades a stablecoin-quoted instrument
	// while calling it USD. Until instruments carry base and quote explicitly
	// (#407) this is the only field where that is visible.
	VenueSymbol string
}

// VenueInstrument is one tradeable pair at one venue — the same pair may be
// listed by several adapters, and the MIC is what tells them apart.
//
// AN ORDER NAMES A VENUE, so a catalogue that lost the MIC would offer a pair
// without saying where it can be traded, and the caller would have to guess. The
// router refuses a guess (Router.Supports), so the guess becomes a refusal at
// admission that the operator reads as a platform fault.
type VenueInstrument struct {
	MIC string
	InstrumentSymbol
}

// StaticSymbolMap is a fixed instrument→symbol map.
type StaticSymbolMap map[string]string

func (m StaticSymbolMap) Symbol(id string) (string, bool) { s, ok := m[id]; return s, ok }

// Instruments returns the mapping ORDERED BY instrument_id. Go randomises map
// iteration, and an unordered answer would make a picker's list reshuffle on
// every load and a golden test flake — neither is a property worth leaving to
// chance for a list a person reads.
func (m StaticSymbolMap) Instruments() []InstrumentSymbol {
	out := make([]InstrumentSymbol, 0, len(m))
	for id, sym := range m {
		out = append(out, InstrumentSymbol{InstrumentID: id, VenueSymbol: sym})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstrumentID < out[j].InstrumentID })
	return out
}

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

// ParseDec parses an exchange decimal string into an exact common.v1.Decimal.
//
// ok=false means the string was not a number, or the value cannot be represented
// as a Decimal at all. It is NOT a value — the caller must refuse the message it
// came from and must never substitute the zero Decimal.
//
// BOTH HALVES OF THAT CONTRACT ARE REPAIRS (#94). This function used to return
// dec.ToProto(r), and &commonpb.Decimal{} — ZERO — for a string it could not
// parse:
//
//   - The zero return is the never-substitute-zero rule broken in the venue
//     parser itself. A garbled FillSz became a fill of quantity 0, a garbled
//     AvgPx became a fill at price 0, and both are FACTs the ledger folds. A
//     zero price does not look wrong on a dashboard; it looks free.
//   - dec.ToProto WRAPS once the scaled coefficient exceeds an int64, around 92.2
//     billion units at scale 8. That is $92bn in money terms and an ordinary
//     position in tokens: OKX lists assets that trade in the trillions, and an
//     accFillSz of 1e12 came back through here as 77662796314.5224192.
//
// dec.ToProtoScaled preserves magnitude and reports when it cannot, so a real
// large fill converts exactly and only a genuinely unrepresentable one refuses.
func ParseDec(s string) (*commonpb.Decimal, bool) {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, false
	}
	return dec.ToProtoScaled(r)
}

// SubDec returns a − b as an exact common.v1.Decimal. ok=false when the result
// cannot be represented; see ParseDec for why that is a refusal and not a zero.
func SubDec(a, b *commonpb.Decimal) (*commonpb.Decimal, bool) {
	return dec.ToProtoScaled(new(big.Rat).Sub(dec.FromProto(a), dec.FromProto(b)))
}
