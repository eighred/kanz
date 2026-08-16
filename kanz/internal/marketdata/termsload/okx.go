package termsload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/exchange/netdial"
	"github.com/eighred/kanz/internal/marketdata/terms"
)

// InstType is the OKX catalogue partition to load.
//
// TYPED RATHER THAN A STRING because the two that are NOT here matter. OKX's
// "SWAP" is a PERPETUAL swap — a linear/inverse futures product with no expiry —
// and reference.v1.SwapTerms is an INTEREST-RATE swap with pay/receive legs, a
// fixed rate and a day count. They share a word and nothing else. Passing "SWAP"
// through as a free string is a one-character way to file perpetual futures under
// KindSwap, where nothing would ever look for them and ChainAsOf would never
// return them. "SPOT" is excluded for the plainer reason that spot has no
// contract terms at all.
type InstType string

const (
	// OptionInstruments is OKX's listed option chain.
	OptionInstruments InstType = "OPTION"
	// FutureInstruments is OKX's DATED futures — the ones that expire. Perpetuals
	// are instType=SWAP and are deliberately not loadable here.
	FutureInstruments InstType = "FUTURES"
)

// okxSettlementType is what every OKX dated contract settles as.
//
// A VENUE FACT WITH A DATE ON IT, not something the response says. OKX lists no
// physically-delivered contract: a linear future settles in USDT and an inverse
// one in the base coin, and both are cash settlements against an index. The
// catalogue has no field that would say otherwise — ctType distinguishes linear
// from inverse, which is a margining question, not a delivery one — so if OKX
// ever lists a delivered contract this constant becomes wrong SILENTLY.
//
// It is written as a constant with this comment rather than derived from a field,
// because deriving it from ctType would look like evidence and would not be.
// reference.v1.FutureTerms requires the field, and "PHYSICAL" would be the answer
// that changes how a position is closed out.
const okxSettlementType = "CASH"

// okxCatalogueLimit bounds the response body read. A full BTC option chain is a
// few hundred kilobytes; this is generous by two orders of magnitude and exists
// so a wrong base URL pointing at something that streams cannot exhaust memory.
const okxCatalogueLimit = 32 << 20

// OKXReference reads OKX's public instrument catalogue
// (GET /api/v5/public/instruments) and maps it into contract terms.
//
// THE MAPPING AND OPTIONS ARE FIXED AT CONSTRUCTION, and validated there. A
// mapping that cannot produce a correct load is a misconfiguration, and this
// platform's rule is that a misconfiguration surfaces before the first event
// rather than as a run that fetches a chain and refuses all of it.
type OKXReference struct {
	baseURL string
	http    *http.Client
	mapping Mapping
	opt     Options
}

// NewOKXReference returns a catalogue reader against baseURL.
//
// THE BASE URL IS REQUIRED AND HAS NO DEFAULT — #147's ruling, and the reason it
// was made for OKX applies unchanged: OKX serves demo and production from the
// SAME host and distinguishes them by a request header, so the endpoint alone
// cannot say which book a caller meant. A demo catalogue's strikes and expiries
// are fiction, and a terms row loaded from one is indistinguishable from a real
// one afterwards — it would simply price the live book against invented contracts.
func NewOKXReference(baseURL string, m Mapping, opt Options) (*OKXReference, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("termsload: okx base URL is required and has no default (#147) — it " +
			"selects the environment, and contract terms loaded from the wrong one would price the " +
			"live book against invented strikes")
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &OKXReference{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    netdial.NewHTTPClient(2 * time.Minute),
		mapping: m,
		opt:     opt,
	}, nil
}

