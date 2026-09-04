package order

import (
	"errors"
	"strings"
	"testing"
	"time"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
)

// THE HALF OF #885's "Verified when" THAT IS EASY TO SKIP.
//
// "An over-long instrument_id is refused" is one line to write and one line to
// prove. "Every existing legitimate instrument id in the estate still admits" is
// the half that decides whether the bound is safe to deploy, and it cannot be
// proved by reasoning about it — a POLICY change that refuses an order admitted
// today has to be measured against the ids that actually exist.
//
// So this is the enumeration, not a sample. Every entry was found by scanning
// the repository for values in an instrument-id position — Go literals, protos,
// migrations, k8s manifests, the load harnesses, the web client's fixtures and
// datamaster's vendor feeds — and grouped by the shape it belongs to. Each group
// carries where it came from, because a future change to the bound must be able
// to re-check the same population rather than trust this list.
//
// IF ONE OF THESE STOPS ADMITTING, THE BOUND IS WRONG — not the test.
var estateInstrumentIDs = map[string][]string{
	// The deployed set. infra/deploy/{market-ingest,venue-binance,venue-okx}-deploy.yaml
	// carry these as the left-hand side of BINANCE_SYMBOLS / OKX_SYMBOLS, so they
	// are the only instrument ids this estate trades in anger today.
	"deployed": {"BTC-USDT", "ETH-USDT"},

	// The canonical BASE-QUOTE pair. instrument.ParseID splits on exactly one "-".
	"pair": {
		"BTC-USD", "ETH-USD", "ETH-EUR", "SOL-USD", "SOL-USDT", "DOGE-USD",
		"BTC-PERP", "ETH-PERP", "SOL-PERP", "EURUSD",
	},

	// Perpetuals and swaps — more than one "-", so not Pairs, but ids all the same.
	"swap": {
		"BTC-USDT-SWAP", "ETH-USDT-SWAP", "SOL-USDT-SWAP", "BTC-USD-SWAP",
		"BTC-USD-PERP", "MYSTERY-SWAP",
	},

	// Rates/curve instruments — internal/risk/pricing/curve and livequote.
	"curve": {
		"USD-DEP-1Y", "USD-DEP-3M", "EUR-DEP-3M", "USD-FUT-1Y",
		"USD-SWAP-2Y", "USD-SWAP-5Y", "USD-SWAP-7Y", "USD-SWAP-10Y",
		"USD-SWAP-15Y", "USD-SWAP-20Y", "USD-SWAP-30Y", "EUR-SWAP-5Y",
		"IRS-5Y", "GOVT-10Y", "USD-POS", "EUR-POS",
	},

	// Dated and struck option contracts, and the namespaced form termsload
	// derives from them. The namespaced entry is the LONGEST id in the estate.
	"contract": {
		"BTC-USD-241227-60000-C", "BTC-USD-241227-60000-P", "BTC-USD-241227-70000-C",
		"BTC-USD-241227-90000-C", "ETH-USD-241227-3000-C", "BTC-60000-C",
		"BTC-DEC24-60K-CALL",
		"XOKX:BTC-USD-241227-60000-C", "XOKX:BTC-USD-241227-60000-P",
	},

	// Equities, including the two shapes internal/refdata's own test names
	// outright: "a RIC carries a dot, and a symbol can carry a slash".
	"equity": {
		"AAPL", "MSFT", "GOOG", "TSLA", "NVDA", "XOM", "TSM", "ASML", "SAP",
		"KO", "VTI", "BND", "BAT", "VOD.L", "AAPL.O", "AAPL.XNAS", "BRK/B",
	},

	// datamaster's DefaultIDResolver falls FIGI -> ISIN -> CUSIP -> Symbol, so
	// each of these IS a canonical id for some vendor row — and the last one is
	// why a space cannot be refused here.
	"vendor": {
		"BBG000B9XRY4", "BBG1",
		"US0378331005", "US0000001", "US9999999",
		"US0000000001", "US0000000002", "US0000000003",
		"037833100", "000000019", "222222229",
		"AAPL US Equity",
	},

	// Venue spellings. They are not canonical ids, but a deployment is free to
	// name its canonical id after one, and internal/execution's symbol-map tests
	// use them on both sides.
	"venue_symbol": {"BTCUSDT", "ETHUSDT", "DOGEUSDT", "ETHEUR", "XBTUSD", "XBTUSDT"},

	// The synthetic fixtures the suites are actually built on. They are estate
	// ids in the only sense that matters here: a change that refused one would
	// turn a green suite red for a reason that has nothing to do with the order.
	"fixture": {
		"INST1", "INST2", "INST-1", "INST-0", "SIM1", "ISS-1", "ACME",
		"BOND-1", "BOND_A", "MBS_A", "MBS_B", "OPT_A", "OPT_B",
		"US-A", "US-B", "EU-B", "EUR-A", "USD-A", "USD-B",
		"PRIVATE-CO", "SOMETHING-ELSE", "OLDSTOCK", "NO-MARK", "NO_DATA",
		"NOPRICE", "UNMARKED", "INJECTED", "MUTATED", "MYSTERY", "SHOCKED",
		"UNSHOCKED", "STALE", "EXACT", "ZERO", "LONG", "SHORT", "FAST", "SLOW",
		"LIQUID_FAST", "LIQUID_SLOW", "ILLIQUID", "CASH", "EQ", "SW", "GOVT",
		"OIL", "OILCO", "TECH", "TECHCO", "DARK", "DARKCO", "FOOD", "BANK",
		"AAA", "BBB", "CCC", "A", "B", "C", "D", "E", "X", "Y", "Z",
		"PRICE-SPINE-955-BTCUSDT", "SOME-VERY-LONG-INSTRUMENT-NAME",
		// Mixed case. Nothing normalizes case on this path.
		"WithUnc", "WithoutUnc",
	},
}

