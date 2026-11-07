# Pure OMS order owner

This crate implements the existing Go `services/oms/internal/order` aggregate:
Accept, Route, Reject, ApplyFill, Cancel, Expire, Amend, IsTerminal and Reconcile.
It owns canonical production protobuf generation and the existing exact rational
Decimal conversions reused by the order and position owners. It has no network,
SQL, credentials, admission callback or production writer. Go remains authoritative.

The diagnostic fill/terminal process delegates to this owner and preserves its
previous capture validation and unsupported-input rules. Store and venue owners
use the same generated types directly. The immutable generated-value/raw-wire
carrier lives in `proto-wire`; the venue facade only maps errors to gRPC status.
No generated models are committed.

Transitions retain untouched known fields and raw unknown metadata, including
future field 33. Replacement fields follow Go assignment semantics: new computed
Decimals replace prior nested unknowns; supplied command Decimals retain theirs.
Accept carries exactly the fields carried by Go, rather than all command metadata.
Cancel/Expire leave the optional remaining quantity unchanged, accessible as the
result's `leaves_quantity`; `message_wire` distinguishes absent from present-empty
without losing its unknown fields. Reconcile returns the existing action and a
typed quarantine reason; operator prose remains at the orchestration boundary.

Input/output canonical messages are bounded to 1 MiB, known-message nesting to
32, and arithmetic input/output exponents to ±64. Sign-only checks and unchanged
Decimal fields are not converted. Arithmetic uses no float and keeps Go's initial
scale-eight half-away rounding followed by repeated half-away division to fit
int64. Invalid timestamps and unsupported wire forms refuse without a candidate.
The inherited normalizer refuses maps and oneofs; current aggregate contracts
contain neither. Go's existing future enum and caller-owned price-policy behavior
is retained; this library is not a substitute for mandatory control admission.

`order-aggregate-fixture` is a bounded, one-shot JSON/base64 qualification adapter,
not a service transport. Build with an immutable `KANZ_BUILD_VERSION`, then set
`KANZ_ORDER_AGGREGATE_BIN` and `KANZ_ORDER_AGGREGATE_VERSION` when running
`go test -race -p 1 ./services/oms/tools/aggregate-conformance -count=1 -json`.
The existing OMS workflow requires the independent Go/rational matrix, complete
raw-wire equality, malformed/resource refusals and all earlier OMS/venue proofs.
Missing or skipped required outcomes cannot qualify the component.

The preserved funded lifecycle in #1316/#1317 is not imported or activated here.
Its additional cancellation contract, sixth venue observation RPC, approved fee
authority, capital transaction and old-writer activation fence require their own
compatibility and operational acceptance before a complete Rust OMS handoff.

The local attribution prerequisite measures existing terminal decision state;
it does not adopt a venue result, schedule a child, bind a tape, or enqueue facts.
Rational TCA intermediates exclude fees and preserve arrival-notional basis
points. All three decomposition legs are absent when quotes are incomplete.
Canonical projection deliberately follows Go's eight-place quantization and
rescaling, despite older prose describing exact representability. Copied ordered
quantity and arrival timestamp preserve nested unknown wire metadata; computed
figures receive fresh Decimal messages. Participation uses the same child
collection, canonical derived IDs and intervals, retaining missing tape and
partial coverage rather than replacing unknown observations with zero.

The attribution-only Go-wire candidate conversion shares the rounding core but
retains Go's i32 output-exponent limit under bounded rational inputs. Every
existing `decimal` caller still has the original ±64 output domain. A candidate
outside the whole-message financial Decimal domain is distinctly classified
`CapturedOutsideCanonicalDomain`, never an approved financial output. The Go
owner and Producer can capture/frame an exponent-114 candidate from valid ±64
inputs; that observation establishes neither broker nor downstream financial
acceptance. The unresolved output contract is tracked in #1413 and blocks a
production handoff; this pure diagnostic never publishes the candidate.

This prerequisite is not unrestricted Go parity. Existing canonical messages
and complete child results retain their 1MiB and 100000-row bounds. Deriving
schedule identities currently refuses more than 100000 declared indices before
allocation. Go has no equivalent fixed count ceiling, and OrderState has no
stored inverse of the SHA-derived child ID. The unresolved workload domain is
tracked in #1413 before general adoption or service handoff. Tape rationals are
limited to 4096-bit numerator/denominator representations; this exceeds the
actual canonical candle-sum domain but is narrower than an arbitrary Go big.Rat
implementation of RealisedVolume. Refusal produces no partial attribution.