// Chain fetches one underlying's contracts of a given type and maps them.
//
// ONE UNDERLYING PER CALL, and uly is required. OKX will answer instType=OPTION
// with no underlying by refusing, and instType=FUTURES with no underlying by
// returning EVERY dated future on the venue — thousands of rows across hundreds
// of underlyings, of which the mapping would resolve a handful and refuse the
// rest. Loadable would then refuse the whole batch, which is correct and useless.
// A catalogue-wide pull is a different operation and should be written as one.
//
// THE NETWORK HALF IS ONLY THIS FUNCTION. Everything that decides what a row
// MEANS is in ParseOKXInstruments, which takes bytes — so every mapping rule is
// provable from a fixture and no test in this package opens a socket to OKX.
func (r *OKXReference) Chain(ctx context.Context, it InstType, uly string) (Batch, error) {
	if strings.TrimSpace(uly) == "" {
		return Batch{}, errors.New("termsload: an underlying is required — a catalogue-wide pull " +
			"returns every contract on the venue, of which the mapping resolves a handful, and the " +
			"all-or-nothing rule then refuses the batch")
	}
	switch it {
	case OptionInstruments, FutureInstruments:
	default:
		return Batch{}, fmt.Errorf("termsload: %q is not an instrument type this loader maps; "+
			"OKX's SWAP is a PERPETUAL, not the interest-rate swap reference.v1.SwapTerms "+
			"describes, and SPOT has no contract terms", it)
	}

	q := url.Values{"instType": {string(it)}, "uly": {uly}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		r.baseURL+"/api/v5/public/instruments?"+q.Encode(), nil)
	if err != nil {
		return Batch{}, fmt.Errorf("termsload: build request: %w", err)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return Batch{}, fmt.Errorf("termsload: okx instruments %s %s: %w", it, uly, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Batch{}, fmt.Errorf("termsload: okx instruments %s %s: HTTP %d", it, uly, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, okxCatalogueLimit))
	if err != nil {
		return Batch{}, fmt.Errorf("termsload: read okx instruments %s %s: %w", it, uly, err)
	}
	b, err := ParseOKXInstruments(body, r.mapping, r.opt)
	if err != nil {
		return Batch{}, fmt.Errorf("termsload: okx instruments %s %s: %w", it, uly, err)
	}
	return b, nil
}

// okxInstrument is one row of /api/v5/public/instruments.
//
// EVERY NUMERIC FIELD IS A STRING, which is what lets them be parsed exactly —
// the same property backfill relies on for candles, and it matters more here: a
// strike is not a price this platform observed, it is a term of a contract, and
// rounding one in transit prices every option in the chain against a contract
// that does not exist.
// TWO FIELDS ARE DELIBERATELY NOT READ. `instFamily` duplicates `uly` for every
// product this loads, so mapping on both would give an operator two places to
// configure the same thing and one of them would drift. `state` (live / preopen /
// suspend / expired) is a TRADING status, not a term: an expired option's strike
// is still what it was, and this store is point-in-time precisely so a read as of
// a past date resolves it. Filtering on state would delete history the moment a
// chain rolled.
type okxInstrument struct {
	InstType  string `json:"instType"`
	InstID    string `json:"instId"`
	Uly       string `json:"uly"`
	OptType   string `json:"optType"`
	Stk       string `json:"stk"`
	ExpTime   string `json:"expTime"`
	ListTime  string `json:"listTime"`
	CtVal     string `json:"ctVal"`
	CtMult    string `json:"ctMult"`
	CtValCcy  string `json:"ctValCcy"`
	TickSz    string `json:"tickSz"`
	SettleCcy string `json:"settleCcy"`
}

// ParseOKXInstruments maps a catalogue response body into a Batch.
//
// # AS_OF IS THE VENUE'S listTime
//
// This is the decision that matters most in this package, because Put's primary
// key is (instrument_id, as_of) and a re-put of the same key is silently dropped.
//
// as_of is EFFECTIVE time, not knowledge time — contract.proto calls it "the
// effective time of this terms record" and terms' own header says "a restatement
// is a new row at a newer as_of, never an overwrite". A listed option's terms
// become effective when the venue lists it, and listTime is the only effective
// time OKX reports. So that is what is stored.
//
// The alternative — stamping the run's clock — defeats the primary key outright.
// Every nightly run would insert a complete duplicate of every chain under a new
// as_of, ChainAsOf's DISTINCT ON would then be choosing between identical rows by
// which run wrote them, and a GENUINE amendment (a corporate action adjusting a
// strike, which terms' header names as the reason the axis exists) would be
// indistinguishable from last night's re-run. Idempotency is what Put's ON
// CONFLICT DO NOTHING is FOR, and a per-run as_of is the one choice that makes it
// dead code.
//
// It also gives the honest point-in-time answer: a read as of a date before the
// contract was listed correctly finds nothing.
//
// THE COST, STATED: a row written from a bad parse cannot be corrected by fixing
// the parser and re-running — the second run presents the same key and the store
// keeps the first, immutable, wrong row. Options.AsOfOverride is the lever that
// supersedes it, and it exists because that case is not hypothetical for the
// first writer this store has ever had.
//
// # Envelope errors arrive inside an HTTP 200
//
// OKX carries refusals — an unknown underlying, a rate limit — in the response
// body's code field. A caller checking only the status would read a refusal as an
// empty catalogue, which is indistinguishable from "this underlying lists no
// options" and would report a successful load of nothing.
func ParseOKXInstruments(body []byte, m Mapping, opt Options) (Batch, error) {
	if err := m.validate(); err != nil {
		return Batch{}, err
	}
	var env struct {
		Code string          `json:"code"`
		Msg  string          `json:"msg"`
		Data []okxInstrument `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return Batch{}, fmt.Errorf("termsload: decode okx instruments: %w", err)
	}
	if env.Code != "0" {
		return Batch{}, fmt.Errorf("termsload: okx refused inside a 200: code %s: %s", env.Code, env.Msg)
	}

	var b Batch
	for _, row := range env.Data {
		rec, reason := mapOKXRow(row, m, opt)
		if reason != "" {
			b.Refused = append(b.Refused, Refusal{InstID: row.InstID, Reason: reason})
			continue
		}
		b.Mapped = append(b.Mapped, rec)
	}
	return b, nil
}

// mapOKXRow maps one catalogue row, or returns the reason it will not.
//
// A NON-EMPTY REASON IS THE ONLY FAILURE CHANNEL, and no path returns a partly
// filled Record. The order of the checks is the order an operator can act on:
// identity, then what kind of thing it is, then the mapping they own, then the
// venue's numbers.
func mapOKXRow(row okxInstrument, m Mapping, opt Options) (terms.Record, string) {
	if strings.TrimSpace(row.InstID) == "" {
		return terms.Record{}, "the row carries no instId, so it names no contract"
	}

	var kind terms.Kind
	switch InstType(row.InstType) {
	case OptionInstruments:
		kind = terms.KindOption
	case FutureInstruments:
		kind = terms.KindFuture
	default:
		// NOT SKIPPED. A row of an unexpected type in a response the caller asked
		// to be one type is either a venue change or a wrong request, and dropping
		// it quietly would make either look like a shorter chain.
		return terms.Record{}, fmt.Sprintf("instType %q is not a contract type this loader maps "+
			"(OKX's SWAP is a perpetual, not an interest-rate swap)", row.InstType)
	}

	underlyingID, ok := m.Underlying[row.Uly]
	if !ok || strings.TrimSpace(underlyingID) == "" {
		return terms.Record{}, fmt.Sprintf("no canonical instrument is mapped to OKX underlying %q; "+
			"the underlying id is what the Greeks layer fetches a spot price with, and a guessed one "+
			"either drops the contract out of every measure or prices it against a different "+
			"instrument entirely", row.Uly)
	}

	asOf, reason := resolveAsOf(row, opt)
	if reason != "" {
		return terms.Record{}, reason
	}

	instrumentID := m.contractID(row.InstID)
	ct := &referencepb.ContractTerms{
		// SET HERE, because nothing else sets them: Put reads Record's own fields
		// for the columns and stores the message as an opaque blob, so a message
		// left without its instrument_id and as_of decodes later as terms that
		// belong to nobody. scanRecord hands the blob straight to a caller.
		InstrumentId: instrumentID,
		AsOf:         timestamppb.New(asOf),
	}

	switch kind {
	case terms.KindOption:
		o, oreason := okxOptionTerms(row, underlyingID)
		if oreason != "" {
			return terms.Record{}, oreason
		}
		ct.Terms = &referencepb.ContractTerms_Option{Option: o}
	default:
		f, freason := okxFutureTerms(row, underlyingID)
		if freason != "" {
			return terms.Record{}, freason
		}
		ct.Terms = &referencepb.ContractTerms_Future{Future: f}
	}

	return terms.Record{
		InstrumentID: instrumentID,
		AsOf:         asOf,
		UnderlyingID: underlyingID,
		Kind:         kind,
		Terms:        ct,
	}, ""
}

// resolveAsOf returns the record's effective time — see ParseOKXInstruments.
func resolveAsOf(row okxInstrument, opt Options) (time.Time, string) {
	if !opt.AsOfOverride.IsZero() {
		return opt.AsOfOverride.UTC(), ""
	}
	return okxMillis(row.ListTime, "listing time (listTime)")
}

// okxOptionTerms maps an OPTION row.
//
// EVERY FIELD termsource.toSpec CHECKS IS CHECKED HERE, on purpose. toSpec
// refuses a non-positive strike, a non-positive multiplier, an absent expiry and
// an OPTION_TYPE_UNSPECIFIED — and its refusal is INVISIBLE at the point of
// refusal: the option is priced linearly and a missing-terms counter increments,
// forever, for a row that was loaded. Refusing here instead makes it a line an
// operator reads once.
func okxOptionTerms(row okxInstrument, underlyingID string) (*referencepb.OptionTerms, string) {
	// STRIKE IS IN THE QUOTE CURRENCY, UNSCALED. OKX's stk for BTC-USD-241227-
	// 60000-C is "60000" and means 60000 USD per unit of BTC — the same units the
	// underlying's spot price is quoted in, which is what Black-Scholes requires.
	// It is NOT per contract: multiplying it by ctVal (0.01) would produce a
	// deep-in-the-money strike of 600, and every option in the chain would price
	// as intrinsic value with no time value and a delta pinned at 1.
	strike, reason := positiveDecimal(row.Stk, "strike (stk)")
	if reason != "" {
		return nil, reason
	}
	mult, reason := okxContractUnits(row)
	if reason != "" {
		return nil, reason
	}
	expiry, reason := okxMillis(row.ExpTime, "expiry (expTime)")
	if reason != "" {
		return nil, reason
	}
	typ, reason := okxOptionType(row.OptType)
	if reason != "" {
		return nil, reason
	}
	return &referencepb.OptionTerms{
		UnderlyingId: underlyingID,
		Strike:       strike,
		Expiry:       timestamppb.New(expiry),
		OptionType:   typ,
		// SET EXPLICITLY THOUGH EUROPEAN IS THE DEFAULT-SHAPED ANSWER. OKX's
		// crypto options are European-exercise, which is a venue fact. Leaving the
		// field UNSPECIFIED would also produce European pricing, because toSpec
		// maps everything that is not AMERICAN to European — so the wrong value
		// and the unset one behave identically today and would diverge the moment
		// an American-style venue is loaded by a reader who trusted the field.
		ExerciseStyle:      referencepb.ExerciseStyle_EXERCISE_STYLE_EUROPEAN,
		ContractMultiplier: mult,
	}, ""
}

// okxFutureTerms maps a FUTURES row.
//
// CHEAP BECAUSE THE HARD PARTS ARE SHARED — the underlying mapping, the
// millisecond expiry and the contract-size arithmetic are the same three
// decisions as an option's, minus the strike and the put/call. What it buys is
// that a dated future stops being an instrument with no terms at all.
func okxFutureTerms(row okxInstrument, underlyingID string) (*referencepb.FutureTerms, string) {
	expiry, reason := okxMillis(row.ExpTime, "expiry (expTime)")
	if reason != "" {
		return nil, reason
	}
	size, reason := okxContractUnits(row)
	if reason != "" {
		return nil, reason
	}
	if strings.TrimSpace(row.SettleCcy) == "" {
		return nil, "settleCcy is absent, so what the contract settles in is unknown; a dated " +
			"contract with no settlement currency is either a perpetual or a row this loader does " +
			"not understand, and calling it CASH would be an assertion about neither"
	}
	ft := &referencepb.FutureTerms{
		UnderlyingId:   underlyingID,
		Expiry:         timestamppb.New(expiry),
		ContractSize:   size,
		SettlementType: okxSettlementType,
	}
	// TICK SIZE IS OPTIONAL IN THE PROTO, so an absent one is left unset. A
	// PRESENT-BUT-UNPARSEABLE one is refused rather than dropped: that is the
	// venue saying something this loader did not understand, and silently
	// discarding it is how a field stops being noticed.
	if strings.TrimSpace(row.TickSz) != "" {
		tick, treason := positiveDecimal(row.TickSz, "tick size (tickSz)")
		if treason != "" {
			return nil, treason
		}
		ft.TickSize = tick
	}
	return ft, ""
}

// okxOptionType maps OKX's optType onto the wire enum.
//
// THERE IS NO DEFAULT BRANCH THAT GUESSES. termsource.toSpec says what a guess
// costs — "guessing call would misprice every put by the whole put-call parity
// gap, in the direction that understates risk on a short book" — and a loader
// that defaulted would hand it a confident CALL with nothing to indicate the
// venue never said so.
func okxOptionType(s string) (referencepb.OptionType, string) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "C":
		return referencepb.OptionType_OPTION_TYPE_CALL, ""
	case "P":
		return referencepb.OptionType_OPTION_TYPE_PUT, ""
	default:
		return referencepb.OptionType_OPTION_TYPE_UNSPECIFIED, fmt.Sprintf(
			"optType %q is neither C nor P, so whether this contract is a call or a put is unknown", s)
	}
}

// okxContractUnits returns the number of UNDERLYING UNITS per contract, which is
// what reference.v1 means by contract_multiplier and contract_size.
//
// # ctVal × ctMult, and the units are ctValCcy
//
// OKX reports a contract's size as a value (ctVal) in a stated currency
// (ctValCcy) with a multiplier (ctMult). For BTC options that is ctVal="0.01",
// ctValCcy="BTC", ctMult="1" — one contract is 0.01 BTC. The product is the
// figure the Greek aggregation scales a position by, and getting it wrong is
// silent in both directions: too small and the book reports a fraction of its
// real delta, too large and it reports a multiple. Neither errors, and neither
// looks like a data problem — it looks like the position size.
//
// ctValCcy MUST BE PRESENT. Without it the product is a number with no units,
// and "underlying units per contract" is precisely a claim about units. This
// does not verify that the currency IS the underlying's base asset, because
// nothing on this platform can: InstrumentReference.base_asset exists in the
// proto and is never populated. That check belongs here the day it can be made.
func okxContractUnits(row okxInstrument) (*commonpb.Decimal, string) {
	if strings.TrimSpace(row.CtValCcy) == "" {
		return nil, "ctValCcy is absent, so the contract value has no stated units and cannot be " +
			"read as underlying units per contract — the figure every Greek is scaled by"
	}
	ctVal, reason := positiveRat(row.CtVal, "contract value (ctVal)")
	if reason != "" {
		return nil, reason
	}
	ctMult, reason := positiveRat(row.CtMult, "contract multiplier (ctMult)")
	if reason != "" {
		return nil, reason
	}
	units, ok := dec.ToProtoScaled(new(big.Rat).Mul(ctVal, ctMult))
	if !ok {
		return nil, fmt.Sprintf("contract size ctVal(%s) × ctMult(%s) is not representable as an "+
			"exact Decimal", row.CtVal, row.CtMult)
	}
	return units, ""
}

// positiveRat parses a venue decimal string exactly and refuses a non-positive
// or absent one. field names it the way OKX does, so a refusal can be looked up
// in the venue's own documentation rather than translated.
func positiveRat(s, field string) (*big.Rat, string) {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil, field + " is absent"
	}
	r, err := dec.ParseRat(t)
	if err != nil {
		return nil, fmt.Sprintf("%s %q is not a decimal number", field, s)
	}
	if r.Sign() <= 0 {
		// A ZERO IS REFUSED, NOT STORED. termsource.toSpec turns a zero strike or
		// multiplier into ok=false and a missing-terms count — the row exists, the
		// option prices linearly, and the operator is looking for a load that
		// never ran. Refusing here is the same outcome made findable.
		return nil, fmt.Sprintf("%s %q is not positive", field, s)
	}
	return r, ""
}

// positiveDecimal is positiveRat rendered as the wire decimal.
func positiveDecimal(s, field string) (*commonpb.Decimal, string) {
	r, reason := positiveRat(s, field)
	if reason != "" {
		return nil, reason
	}
	d, ok := dec.ToProtoScaled(r)
	if !ok {
		return nil, fmt.Sprintf("%s %q is not representable as an exact Decimal", field, s)
	}
	return d, ""
}

// okxMillis parses one of OKX's Unix-millisecond timestamp strings.
//
// MILLISECONDS, NOT SECONDS, and the failure mode of getting that wrong is why
// this is one function rather than an inline ParseInt at each site: 1735286400000
// read as seconds is the year 56939, and an option expiring in fifty thousand
// years has effectively infinite time value — a call would price at very nearly
// spot, with a delta of 1, and nothing about the number would look wrong.
func okxMillis(s, field string) (time.Time, string) {
	t := strings.TrimSpace(s)
	if t == "" {
		return time.Time{}, field + " is absent"
	}
	ms, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Sprintf("%s %q is not a Unix millisecond timestamp", field, s)
	}
	if ms <= 0 {
		return time.Time{}, fmt.Sprintf("%s %q is not a positive Unix millisecond timestamp", field, s)
	}
	return time.UnixMilli(ms).UTC(), ""
}