func TestEveryEstateInstrumentIDStillAdmits(t *testing.T) {
	for group, ids := range estateInstrumentIDs {
		for _, id := range ids {
			cmd := limitOrder(d(100, 0), d(1025, -2))
			cmd.InstrumentId = id
			if _, err := Accept(cmd, t0); err != nil {
				t.Errorf("%s: instrument_id %q (%d bytes) is REFUSED by the new bound: %v\n"+
					"This id exists in the estate. A bound that refuses a live instrument is worse "+
					"than no bound — widen MaxInstrumentIDLen or drop the rule that caught it, and "+
					"do not delete this entry.", group, id, len(id), err)
			}
		}
	}
}

// NON-VACUITY. The corpus above proves nothing if the bound is unreachable, and
// it would stay green against a validateInstrumentID that returned nil always.
// This is the other side: the shapes that must NOT admit.
func TestAnInstrumentIDOutsideTheBoundIsRefusedAtAdmission(t *testing.T) {
	for name, id := range map[string]string{
		"empty":                               "",
		"one byte over the ceiling":           strings.Repeat("A", MaxInstrumentIDLen+1),
		"a megabyte of it":                    strings.Repeat("A", 1<<20),
		"a newline":                           "BTC-\nUSDT",
		"a carriage return":                   "BTC-\rUSDT",
		"a NUL":                               "BTC-\x00USDT",
		"a DEL":                               "BTC-\x7fUSDT",
		"a tab":                               "BTC-\tUSDT",
		"a control byte inside invalid UTF-8": "\xff\x00",
	} {
		t.Run(name, func(t *testing.T) {
			cmd := limitOrder(d(100, 0), d(1025, -2))
			cmd.InstrumentId = id
			_, err := Accept(cmd, t0)
			var re *RejectError
			if !errors.As(err, &re) {
				t.Fatalf("Accept(%q) = %v; want a *RejectError — an unbounded instrument_id reaches "+
					"the order book, the position subject and every log line for the order", id, err)
			}
			if re.Code != "INVALID_ORDER" {
				t.Fatalf("code = %q, want INVALID_ORDER", re.Code)
			}
			if !strings.Contains(re.Msg, "instrument_id") {
				t.Fatalf("reason %q does not name instrument_id — an operator cannot act on a "+
					"refusal that does not say which field was out of bounds", re.Msg)
			}
		})
	}
}

// EXACTLY AT THE CEILING IS ADMITTED. An off-by-one here refuses the last
// legitimate id, and the corpus above would not catch it: nothing in the estate
// is 64 bytes long.
func TestAnInstrumentIDExactlyAtTheCeilingAdmits(t *testing.T) {
	cmd := limitOrder(d(100, 0), d(1025, -2))
	cmd.InstrumentId = strings.Repeat("A", MaxInstrumentIDLen)
	if _, err := Accept(cmd, t0); err != nil {
		t.Fatalf("an id of exactly %d bytes was refused: %v", MaxInstrumentIDLen, err)
	}
}

