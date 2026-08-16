package termsload

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/marketdata/terms"
)

// The two instants the fixtures use, named so a failure message is readable.
var (
	listed = time.Date(2024, 11, 1, 8, 0, 0, 0, time.UTC)
	expiry = time.Date(2024, 12, 27, 8, 0, 0, 0, time.UTC)
)

// mapping is the operator-supplied configuration every test starts from: OKX
// calls the underlying "BTC-USD", this platform calls the spot instrument
// "BTC-USDT". THEY DIFFER ON PURPOSE — an identity mapping would let a bug that
// passes the venue's own string straight through go unnoticed in every assertion
// below.
func mapping() Mapping {
	return Mapping{
		Underlying:        map[string]string{"BTC-USD": "BTC-USDT"},
		ContractNamespace: "XOKX",
	}
}

// okxOption renders one OPTION catalogue row in OKX's own shape: every numeric
// field a quoted string, timestamps in Unix milliseconds.
func okxOption(instID, uly, optType, stk string, expMs, listMs int64) string {
	return fmt.Sprintf(`{"instType":"OPTION","instId":%q,"uly":%q,"instFamily":%q,`+
		`"optType":%q,"stk":%q,"expTime":"%d","listTime":"%d",`+
		`"ctVal":"0.01","ctMult":"1","ctValCcy":"BTC","tickSz":"0.0005",`+
		`"settleCcy":"BTC","state":"live"}`,
		instID, uly, uly, optType, stk, expMs, listMs)
}

// okxFuture renders one FUTURES catalogue row.
func okxFuture(instID, uly string, expMs, listMs int64) string {
	return fmt.Sprintf(`{"instType":"FUTURES","instId":%q,"uly":%q,"instFamily":%q,`+
		`"ctType":"inverse","expTime":"%d","listTime":"%d",`+
		`"ctVal":"100","ctMult":"1","ctValCcy":"USD","tickSz":"0.1",`+
		`"settleCcy":"BTC","state":"live"}`,
		instID, uly, uly, expMs, listMs)
}

func okxCatalogue(rows ...string) []byte {
	return []byte(`{"code":"0","msg":"","data":[` + strings.Join(rows, ",") + `]}`)
}

// chain is the realistic fixture: a two-strike, both-sides BTC option chain.
func chain() []byte {
	return okxCatalogue(
		okxOption("BTC-USD-241227-60000-C", "BTC-USD", "C", "60000", expiry.UnixMilli(), listed.UnixMilli()),
		okxOption("BTC-USD-241227-60000-P", "BTC-USD", "P", "60000", expiry.UnixMilli(), listed.UnixMilli()),
		okxOption("BTC-USD-241227-90000-C", "BTC-USD", "C", "90000", expiry.UnixMilli(), listed.UnixMilli()),
	)
}

func parse(t *testing.T, body []byte) Batch {
	t.Helper()
	b, err := ParseOKXInstruments(body, mapping(), Options{})
	if err != nil {
		t.Fatalf("ParseOKXInstruments: %v", err)
	}
	return b
}

// byID indexes a batch so an assertion names the contract it is about.
func byID(recs []terms.Record) map[string]terms.Record {
	out := make(map[string]terms.Record, len(recs))
	for _, r := range recs {
		out[r.InstrumentID] = r
	}
	return out
}

