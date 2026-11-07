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