// THE BOUND MUST CLEAR THE ESTATE BY A MARGIN, NOT BY A BYTE.
//
// #885's floor is not the longest id that happens to exist today: termsload
// composes a derived contract id as ContractNamespace + ":" + venue instId, and
// the namespace is operator-supplied with no default and no bound of its own. A
// ceiling sitting just above the longest observed id would be refuted by the
// next operator who names a longer namespace, at admission, on the capital path.
// This states the margin so a future narrowing has to argue with it.
func TestTheBoundClearsTheLongestEstateIDWithHeadroom(t *testing.T) {
	longest, longestID := 0, ""
	for _, ids := range estateInstrumentIDs {
		for _, id := range ids {
			if len(id) > longest {
				longest, longestID = len(id), id
			}
		}
	}
	if longest != 30 || longestID != "SOME-VERY-LONG-INSTRUMENT-NAME" {
		t.Fatalf("the longest enumerated id is now %q (%d bytes); it was 30. Re-derive the bound "+
			"against the new population rather than adjusting this number", longestID, longest)
	}
	if MaxInstrumentIDLen < 2*longest {
		t.Fatalf("MaxInstrumentIDLen = %d is less than twice the longest id the estate holds (%d). "+
			"The namespace half of a derived contract id is operator-supplied and unbounded, so a "+
			"ceiling this close refuses the next namespace somebody names", MaxInstrumentIDLen, longest)
	}
}

// AND THE REFUSAL MUST REACH THE BOOK'S OWN STORE, not just the aggregate.
//
// Accept is pure, so a unit test on it proves the rule and nothing about where
// the rejection lands. The OMS handler returns nil for a REFUSAL too — it acks
// and publishes REJECTED — so checking the handler's error reads a refusal as a
// success. The load-bearing assertion is the LAST one: NO ROW. `orders`.
// instrument_id is TEXT with no length and no CHECK in all nine tables that hold
// one, so "refused" has to mean the string never got there.
//
// Gated on TEST_POSTGRES_URL, like every other Postgres test in this package.
// The in-memory store would pass this too, which is exactly why it is not the
// one being asserted against: MemoryStore ignores ctx and its own durability.
func TestPostgres_AnOverLongInstrumentIDNeverReachesTheBook(t *testing.T) {
	pool := newPool(t) // skips unless TEST_POSTGRES_URL is set
	ctx := testCtx()

	fb := &fakeBus{}
	store := NewPostgres(pool)
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter([]execution.Venue{execution.NewSimVenue("XSIM")}), nil, nil,
		WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	svc.now = func() time.Time { return t0 }

	cmd := limitOrder(d(100, 0), d(1025, -2))
	cmd.OrderId = "o885"
	cmd.InstrumentId = strings.Repeat("Z", MaxInstrumentIDLen+1)

	if err := svc.Handle(ctx, submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("submit returned %v — a malformed instrument_id is a PERMANENT condition, so the "+
			"delivery must be acked and refused, never nacked into a retry loop", err)
	}

	oc, _ := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc == nil || oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED {
		t.Fatalf("outcome = %v, want REJECTED", oc)
	}
	if !strings.Contains(oc.GetReason(), "instrument_id") {
		t.Fatalf("reason %q does not name the field that was out of bounds", oc.GetReason())
	}
	if fb.last(EventTypeRejected) == nil {
		t.Fatal("no OrderRejected FACT — the refusal exists only as a command outcome, and a " +
			"consumer folding FACTs would never learn this order died")
	}

	if _, _, err := store.Load(ctx, cmd.GetOrderId()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load(%q) = %v, want ErrNotFound — the order was WRITTEN. An unbounded, "+
			"caller-supplied string reached `orders`.instrument_id, which is TEXT with no length "+
			"and no CHECK, and every consumer of the book inherits it from there", cmd.GetOrderId(), err)
	}
}

// The rule's own unit surface, so a failure says which arm broke rather than
// only that an order was refused.
func TestValidateInstrumentIDNamesTheRuleItBroke(t *testing.T) {
	for _, tc := range []struct{ name, id, want string }{
		{"empty", "", "required"},
		{"too long", strings.Repeat("A", MaxInstrumentIDLen+1), "bytes"},
		{"control byte", "A\x01B", "control byte"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rej := validateInstrumentID(tc.id)
			if rej == nil {
				t.Fatalf("validateInstrumentID(%q) = nil", tc.id)
			}
			if !strings.Contains(rej.Msg, tc.want) {
				t.Fatalf("message %q does not contain %q", rej.Msg, tc.want)
			}
		})
	}
	if rej := validateInstrumentID("XOKX:BTC-USD-241227-60000-C"); rej != nil {
		t.Fatalf("the longest real id in the estate was refused: %v", rej)
	}
}