// A REALISTIC OPTION CHAIN BECOMES THE RECORDS THE PRICER NEEDS.
//
// Every field asserted here is one termsource.toSpec reads: a wrong one does not
// fail, it prices. The strike in particular is asserted EXACTLY — OKX sends it as
// a string so it survives transport, and a float anywhere in this path would
// return a number that is right to eight places and wrong afterwards.
func TestARealisticOptionChainParsesIntoStorableRecords(t *testing.T) {
	b := parse(t, chain())

	if len(b.Refused) != 0 {
		t.Fatalf("a well-formed chain produced refusals: %v", b.Refused)
	}
	if len(b.Mapped) != 3 {
		t.Fatalf("mapped %d rows, want 3", len(b.Mapped))
	}

	recs := byID(b.Mapped)
	rec, ok := recs["XOKX:BTC-USD-241227-60000-C"]
	if !ok {
		t.Fatalf("no record under the namespaced contract id; got %v", keys(recs))
	}
	if rec.Kind != terms.KindOption {
		t.Errorf("kind = %q, want OPTION — a filtered ChainAsOf would never return this row", rec.Kind)
	}
	if rec.UnderlyingID != "BTC-USDT" {
		t.Errorf("underlying_id = %q, want the CANONICAL spot id BTC-USDT; %q is what OKX calls it "+
			"and the venue's string reached the column", rec.UnderlyingID, rec.UnderlyingID)
	}
	if !rec.AsOf.Equal(listed) {
		t.Errorf("as_of = %s, want the venue's listTime %s", rec.AsOf, listed)
	}

	opt := rec.Terms.GetOption()
	if opt == nil {
		t.Fatalf("the record carries no OptionTerms; oneof = %T", rec.Terms.GetTerms())
	}
	if got := dec.FromProto(opt.GetStrike()).RatString(); got != "60000" {
		t.Errorf("strike = %s, want exactly 60000", got)
	}
	if got := dec.FromProto(opt.GetContractMultiplier()).FloatString(2); got != "0.01" {
		t.Errorf("contract_multiplier = %s, want 0.01 — ctVal(0.01) × ctMult(1) in units of the "+
			"underlying, which is the figure every Greek is scaled by", got)
	}
	if !opt.GetExpiry().AsTime().Equal(expiry) {
		t.Errorf("expiry = %s, want %s — expTime is Unix MILLISECONDS", opt.GetExpiry().AsTime(), expiry)
	}
	if opt.GetUnderlyingId() != "BTC-USDT" {
		t.Errorf("OptionTerms.underlying_id = %q, want BTC-USDT — this is the id the Greeks layer "+
			"fetches a spot price with", opt.GetUnderlyingId())
	}
	if opt.GetExerciseStyle() != referencepb.ExerciseStyle_EXERCISE_STYLE_EUROPEAN {
		t.Errorf("exercise_style = %v, want EUROPEAN set EXPLICITLY; UNSPECIFIED prices the same "+
			"today and stops doing so the day an American-style venue is loaded", opt.GetExerciseStyle())
	}

	// THE MESSAGE MUST IDENTIFY ITSELF. Put stores it as an opaque blob and
	// scanRecord hands it straight back, so a ContractTerms without its own
	// instrument_id and as_of decodes later as terms belonging to nobody.
	if rec.Terms.GetInstrumentId() != rec.InstrumentID {
		t.Errorf("ContractTerms.instrument_id = %q, want %q", rec.Terms.GetInstrumentId(), rec.InstrumentID)
	}
	if !rec.Terms.GetAsOf().AsTime().Equal(rec.AsOf) {
		t.Errorf("ContractTerms.as_of = %s, want %s", rec.Terms.GetAsOf().AsTime(), rec.AsOf)
	}
}

// THE PUT/CALL MAP IS ASSERTED BOTH WAYS, IN ONE TEST, BECAUSE A SWAPPED MAP IS
// INVISIBLE EVERYWHERE ELSE.
//
// Every other assertion in this file — strike, expiry, multiplier, underlying,
// count — passes unchanged if C and P are exchanged. The chain still has the
// right number of rows with the right strikes; only the RIGHT each contract
// confers is inverted. termsource.toSpec states the cost: mistaking a put for a
// call misprices it by the whole put-call parity gap, in the direction that
// understates risk on a short book.
//
// So both directions are checked, and a same-strike pair is used so nothing but
// optType distinguishes the two rows.
func TestTheCallAndPutMappingIsAssertedInBothDirections(t *testing.T) {
	b := parse(t, chain())
	recs := byID(b.Mapped)

	call := recs["XOKX:BTC-USD-241227-60000-C"].Terms.GetOption()
	put := recs["XOKX:BTC-USD-241227-60000-P"].Terms.GetOption()
	if call == nil || put == nil {
		t.Fatalf("the same-strike call/put pair did not both map: %v", keys(recs))
	}
	if call.GetOptionType() != referencepb.OptionType_OPTION_TYPE_CALL {
		t.Errorf("optType C mapped to %v, want CALL — the map is swapped", call.GetOptionType())
	}
	if put.GetOptionType() != referencepb.OptionType_OPTION_TYPE_PUT {
		t.Errorf("optType P mapped to %v, want PUT — the map is swapped", put.GetOptionType())
	}
	// AND THEY MUST DIFFER. Both assertions above still pass if one of them is
	// wrong in a way the other compensates for; this pins that the two rows are
	// not being given the same type.
	if call.GetOptionType() == put.GetOptionType() {
		t.Errorf("the call and the put both mapped to %v", call.GetOptionType())
	}
}

