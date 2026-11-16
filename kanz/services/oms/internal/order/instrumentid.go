package order

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
