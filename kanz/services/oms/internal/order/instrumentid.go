package order

// WHAT AN instrument_id MAY LOOK LIKE AT ADMISSION, AND WHY THE ANSWER IS
// "ALMOST ANYTHING, BUT NOT ARBITRARILY MUCH OF IT" (#885).
//
// The only rule this door had was "not empty". From here the value travels into
// the pre-trade gate's warning ledger, into metric and log fields, into the
// order state, into `orders`.instrument_id (TEXT, no length, no CHECK, in all
// nine tables that hold one) and into the compacted NATS subject
// risk.position.changed.<tenant>.<portfolio>.<instrument>. Its only ceiling
// today is the broker's max payload — nats-server's 1 MB default, and neither
// infra/nats/nats.yaml nor test/backing/nats-dev.conf sets max_payload. #814
// digested the ledger's keys for exactly this reason, which bounded ONE
// consumer; every new one still inherits the exposure.
//
// # The charset is NOT bounded, and that is the measured answer rather than a
// # gap
//
// The obvious move is to copy internal/orderid — 1 to 32 letters and digits,
// probed against OKX. It would be wrong here, and expensively so. An order id is
// stamped DIRECTLY as the venue's clOrdId, so it is subject to the intersection
// of every venue's rules. An instrument_id is not sent to any venue at all: it
// is a KEY into the adapter's instrument→symbol table (internal/execution/
// symbolmap.go), and what reaches the exchange is the operator-configured
// venue symbol on the other side of that table. No venue ever sees this string.
//
// So the constraint is what the ESTATE's own id space produces, and it produces
// every one of these:
//
//	VOD.L        a RIC carries a dot, and BRK/B a slash — stated outright, with
//	             a named test, by the ONE classifier source: internal/refdata/
//	             http_test.go's TestAnInstrumentIDIsEscapedIntoThePath
//	AAPL.XNAS    the `symbols` map in infra/deploy/rig-dev-secrets.yaml — a
//	             deployed manifest, not a fixture
//	MBS_A, OPT_A underscores, throughout internal/risk/compute
//	XOKX:BTC-USD-241227-60000-C
//	             a COLON BY CONSTRUCTION — internal/marketdata/termsload builds a
//	             derived contract id as ContractNamespace + ":" + venue instId
//	AAPL US Equity
//	             a SPACE, and reachable rather than hypothetical: datamaster's
//	             DefaultIDResolver falls FIGI → ISIN → CUSIP → Symbol, so a
//	             vendor row with none of the three yields the Bloomberg ticker
//	             verbatim as the canonical id (services/datamaster/internal/feed/
//	             reference.go)
//	WithUnc      mixed case
//
// An allow-list of any shape — alphanumeric, alphanumeric plus hyphen, upper
// case only — refuses one of those, and a bound that refuses a live instrument
// is worse than no bound. So the charset is deliberately open.
//
// THAT LEAVES ONE REAL DEFECT UNCLOSED, ON PURPOSE (#999). subject.Token maps
// ".", "*", ">" and " " to "_" to make an id safe as a subject token, which is
// lossy: VOD.L and VOD_L become one compacted position subject, and one
// instrument's current state overwrites the other's. Refusing dots here would
// hide it behind an admission rule that also refuses a RIC. The repair belongs
// at the subject, where the encoding can be made injective instead.
//
// The one exclusion is ASCII CONTROL CHARACTERS, and it is the weaker half of
// this rule: it rests on the absence of a counter-example rather than on a
// measurement. No instrument id anywhere in the estate contains one, and no
// identifier scheme master.v1.IdentifierScheme names (ISIN, CUSIP, SEDOL, FIGI,
// RIC) can produce one. What they would reach: subject.Token sanitizes ".", "*",
// ">" and " " out of a subject token but NOT "\r" or "\n", and the subject is
// written into a line-oriented wire protocol; the same string is a raw structured
// log field at service.go's admission log.
//
// # The length bound, and the evidence for each end
//
// FLOOR — what must still admit. The longest id naming a real instrument is
// XOKX:BTC-USD-241227-60000-C, 27 bytes (internal/marketdata/termsload/
// okx_test.go); the longest in the estate at all is 30, and it is a deliberately
// oversized column-truncation fixture (cmd/kanz-monitor/view_test.go). But the
// floor is NOT either of those numbers, because the 27-byte one is composed
// rather than written down: termsload.contractID returns ContractNamespace + ":"
// + instID, the namespace is operator-supplied, required, and has no default and
// no bound of its own, and the longest venue contract id observed is
// BTC-USD-241227-60000-C at 22. A namespace of ten characters therefore already
// produces 33. THIS IS WHY THE ANSWER IS NOT 32: borrowing orderid.MaxLen would
// refuse a dated, struck option contract under any namespace longer than nine
// characters, and it would do it at admission, on the capital path.
//
// CEILING — 64 bytes. It is a POLICY choice with stated headroom, not a constant
// derived from anything: no venue and no standard supplies one, because no venue
// sees this field. 64 is more than twice the longest id in the estate and leaves
// 41 bytes of namespace over the longest venue contract id, while every
// identifier scheme master.v1 names fits in 12. Against ~1 MB it is a bound worth
// having, and a per-entry cost that can be multiplied out and stated. The margin
// is asserted, not just described: see
// TestTheBoundClearsTheLongestEstateIDWithHeadroom.
//
// It counts BYTES, not runes, because bytes are what is being bounded — memory,
// wire and column. A non-ASCII id is admitted and simply gets fewer characters.
const MaxInstrumentIDLen = 64

// validateInstrumentID is the whole instrument-id rule at admission, in one
// place so a future consumer cannot inherit a laxer one.
//
// THE MESSAGE NAMES THE RULE AND THE MEASUREMENT. "invalid instrument" sends an
// operator looking for a missing security-master record; this says the id itself
// is out of bounds and what the bound is.
func validateInstrumentID(id string) *RejectError {
	if id == "" {
		// The wording predates the bound and is kept verbatim: it is the refusal
		// operators and tests already know.
		return reject("INVALID_ORDER", "instrument_id required")
	}
	if len(id) > MaxInstrumentIDLen {
		return reject("INVALID_ORDER",
			"instrument_id is %d bytes; at most %d are accepted. The longest id this estate holds "+
				"is 27 bytes (a namespaced option contract), and the field reaches the order book, "+
				"the position subject and every log line for the order, where it had no bound at all",
			len(id), MaxInstrumentIDLen)
	}
	// THE SUBJECT IS NO LONGER THE REASON THIS REFUSES (#999). The message below
	// used to say a control byte "reaches a NATS subject token and a log field
	// unsanitized". The subject half stopped being true when subject.Token became
	// an injective percent-escape: a control byte is now escaped there, not
	// carried. This bound stays because the id also reaches log fields and the
	// order book, and because a position can arrive from venue reconciliation
	// without passing this door at all — defence in depth, on a premise that is
	// stated accurately rather than inherited.
	for i := 0; i < len(id); i++ {
		if b := id[i]; b < 0x20 || b == 0x7F {
			// Byte-wise, not rune-wise: ranging over invalid UTF-8 yields
			// RuneError and would step straight over an embedded control byte.
			return reject("INVALID_ORDER",
				"instrument_id contains the control byte %#02x at position %d. Dots, slashes, "+
					"colons, underscores, spaces and mixed case are all legitimate here — a RIC, a "+
					"Bloomberg ticker and a namespaced contract id each carry one — but a control "+
					"byte reaches a log field unsanitized", b, i)
		}
	}
	return nil
}