// AN UNMAPPED UNDERLYING IS REFUSED, NOT GUESSED.
//
// OKX's uly here is "ETH-USD", which is not in the operator's map. Taking it as a
// canonical id would be the tempting move — it has the shape of one — and it is
// exactly the trap: "BTC-USD" IS a canonical id on this platform (USDT-quoted, at
// that), so the guess would sometimes resolve to a real, different instrument and
// price the whole chain against its spot.
func TestAnUnmappedUnderlyingIsRefusedRatherThanGuessed(t *testing.T) {
	b := parse(t, okxCatalogue(
		okxOption("ETH-USD-241227-3000-C", "ETH-USD", "C", "3000", expiry.UnixMilli(), listed.UnixMilli()),
	))

	if len(b.Mapped) != 0 {
		t.Fatalf("an unmapped underlying produced a record: underlying_id = %q",
			b.Mapped[0].UnderlyingID)
	}
	if len(b.Refused) != 1 {
		t.Fatalf("got %d refusals, want 1", len(b.Refused))
	}
	if !strings.Contains(b.Refused[0].Reason, "ETH-USD") {
		t.Errorf("the refusal does not name the underlying the operator must map: %s", b.Refused[0].Reason)
	}
	if b.Refused[0].InstID != "ETH-USD-241227-3000-C" {
		t.Errorf("the refusal names %q, not the venue instId an operator can look up", b.Refused[0].InstID)
	}
}

// A ROW THAT WOULD PRICE WRONG IS REFUSED, NOT DEFAULTED.
//
// Each of these passes terms.Postgres.Put's own validation — Put only checks that
// an instrument_id and a payload exist — and each then fails termsource.toSpec,
// which has no error channel. The result is not an error anyone sees: the option
// is priced LINEARLY and a missing-terms counter increments forever, for an
// instrument whose terms were loaded. That is strictly worse than no row, which is
// why every one of these is refused here.
func TestARowThatWouldPriceWrongIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  string
		want string
	}{
		{
			name: "a zero strike prices as a forward",
			row:  okxOption("BTC-USD-241227-0-C", "BTC-USD", "C", "0", expiry.UnixMilli(), listed.UnixMilli()),
			want: "strike",
		},
		{
			name: "an absent strike is not a zero one",
			row:  okxOption("BTC-USD-241227-X-C", "BTC-USD", "C", "", expiry.UnixMilli(), listed.UnixMilli()),
			want: "strike",
		},
		{
			name: "an unparseable expiry must not become the zero time",
			row: strings.Replace(
				okxOption("BTC-USD-241227-60000-C", "BTC-USD", "C", "60000", expiry.UnixMilli(), listed.UnixMilli()),
				`"expTime":"`+fmt.Sprint(expiry.UnixMilli())+`"`, `"expTime":"soon"`, 1),
			want: "expTime",
		},
		{
			name: "an optType the venue does not document is not a call",
			row:  okxOption("BTC-USD-241227-60000-X", "BTC-USD", "X", "60000", expiry.UnixMilli(), listed.UnixMilli()),
			want: "optType",
		},
		{
			name: "a zero contract multiplier makes every Greek zero",
			row: strings.Replace(
				okxOption("BTC-USD-241227-60000-C", "BTC-USD", "C", "60000", expiry.UnixMilli(), listed.UnixMilli()),
				`"ctVal":"0.01"`, `"ctVal":"0"`, 1),
			want: "ctVal",
		},
		{
			name: "a contract value with no units is not a multiplier",
			row: strings.Replace(
				okxOption("BTC-USD-241227-60000-C", "BTC-USD", "C", "60000", expiry.UnixMilli(), listed.UnixMilli()),
				`"ctValCcy":"BTC"`, `"ctValCcy":""`, 1),
			want: "ctValCcy",
		},
		{
			name: "a row with no listTime has no effective time to store it at",
			row: strings.Replace(
				okxOption("BTC-USD-241227-60000-C", "BTC-USD", "C", "60000", expiry.UnixMilli(), listed.UnixMilli()),
				`"listTime":"`+fmt.Sprint(listed.UnixMilli())+`"`, `"listTime":""`, 1),
			want: "listTime",
		},
		{
			name: "a perpetual is not an interest-rate swap",
			row: strings.Replace(
				okxOption("BTC-USD-SWAP", "BTC-USD", "C", "60000", expiry.UnixMilli(), listed.UnixMilli()),
				`"instType":"OPTION"`, `"instType":"SWAP"`, 1),
			want: "instType",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := parse(t, okxCatalogue(tc.row))
			if len(b.Mapped) != 0 {
				t.Fatalf("the row was mapped instead of refused: %+v", b.Mapped[0])
			}
			if len(b.Refused) != 1 {
				t.Fatalf("got %d refusals, want exactly 1 — the row was dropped silently",
					len(b.Refused))
			}
			if !strings.Contains(b.Refused[0].Reason, tc.want) {
				t.Errorf("the refusal does not name %q, so an operator cannot act on it: %s",
					tc.want, b.Refused[0].Reason)
			}
		})
	}
}

