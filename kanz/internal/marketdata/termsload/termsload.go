// Package termsload turns a venue's public instrument catalogue into
// terms.Record values ready for terms.Postgres.Put — the PRODUCTION WRITER the
// contract-terms store has never had.
//
// # What was missing, and what it cost
//
// terms.Postgres.Put had only test callers, and nothing anywhere constructed a
// reference.v1.ContractTerms. The table was therefore empty in every environment,
// and two shipped features computed over it:
//
//   - The fixed-income measures (DV01, Duration, Convexity, SpreadDuration) are
//     wired at the risk-engine composition root and resolve their terms through
//     termsource.Provider. Over an empty table every one of them returns zero for
//     every portfolio — a number, not an error, and a book with real rate risk
//     reports none.
//   - termsource.Provider.OptionTerms can never return ok=true, which is one of
//     the named blockers on RegisterGreeks.
//
// The store was built, the readers were built, and the middle was a test double.
// This package is the middle.
//
// # A REFUSED ROW IS BETTER THAN A WRONG ONE, and that is the design
//
// termsource.toSpec is the consumer, and it refuses terms it cannot use: a
// non-positive strike, a non-positive multiplier, an absent expiry, an
// OPTION_TYPE_UNSPECIFIED. When it refuses, the Provider fires its missing-terms
// observer — so a row this loader writes badly does not fail, it becomes a
// COUNTED MISSING-TERMS EVENT FOREVER, indistinguishable from the terms never
// having been loaded at all. The operator then goes looking for a load that ran
// and succeeded.
//
// Worse, a row that is merely WRONG rather than degenerate — a strike off by a
// factor, an underlying pointing at the wrong spot instrument — passes toSpec and
// prices. Silently, and for every position in the chain.
//
// So this package refuses. Every rejection is a Refusal carrying the venue's own
// instId and a reason a human can act on, and nothing is ever defaulted.
//
// # Read-only, unauthenticated
//
// /api/v5/public/instruments is public. This holds no API key and cannot place an
// order, which is worth stating because the venue adapter next door holds one that
// can.
package termsload

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/marketdata/terms"
)

// maxListedRefusals bounds how many refusals Loadable names in its error.
//
// A whole option chain is hundreds of rows, and a mapping mistake usually refuses
// ALL of them — an error listing four hundred identical reasons is one nobody
// reads. The count is always exact; the listing is a sample.
const maxListedRefusals = 10

