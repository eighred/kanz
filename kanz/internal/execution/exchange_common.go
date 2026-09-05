package execution

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sort"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/instrument"
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
	// IT IS THE SOURCE OF TRUTH FOR WHAT IS ACTUALLY BOUGHT. The symbol is what
	// the adapter sends to the exchange; InstrumentID is what a human typed
	// beside it in a symbol map. The estate mapped "BTC-USD" to BTCUSDT on both
	// live venues, so the pair below is derived from THIS field (#407).
	VenueSymbol string

	// Pair is what this mapping actually trades, resolved from VenueSymbol.
	//
	// A ZERO Pair MEANS THE PLATFORM COULD NOT TELL — a venue using its own
	// ticker for the asset, or an instrument that is not a pair at all. It never
	// means "no quote", and a caller must not render it as a fact.
	Pair instrument.Pair

	// QuoteMismatch reports that InstrumentID claims one quote and VenueSymbol
	// shows another. False when the quote could not be determined at all: an
	// unreadable symbol is not evidence of a mismatch.
	QuoteMismatch bool
}

// Mismatches returns the entries whose canonical id disagrees with the exchange
// symbol about the quote asset (#407).
//
// AN ADAPTER CALLS THIS BEFORE IT SERVES AN ORDER, because the mapping is a
// statement about what a position is denominated in, and this is the last moment
// anything can check it: after this the symbol goes to the exchange and the id
// goes into the ledger, and nothing downstream sees both.
//
// It is empty both when every mapping agrees AND when none could be read, so a
// caller that wants to distinguish "checked, and fine" from "could not tell"
// must look at Instruments() — which is the distinction this platform's
// standard exists to preserve.
func (m StaticSymbolMap) Mismatches() []InstrumentSymbol {
	var out []InstrumentSymbol
	for _, in := range m.Instruments() {
		if in.QuoteMismatch {
			out = append(out, in)
		}
	}
	return out
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
		in := InstrumentSymbol{InstrumentID: id, VenueSymbol: sym}
		// Resolved HERE, once, where both halves of the mapping are in hand —
		// so the catalogue, the startup check and the picker cannot form
		// different opinions about what a position is denominated in (#407).
		if res, isPair := instrument.Resolve(id, sym); isPair {
			in.Pair = res.Pair
			in.QuoteMismatch = res.Mismatched
		}
		out = append(out, in)
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

// ErrVenueOverfill is the exchange claiming a cumulative filled quantity LARGER
// than the quantity this platform sent it.
//
// IT IS NOT AN ARITHMETIC EDGE CASE, IT IS A DISAGREEMENT ABOUT WHAT WAS
// AUTHORISED — the same class Reconcile freezes an order over, and the same one
// the OMS aggregate refuses as OVERFILL when the fill reaches it through
// ApplyFill. On the user-data websocket it does not reach ApplyFill: the
// ingester builds the fill FACT itself and the OMS order aggregate does not
// consume that subject, so this sentinel is the whole bound on that path.
var ErrVenueOverfill = errors.New("venue reports a cumulative filled quantity greater than the ordered quantity")

// ErrLeavesUnrepresentable is ordered − cumulative not fitting a Decimal at all.
// Separate from ErrVenueOverfill because they are different findings: one is a
// venue contradiction an operator must resolve against the exchange's own order
// history, the other is a number too large to carry, and merging them would make
// the counter that fires on the first unreadable.
var ErrLeavesUnrepresentable = errors.New("leaves quantity is not representable as a Decimal")

// LeavesRemaining is ordered − cumulative for a venue execution report, and the
// ONE place the bound between them is applied.
//
// # Why it exists rather than a subtraction at each ingester
//
// Both connectors computed leaves with SubDec and checked only its ok. SubDec
// represents a negative result perfectly well and reports ok, so a venue
// reporting a cumulative 14 against an order of 10 produced LeavesQuantity −4
// and published it as an order.order.filled FACT. The position book folded it
// and the accounting ledger journalled both legs: the fund's book recorded an
// execution larger than any order the controls admitted, with no refusal, no
// quarantine and no counter, and the first thing that could notice was a balance
// reconciliation against the venue, if it ran (#1045).
//
// The two connectors shared the shape, so a fix in one of them would have been a
// fix in one of them. This is the helper both call.
//
// # Refusing, not healing
//
// The caller must refuse the WHOLE report on either error — never clamp leaves
// to zero and never record the venue's cumulative. Clamping would heal the order
// to a size the platform never authorised, which is the same defect with the
// evidence removed; and the fill quantity on the report is not trustworthy
// either once the cumulative is not.
//
// EQUALITY IS NOT AN OVERFILL. A report that completes the order exactly is the
// ordinary terminal case and leaves zero.
func LeavesRemaining(ordered, cumulative *commonpb.Decimal) (*commonpb.Decimal, error) {
	o, c := dec.FromProto(ordered), dec.FromProto(cumulative)
	if c.Cmp(o) > 0 {
		return nil, fmt.Errorf("%w: ordered %s, venue reports %s cumulative filled",
			ErrVenueOverfill, o.FloatString(8), c.FloatString(8))
	}
	leaves, ok := SubDec(ordered, cumulative)
	if !ok {
		return nil, fmt.Errorf("%w: ordered %s minus cumulative %s",
			ErrLeavesUnrepresentable, o.FloatString(8), c.FloatString(8))
	}
	return leaves, nil
}

// ReportRefusal is the ONE answer both connectors give a venue execution report
// they will not publish as a fill (#1045).
//
// # Why it is here and not written out at each ingester
//
// The bound was missing from both connectors because the subtraction was
// written at both, so the answer to it is shared for the same reason the bound
// is. A refusal implemented per connector is a refusal that gets improved on one
// venue: the copy is made once and the fix lands on one side afterwards, which
// is how "one implementation per concept" fails in practice on this platform.
//
// # What a refusal has to leave behind, and why all three
//
// REFUSING ALONE IS A SILENT DROP, and a dropped execution is worse than a wrong
// one: the fund holds a position and no FACT, no projection and no ledger entry
// says so. Nothing downstream can notice, because there is nothing to notice.
// So a refusal is three marks and no fewer:
//
//   - the COUNTER, which is the only alertable signal on this path. The two fill
//     paths that reach the OMS aggregate move its quarantine counter when
//     ApplyFill refuses; this one reaches no aggregate, so without this
//     "the venue over-filled us and we refused" and "no venue has ever
//     over-filled us" are the same silence.
//   - the ERROR LOG, which carries the venue's own numbers an operator resolves
//     the disagreement with.
//   - the FREEZE on this adapter's own order view, which is the durable half:
//     orderview.Dispatch refuses to work a quarantined order, so the refusal
//     survives into the next ExecuteRequest instead of being a line in a log
//     nobody greps.
//
// COUNTED AND LOGGED BEFORE THE FREEZE, for the reason the OMS's own quarantine
// states: the freeze is a store write and it can fail, and an operator must
// learn the attempt was made either way.
type ReportRefusal struct {
	// Venue is the MIC, and the counter's first answer to "which exchange".
	Venue string
	// Orders is the view the freeze is written to. Its Quarantined is non-failing
	// by contract — this runs inside a websocket read loop.
	Orders OrderTracker
	// OnRefused is the counter seam, supplied by the composition root from
	// WorkerDeps.OnFillRefused. Nil is tolerated so a test need not wire one;
	// production never leaves it nil, because the completeness guard makes an
	// omitted WorkerDeps field a visible decision in a diff.
	OnRefused func(mic, orderID, reason string)
	// OnDropped is the counter seam for Dropped, supplied from
	// WorkerDeps.OnFillDropped. Nil is tolerated on the same terms as OnRefused.
	OnDropped func(mic, orderID, reason string)
	// Logger is where the ERROR goes. Nil falls back to the default logger rather
	// than dropping the loudest half of the refusal.
	Logger *slog.Logger
}

// Refuse records the refusal of one execution report.
func (r ReportRefusal) Refuse(orderID string, reason error) {
	if r.OnRefused != nil {
		r.OnRefused(r.Venue, orderID, RefusalKind(reason))
	}
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error("VENUE EXECUTION REPORT REFUSED — this adapter will not publish it as a fill, and "+
		"the order is being frozen. The venue and this platform disagree about what was authorised, "+
		"and no re-drive resolves that: a human must compare it against the exchange's own order history",
		"venue", r.Venue, "order_id", orderID, "reason", reason)
	if r.Orders != nil {
		r.Orders.Quarantined(&orderpb.OrderState{OrderId: orderID}, reason.Error())
	}
}

// The two reasons an execution report is DROPPED rather than refused (#1047) —
// the counter labels for ReportRefusal.Dropped.
//
// BOUNDED BY CONSTRUCTION, like RefusalKind and for the same reason: a label
// derived from an exchange's error text would let a venue mint unbounded
// Prometheus cardinality.
const (
	// DropUnknownOrder: the view answered, and this adapter does not hold the
	// order. Routine — a shared exchange account and a second replica both
	// produce reports that are not ours — and NOT a fault. It is counted anyway,
	// because it is the baseline DropStoreError is read against: an adapter that
	// normally sees a steady trickle of other people's orders and an adapter that
	// has just gone blind are otherwise the same rising line.
	DropUnknownOrder = "unknown_order"
	// DropStoreError: the order view could not be READ, so this adapter cannot
	// say whether it holds the order. Every increment is an execution that
	// reached no FACT, no position projection and no ledger entry, and that the
	// OMS sweep can only recover while the order is still non-terminal.
	DropStoreError = "store_error"
)

// Dropped records ONE execution report that did not become a fill FACT because
// this adapter could not RESOLVE it to an order it holds (#1047).
//
// # Why this is not Refuse
//
// Refuse answers a report the adapter UNDERSTOOD and will not honour: the venue
// and the platform disagree about what was authorised, no re-drive resolves it,
// and the order is frozen so that a re-dispatch cannot work a size nobody can
// state. A drop is the opposite situation — nothing was understood, because the
// report was never matched to an order — so there are three differences and each
// one has a reason:
//
//   - NO FREEZE. Quarantine is a claim that a specific order is contradicted.
//     On DropStoreError the adapter cannot even confirm it holds the order, and
//     the freeze is a WRITE to the store whose READ just failed, so it would
//     mostly fail on its way to onErr. Worse if it succeeded: a store outage
//     touches every in-flight order at once, so freezing on it would convert a
//     transient blip into a quarantine across the whole book that a human must
//     clear order by order. On DropUnknownOrder there is nothing to freeze — the
//     order is somebody else's.
//   - ITS OWN COUNTER, not the refusal counter. "The venue over-filled us" and
//     "we could not read our own view" are different incidents with different
//     responses, and one series carrying both cannot be alerted on either.
//   - THE CALLER DECIDES WHAT TO DO WITH THE FRAME, and the two reasons differ
//     there too: an unknown order is skipped, a store error is returned. See the
//     ingesters.
//
// # Why the log level differs by reason
//
// DropUnknownOrder is ordinary traffic and an ERROR per frame would be noise
// that trains an operator to ignore the line — the counter is its whole signal.
// DropStoreError is a lost execution, so it gets the ERROR, and the message
// names what was lost rather than what was degraded.
func (r ReportRefusal) Dropped(orderID, reason string) {
	if r.OnDropped != nil {
		r.OnDropped(r.Venue, orderID, reason)
	}
	if reason == DropUnknownOrder {
		return
	}
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error("VENUE EXECUTION REPORT DROPPED — this adapter could not resolve it to an order "+
		"it holds, so no fill FACT was published: the position book has not booked this trade and "+
		"the accounting ledger has not journalled its cash. The OMS sweep re-queries the venue and "+
		"can adopt the execution ONLY while the order is still working — an order the venue "+
		"completed during this outage lands terminal and the missed execution is unreachable",
		"venue", r.Venue, "order_id", orderID, "reason", reason)
}

// RefusalKind is the counter LABEL for a refusal.
//
// BOUNDED BY CONSTRUCTION, and deliberately not the error text: an exchange
// controls what its error strings contain, and a label taken from one would let
// a venue mint unbounded Prometheus cardinality by putting an order id in a
// message. Anything unrecognised is "other" rather than dropped — a refusal this
// table has not learned to name is still a refusal that happened.
func RefusalKind(err error) string {
	switch {
	case errors.Is(err, ErrVenueOverfill):
		return "overfill"
	case errors.Is(err, ErrLeavesUnrepresentable):
		return "unrepresentable"
	default:
		return "other"
	}
}