// A PARTIAL CHAIN IS NOT STORED AT ALL.
//
// Put's own doc gives the rule and the reason: it writes in one transaction "so a
// chain load is all-or-nothing: a partially loaded chain would calibrate a surface
// from some of an underlying's strikes, which fits cleanly and is wrong". Handing
// it the rows that happened to map would honour the transaction and defeat the
// point of it — volsurface fits what it is given, so a chain missing its wings
// produces a smooth surface with no skew rather than an error.
//
// The refusals are still REPORTED, because an operator who fixes one row per run
// to discover the next is an operator who stops running the loader.
func TestAPartialChainRefusesTheWholeBatchAndStillNamesEveryRefusal(t *testing.T) {
	body := okxCatalogue(
		okxOption("BTC-USD-241227-60000-C", "BTC-USD", "C", "60000", expiry.UnixMilli(), listed.UnixMilli()),
		okxOption("BTC-USD-241227-70000-C", "BTC-USD", "C", "0", expiry.UnixMilli(), listed.UnixMilli()),
		okxOption("ETH-USD-241227-3000-C", "ETH-USD", "C", "3000", expiry.UnixMilli(), listed.UnixMilli()),
	)
	b := parse(t, body)

	if len(b.Mapped) != 1 || len(b.Refused) != 2 {
		t.Fatalf("mapped %d and refused %d, want 1 and 2", len(b.Mapped), len(b.Refused))
	}

	recs, err := b.Loadable()
	if err == nil {
		t.Fatalf("Loadable returned %d records for a chain that lost two strikes — volsurface "+
			"would fit that cleanly and be wrong", len(recs))
	}
	if recs != nil {
		t.Errorf("Loadable refused AND returned %d records; a caller ignoring the error would "+
			"store a partial chain", len(recs))
	}
	if !errors.Is(err, ErrPartialChain) {
		t.Errorf("the refusal is not matchable with errors.Is(ErrPartialChain): %v", err)
	}
	// BOTH problems must be in the one message. Reporting the first would make
	// fixing a chain an iterative game against a venue round-trip.
	for _, want := range []string{"BTC-USD-241227-70000-C", "ETH-USD-241227-3000-C"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}

	// AND A CLEAN CHAIN IS LOADABLE, or the rule above would be indistinguishable
	// from a loader that never returns anything.
	clean, err := parse(t, chain()).Loadable()
	if err != nil {
		t.Fatalf("a chain with no refusals was not loadable: %v", err)
	}
	if len(clean) != 3 {
		t.Fatalf("Loadable returned %d records for a clean 3-row chain", len(clean))
	}
}