// Mapping resolves a venue's own names onto this platform's canonical ids.
//
// # It is supplied by the OPERATOR and has no fallback
//
// There is no automated venue-symbol resolver on this platform and this package
// does not invent one. Every existing consumer of the same problem takes the
// mapping from configuration — market-ingest reads MARKET_INGEST_OKX_SYMBOLS as
// "canonicalID=venueSymbol" pairs, kanz-backfill takes --instrument and --symbol
// as two separate REQUIRED flags, and execution.InstrumentSymbol carries both
// halves side by side. #407 is the record of what conflating them costs.
type Mapping struct {
	// Underlying maps OKX's `uly` label (e.g. "BTC-USD") to the canonical
	// instrument_id of the SPOT instrument whose price drives the derivative.
	//
	// THE ONE FIELD THAT MUST NEVER BE GUESSED. reference.v1.OptionTerms's
	// underlying_id becomes compute.OptionSpec.UnderlyingID, which is the key the
	// Greeks layer prices the option against. A guess has two outcomes and both
	// are bad:
	//
	//   - it resolves to nothing, and every option in the chain silently drops out
	//     of the Greeks — the same visible result as never loading the terms, so
	//     the load looks done and is not;
	//   - it resolves to a REAL BUT DIFFERENT instrument, and every Greek in the
	//     chain is computed against the wrong spot price. Nothing errors. The
	//     numbers are plausible.
	//
	// The second is not hypothetical here. OKX's `uly` for its BTC options is the
	// literal string "BTC-USD", and "BTC-USD" is also a canonical instrument_id on
	// this platform — which venuesrv's own test comment records is USDT-quoted on
	// both live venues. So the string that looks like a free identity mapping is
	// the exact case where taking it would be wrong, and would look right.
	//
	// KEYED BY THE VENUE'S LABEL, which is the OPPOSITE DIRECTION from every other
	// symbol map in this tree: execution.StaticSymbolMap and market-ingest's
	// MARKET_INGEST_OKX_SYMBOLS are both canonicalID=venueSymbol, because every
	// existing consumer already holds the canonical id and needs the venue's name
	// for it. A catalogue load is the first thing to arrive from the other side —
	// it holds the venue's name and needs the canonical id — and there is no
	// reverse resolver on this platform to borrow.
	//
	// The direction is stated because a map written the wrong way round compiles,
	// has the right type, and refuses every row with a reason that blames the
	// venue.
	Underlying map[string]string

	// Contract maps a venue instId to the canonical instrument_id of the
	// DERIVATIVE itself, for contracts this platform has already named. Optional:
	// an instId absent here gets the namespaced id described on
	// ContractNamespace.
	Contract map[string]string

	// ContractNamespace prefixes the derivative's own canonical id when Contract
	// holds no entry for it, producing "<namespace>:<instId>" — for example
	// "XOKX:BTC-USD-241227-60000-C". REQUIRED, no default.
	//
	// # Why this one is derived when Underlying is refused
	//
	// The two ids fail differently, and that asymmetry is the whole justification.
	//
	// A wrong UNDERLYING id is a PRICING INPUT: it silently reprices the chain.
	// A wrong CONTRACT id is a JOIN KEY: LatestAsOf finds no row for whatever id
	// the position actually carries, the missing-terms observer fires, and the
	// option is priced linearly — wrong, but wrong in the direction that is
	// counted and visible. Nothing is mispriced against a stranger's spot.
	//
	// Requiring an operator to enumerate every contract instead would mean typing
	// out a five-hundred-row option chain by hand before the first load, which is
	// how a loader ends up never being run.
	//
	// THE NAMESPACE IS REQUIRED PRECISELY TO KEEP THAT PROPERTY. A bare instId
	// could collide with a canonical spot id and serve one instrument another's
	// terms — a mispricing, not a missing join. Forcing the operator to name a
	// namespace makes the derived id live in a space they control and can check.
	//
	// WHAT THE DERIVED ID IS NOT: a Pair. instrument.ParseID splits a canonical id
	// on a single "-" and refuses anything with more ("BTC-USD-PERP" is pinned as
	// refused in its own test, "ambiguous rather than clever"), so no dated or
	// struck contract has a base/quote today — with or without this namespace. That
	// is a pre-existing gap in the id grammar, not one this introduces, and it
	// means the QuoteMismatch guard does not cover these instruments. Worth knowing
	// before anyone routes an order at one.
	ContractNamespace string
}

// validate refuses a Mapping that cannot produce a correct load.
//
// AT CONFIGURATION TIME, NOT ON THE FIRST ROW. An empty Underlying map refuses
// every row of every chain — a run that fetches the catalogue, maps nothing, and
// reports several hundred refusals reads as a venue problem. Refusing here says
// which of the two it is, before a request is made.
func (m Mapping) validate() error {
	if strings.TrimSpace(m.ContractNamespace) == "" {
		return errors.New("termsload: a contract namespace is required and has no default — it is " +
			"what keeps a derived contract id out of the canonical spot id space, where a collision " +
			"would serve one instrument another instrument's terms")
	}
	if len(m.Underlying) == 0 {
		return errors.New("termsload: no underlying mapping is configured, so every row would be " +
			"refused — \"nothing configured\" and \"checked, and fine\" must not look the same")
	}
	for venue, canonical := range m.Underlying {
		if strings.TrimSpace(venue) == "" || strings.TrimSpace(canonical) == "" {
			return fmt.Errorf("termsload: underlying mapping %q=%q has an empty half; a blank "+
				"canonical id would be stored as \"no underlying\", which is what a swap looks like",
				venue, canonical)
		}
	}
	return nil
}

// contractID returns the canonical id for one venue contract.
func (m Mapping) contractID(instID string) string {
	if id, ok := m.Contract[instID]; ok && strings.TrimSpace(id) != "" {
		return id
	}
	return m.ContractNamespace + ":" + instID
}

