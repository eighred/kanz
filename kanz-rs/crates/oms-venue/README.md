# Typed OMS venue client

This source-first owner implements the existing `venue.v1.VenueAdapterService`
Execute, CancelOrder, Describe, ListInstruments and QueryOrder contract. It does
not run an OMS, subscribe to commands, admit orders, hold exchange credentials or
replace the authoritative Go caller. Migration acceptance belongs to #1413.

`Client::connect` requires an HTTPS endpoint, a Unix SPIFFE Workload API socket,
an exact expected peer SPIFFE ID and a tenant. Its sole connector performs
rotating SPIFFE mutual TLS, limits trust to the expected peer's trust domain,
and requires negotiated HTTP/2. The private HTTP URI passed to tonic prevents a
second TLS wrapper; the connector never returns plaintext IO. There is no public
Channel or connector injection. The legacy Go plaintext mode is outside this
Rust qualification domain. The bounded initial identity and connection
attempts take at most ten seconds each (subject to asynchronous runtime progress).
`connect_until` accepts explicit startup cancellation. `close` consumes the client,
closes its channel, aborts and joins its owned tonic workers and shuts down the
SPIFFE supervisor. Dropping a client or cancelled future aborts owned work but is
not equivalent evidence of a completed explicit join. SPIFFE's dependency-owned
transport cleanup is additionally observed by the actual fixture.

Every RPC has an explicit nonzero deadline, capped at 120 seconds. The owner
checks its monotonic absolute expiry after the awaited RPC as well as through
the timer: a ready inner future cannot return success after the budget expires.
A remote Cancelled response received before that expiry stays Cancelled; no peer
error text is interpreted as proof of a local timeout. Messages are
bounded to 1 MiB and known message nesting to 32 levels. These are named bounded
refusals, not a claim of parity for arbitrarily large Go messages. Generated types
come from the canonical venue schema and reuse the existing canonical OrderState,
Fill, Decimal and Money types. Immutable `Canonical<T>` values retain normalized
Go-compatible wire, including raw unknown values. The shared `kanz-proto-wire`
normalizer is also used by the OMS outbox relay; its original golden tests and
unknown-field mutation guards remain mandatory. Qualified graphs have no maps or
oneofs; new such schema fields require an explicit normalization compatibility
extension rather than silently discarding unknowns.

Execute errors never fabricate fills. An empty successful fill list is valid.
CancelOrder only confirms withdrawal on OK: a timeout remains ambiguous. Query
transport failures remain errors; Unimplemented and unknown/unspecified state
enums yield Indeterminate, while Unknown is reserved for an affirmative venue
answer. Returned fills preserve canonical wire. The response retains absent
executed quantity as absent.

Registration verifies the configured MIC/account against Describe and applies
the existing Go required-account and required-order-type switches. A fatal
registration refusal closes and joins this owner before returning. Empty capability
lists remain undeclared, not evidence of support; the raw canonical declaration
is retained for the future local admission owner. A failed catalog request is
stored separately from a successful empty catalog, matching existing startup
semantics. This library does not add an admission boolean or move the current
local risk/compliance/mandate gate across a process boundary.

The `oms-venue-fixture` example is a loopback-only interactive qualification
executable, stamped with `KANZ_BUILD_VERSION`. The independent Go package
`services/oms/tools/venue-conformance` supplies an ephemeral workload identity
server and synthetic typed venue. CI must run the actual stamped process and
require the exact RPC, wire, identity, rotation and lifecycle outcomes. Synthetic
mTLS proof does not establish production SPIRE registration, policy coverage,
exchange behavior, credentials or permission to activate a Rust writer.