// AS_OF IS THE VENUE'S listTime, WHICH IS WHAT MAKES A RE-RUN A NO-OP.
//
// Put's primary key is (instrument_id, as_of) with ON CONFLICT DO NOTHING. A
// per-run clock would give every nightly load a fresh as_of for every contract:
// the table would grow by a full chain per run, and ChainAsOf's DISTINCT ON would
// be picking between identical rows by which run wrote them — so a GENUINE
// amendment, the case the as_of axis exists for, would be indistinguishable from
// last night's re-run.
//
// Parsing the same body twice must therefore produce the same key both times.
func TestAsOfComesFromTheVenueSoARerunIsIdempotent(t *testing.T) {
	first, second := parse(t, chain()), parse(t, chain())

	for i := range first.Mapped {
		if !first.Mapped[i].AsOf.Equal(listed) {
			t.Fatalf("as_of = %s, want the venue's listTime %s — a run clock reached the primary key",
				first.Mapped[i].AsOf, listed)
		}
		if !first.Mapped[i].AsOf.Equal(second.Mapped[i].AsOf) {
			t.Fatalf("two parses of the same catalogue produced different as_of values (%s, %s); "+
				"every re-run would insert a duplicate chain",
				first.Mapped[i].AsOf, second.Mapped[i].AsOf)
		}
	}

	// THE OVERRIDE IS THE REPAIR LEVER. A row written from a bad parse cannot be
	// corrected by re-running — the store keeps the first, immutable row — so an
	// operator supersedes it by stating a later effective instant.
	correction := listed.Add(72 * time.Hour)
	fixed, err := ParseOKXInstruments(chain(), mapping(), Options{AsOfOverride: correction})
	if err != nil {
		t.Fatalf("ParseOKXInstruments with an override: %v", err)
	}
	for _, rec := range fixed.Mapped {
		if !rec.AsOf.Equal(correction) {
			t.Errorf("as_of = %s, want the override %s — there would be no way to supersede a "+
				"wrong row", rec.AsOf, correction)
		}
		if !rec.Terms.GetAsOf().AsTime().Equal(correction) {
			t.Errorf("ContractTerms.as_of = %s, want the override %s — the blob and the column "+
				"would disagree", rec.Terms.GetAsOf().AsTime(), correction)
		}
	}
}

// THE CONTRACT ID IS NAMESPACED, AND THE VENUE'S OWN instId IS NOT USED BARE.
//
// A bare instId lives in the same string space as a canonical spot id, so a
// collision would serve one instrument another instrument's terms — a mispricing.
// The namespaced form can only fail the other way: LatestAsOf finds no row for
// whatever id the position carries, the missing-terms observer fires, and the
// option is priced linearly. Wrong, but COUNTED.
func TestTheContractIdIsNamespacedAndOverridable(t *testing.T) {
	b := parse(t, chain())
	for _, rec := range b.Mapped {
		if !strings.HasPrefix(rec.InstrumentID, "XOKX:") {
			t.Errorf("instrument_id = %q has no namespace; a bare venue instId can collide with a "+
				"canonical spot id and serve it the wrong terms", rec.InstrumentID)
		}
	}

	// An operator who HAS named the contract wins over the derived id.
	m := mapping()
	m.Contract = map[string]string{"BTC-USD-241227-60000-C": "BTC-DEC24-60K-CALL"}
	named, err := ParseOKXInstruments(chain(), m, Options{})
	if err != nil {
		t.Fatalf("ParseOKXInstruments: %v", err)
	}
	if _, ok := byID(named.Mapped)["BTC-DEC24-60K-CALL"]; !ok {
		t.Errorf("the operator's explicit contract id was ignored; got %v", keys(byID(named.Mapped)))
	}
}

// A MISCONFIGURED MAPPING IS REFUSED AT CONFIGURATION TIME, NOT ON THE FIRST ROW.
//
// "Nothing configured" and "checked, and fine" must not look the same. An empty
// underlying map refuses every row of every chain, and a run that fetches a
// catalogue and reports four hundred refusals reads as a venue problem.
func TestAMappingThatCouldNotWorkIsRefusedBeforeAnyRequest(t *testing.T) {
	for name, m := range map[string]Mapping{
		"no contract namespace": {Underlying: map[string]string{"BTC-USD": "BTC-USDT"}},
		"no underlying map":     {ContractNamespace: "XOKX"},
		"a blank canonical id": {
			Underlying:        map[string]string{"BTC-USD": " "},
			ContractNamespace: "XOKX",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseOKXInstruments(chain(), m, Options{}); err == nil {
				t.Error("the parse accepted a mapping that cannot produce a correct load")
			}
			if _, err := NewOKXReference("https://example.invalid", m, Options{}); err == nil {
				t.Error("the constructor accepted it, so the refusal would arrive mid-run")
			}
		})
	}
}