// Options tunes a load.
type Options struct {
	// AsOfOverride replaces the venue's listing time as every record's as_of.
	//
	// # THE REPAIR LEVER, and it should be used as one
	//
	// as_of defaults to the venue's own listTime — see the package's AS_OF
	// reasoning on ParseOKXInstruments. That makes a re-run a true no-op against
	// Put's ON CONFLICT DO NOTHING, which is the property that lets this be run on
	// a schedule. It also means a row written from a BAD PARSE cannot be corrected
	// by re-running: the second run presents the same primary key and the store
	// keeps the first, immutable, wrong row.
	//
	// Setting this to a later instant is how that is fixed. It writes the corrected
	// terms as a new as_of version beside the wrong one, which LatestAsOf then
	// prefers — the same mechanism a genuine amendment uses, which is the point:
	// the store has one way to supersede a record and this is it.
	//
	// It is deliberately not a "use now" boolean. An operator setting this is
	// stating an effective instant, and a correction whose as_of is a wall clock
	// read at some point during a multi-chain run would version different
	// underlyings at different times for no reason anyone could reconstruct.
	AsOfOverride time.Time
}

// Refusal is one catalogue row this loader declined to map, and why.
//
// It carries the VENUE's instId rather than a canonical id, because a refused row
// has no canonical id — that is frequently the refusal itself — and the instId is
// what the operator pastes back into the venue's own documentation.
type Refusal struct {
	InstID string
	Reason string
}

func (r Refusal) String() string {
	if r.InstID == "" {
		return "(row with no instId): " + r.Reason
	}
	return r.InstID + ": " + r.Reason
}

// Batch is the result of mapping one catalogue response.
//
// BOTH HALVES ARE RETURNED TOGETHER, deliberately. The parse never stops at the
// first bad row: a mapping mistake usually breaks a whole chain the same way, and
// an operator who fixes one refusal per run to discover the next one is an
// operator who stops running the loader.
type Batch struct {
	// Mapped are the rows that became records. Do NOT hand these to Put directly
	// — use Loadable, which enforces the all-or-nothing rule below.
	Mapped []terms.Record
	// Refused are the rows that did not, each with a reason.
	Refused []Refusal
}

// ErrPartialChain reports that some rows of a chain could not be mapped, so none
// of it may be stored.
var ErrPartialChain = errors.New("termsload: the chain is partial")

// Loadable returns the records to hand to terms.Postgres.Put, or refuses the
// WHOLE batch if any row was refused.
//
// # All-or-nothing, and it is the store's rule rather than this package's
//
// Put's own doc states it: "Written in one transaction so a chain load is
// all-or-nothing: a partially loaded chain would calibrate a surface from some of
// an underlying's strikes, which fits cleanly and is wrong." Handing Put a
// pre-filtered slice would honour the transaction and defeat the reason for it —
// the transaction protects against a write failing halfway, and this protects
// against the caller deciding halfway is acceptable.
//
// Concretely, a chain missing its wings is not a chain with a gap. volsurface
// fits the strikes it is given, so dropping the far strikes produces a smooth,
// confident surface with no skew — and the skew is the part that prices the tail
// risk. A missing surface fails loudly; a flattened one does not fail at all.
//
// The refusals are still reported so every one can be fixed in a single pass.
func (b Batch) Loadable() ([]terms.Record, error) {
	if len(b.Refused) == 0 {
		return b.Mapped, nil
	}
	listed := b.Refused
	suffix := ""
	if len(listed) > maxListedRefusals {
		listed = listed[:maxListedRefusals]
		suffix = fmt.Sprintf(" (and %d more)", len(b.Refused)-maxListedRefusals)
	}
	reasons := make([]string, 0, len(listed))
	for _, r := range listed {
		reasons = append(reasons, r.String())
	}
	return nil, fmt.Errorf("%w: %d of %d rows could not be mapped, so none are stored: %s%s",
		ErrPartialChain, len(b.Refused), len(b.Refused)+len(b.Mapped),
		strings.Join(reasons, "; "), suffix)
}
