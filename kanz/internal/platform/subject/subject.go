// Package subject builds NATS subject tokens out of ids the platform did not choose.
//
// It exists because two different streams now key their subjects on entity ids —
// compliance.mandate.changed.<tenant>.<portfolio> (EXEC-M13) and
// risk.position.changed.<tenant>.<portfolio>.<instrument> (EXEC-M20) — and both are
// COMPACTED CURRENT-STATE streams, where the subject IS the identity of the thing whose
// latest value is kept. Get the token wrong and a consumer arms itself with the wrong
// entity's state, or with everybody's, and nothing errors.
//
// Promoted from internal/compliance on the second-consumer trigger. One implementation:
// two would be two chances to disagree about what a portfolio's subject is, and the
// disagreement would be invisible until a control read the wrong book.
package subject

import "strings"

// A POSITION IS STATE, AND ITS SUBJECT HAS TO SAY WHICH POSITION IT IS (EXEC-M20).
//
// Every position FACT rode the flat `risk.position.changed`, and JetStream compacts PER
// SUBJECT — so a stream set to "keep the latest per subject" retained exactly ONE message
// for the whole platform, and DeliverLastPerSubject (how a booting consumer learns current
// state in one read) returned one instrument's position and silence about every other.
//
// What that cost: the compliance monitor's book is in-memory and filled by a durable
// consumer, which resumes at its last ack. A restarted monitor came back with a PARTIAL
// book and evaluated every mandate against it — a fund holding an instrument its mandate
// FORBIDS looked compliant, because the holding was simply not there. That is the mandate
// defect (EXEC-M13) on a different stream.
//
// It lives HERE, and not in internal/risk/ingest, because the OMS publishes it and the
// compliance monitor consumes it — and the RISK-02 boundary (test/arch) forbids outsiders
// importing risk internals. One implementation, importable by everyone: two would be two
// chances to disagree about which subject a holding rides, and the disagreement would be
// invisible until a control read the wrong book.
const (
	// PositionChanged is the taxonomy name — and the EVENT_TYPE, which does not change.
	PositionChanged = "risk.position.changed"
	// PositionAll is what a consumer BINDS: the latest state of every holding.
	PositionAll = PositionChanged + ".>"
)

// PositionFor is the subject ONE holding's state rides on. Only the SUBJECT carries the
// entity; the event_type stays PositionChanged, exactly as the mandate's does.
func PositionFor(tenantID, portfolioID, instrumentID string) string {
	return PositionChanged + "." + Token(tenantID) + "." + Token(portfolioID) + "." + Token(instrumentID)
}

// THE EXECUTION PLANE NEEDS TO KNOW WHICH VENUE HOLDS IT (EXEC-M19a).
//
// A fund's EXPOSURE does not care which exchange the BTC sits at: one instrument, one
// number, and that is exactly what the risk engine and the compliance monitor consume. But
// EXECUTION cannot work without it — a CLOSE signal must flatten what is held AT EACH
// VENUE, and you cannot sell 1 BTC on Binance if it is sitting at OKX. The fill has always
// carried the venue; the projector threw it away, so nothing could answer "how much BTC do
// we hold at OKX" — and webhook-ingest's CLOSE path was wired to an EMPTY position map, so
// a `close` alert produced ZERO orders and the webhook answered 202.
//
// So the OMS's book is keyed per (portfolio, venue, instrument) — the finest true grain —
// and PROJECTED TWICE: the fund-level aggregate on PositionChanged (its consumers untouched,
// and unable to lose a venue), and the per-venue holding here. One source of truth at two
// granularities, each serving a consumer with a genuinely different need.
//
// The two subjects must never collide, and the aggregate's wildcard must never SWALLOW the
// per-venue FACTs — a monitor that folded each venue as though it were the fund's total
// would understate every limit it checks. `risk.position.changed.>` and
// `risk.position.venue.changed.>` diverge at the third token, so neither matches the other.
// Both still ride the compacted POSITION stream, which carries `risk.position.>`.
const (
	// VenuePositionChanged is the event_type of the per-venue holding FACT.
	VenuePositionChanged = "risk.position.venue.changed"
	// VenuePositionAll is what the execution plane BINDS: every venue of every holding.
	VenuePositionAll = VenuePositionChanged + ".>"
)

// VenuePositionFor is the subject ONE holding AT ONE VENUE rides on.
func VenuePositionFor(tenantID, portfolioID, venue, instrumentID string) string {
	return VenuePositionChanged + "." +
		Token(tenantID) + "." + Token(portfolioID) + "." + Token(venue) + "." + Token(instrumentID)
}

// Token makes an id safe as a single NATS subject token.
//
// A subject is DOT-DELIMITED, and `*` (one token) and `>` (all remaining tokens) are
// wildcards. An id carrying any of them does not fail — it silently changes what a
// publish lands on and what a subscription matches. An empty id would collapse the token
// entirely, so two different entities would share one subject and one would overwrite the
// other on a compacted stream.
func Token(s string) string {
	if s == "" {
		return "_"
	}
	return strings.NewReplacer(".", "_", "*", "_", ">", "_", " ", "_").Replace(s)
}