// A DATED FUTURE BECOMES FutureTerms, INCLUDING ITS SETTLEMENT TYPE.
//
// contract_size is ctVal × ctMult in the units ctValCcy states, exactly as an
// option's multiplier is. settlement_type is a venue fact this loader asserts
// rather than reads — OKX lists no physically-delivered contract, and nothing in
// the response would say so if it did.
func TestADatedFutureMapsToFutureTerms(t *testing.T) {
	b := parse(t, okxCatalogue(okxFuture("BTC-USD-241227", "BTC-USD", expiry.UnixMilli(), listed.UnixMilli())))

	if len(b.Refused) != 0 {
		t.Fatalf("a well-formed future was refused: %v", b.Refused)
	}
	if len(b.Mapped) != 1 {
		t.Fatalf("mapped %d rows, want 1", len(b.Mapped))
	}
	rec := b.Mapped[0]
	if rec.Kind != terms.KindFuture {
		t.Errorf("kind = %q, want FUTURE", rec.Kind)
	}
	f := rec.Terms.GetFuture()
	if f == nil {
		t.Fatalf("the record carries no FutureTerms; oneof = %T", rec.Terms.GetTerms())
	}
	if f.GetUnderlyingId() != "BTC-USDT" {
		t.Errorf("underlying_id = %q, want the canonical BTC-USDT", f.GetUnderlyingId())
	}
	if got := dec.FromProto(f.GetContractSize()).RatString(); got != "100" {
		t.Errorf("contract_size = %s, want 100", got)
	}
	if got := dec.FromProto(f.GetTickSize()).FloatString(1); got != "0.1" {
		t.Errorf("tick_size = %s, want 0.1", got)
	}
	if f.GetSettlementType() != "CASH" {
		t.Errorf("settlement_type = %q, want CASH — the field is required and PHYSICAL would change "+
			"how a position is closed out", f.GetSettlementType())
	}
	if !f.GetExpiry().AsTime().Equal(expiry) {
		t.Errorf("expiry = %s, want %s", f.GetExpiry().AsTime(), expiry)
	}
}

// A VENUE REFUSAL ARRIVES INSIDE AN HTTP 200.
//
// OKX carries "underlying does not exist" and rate limits in the envelope's code
// field. A parse that checked only the JSON shape would read one as an empty
// catalogue — which is indistinguishable from "this underlying lists no options"
// and would report a successful load of nothing.
func TestAVenueRefusalInsideA200IsNotAnEmptyCatalogue(t *testing.T) {
	_, err := ParseOKXInstruments(
		[]byte(`{"code":"51001","msg":"Instrument ID does not exist","data":[]}`), mapping(), Options{})
	if err == nil {
		t.Fatal("a venue refusal carried in a 200 was read as an empty catalogue")
	}
	if !strings.Contains(err.Error(), "51001") {
		t.Errorf("the error does not name the venue's code: %v", err)
	}
}

// THE BASE URL HAS NO DEFAULT (#147). OKX serves demo and production from the
// same host, and a chain of invented strikes loaded from the demo catalogue is
// indistinguishable from a real one once it is in the table.
func TestTheBaseURLIsRequired(t *testing.T) {
	if _, err := NewOKXReference("  ", mapping(), Options{}); err == nil {
		t.Fatal("an empty base URL was accepted")
	}
}

// THE FETCH ASKS FOR THE UNDERLYING THE CALLER NAMED, and refuses the types that
// are not contract terms. OKX's SWAP is a PERPETUAL — reference.v1.SwapTerms is an
// interest-rate swap with pay/receive legs — so passing it through would file
// perpetual futures under a kind nothing looks for.
func TestChainRequestsTheNamedUnderlyingAndRefusesOtherInstrumentTypes(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(chain())
	}))
	t.Cleanup(srv.Close)

	ref, err := NewOKXReference(srv.URL, mapping(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := ref.Chain(context.Background(), OptionInstruments, "BTC-USD")
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if len(b.Mapped) != 3 {
		t.Fatalf("mapped %d rows over HTTP, want 3", len(b.Mapped))
	}
	if !strings.Contains(query, "instType=OPTION") || !strings.Contains(query, "uly=BTC-USD") {
		t.Errorf("the request does not name the instType and underlying: %s", query)
	}

	if _, err := ref.Chain(context.Background(), InstType("SWAP"), "BTC-USD"); err == nil {
		t.Error("instType=SWAP was accepted; an OKX perpetual is not an interest-rate swap")
	}
	if _, err := ref.Chain(context.Background(), OptionInstruments, " "); err == nil {
		t.Error("a catalogue-wide pull with no underlying was accepted")
	}
}

func keys(m map[string]terms.Record) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
