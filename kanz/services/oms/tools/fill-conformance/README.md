# Aggregate-fold conformance

This diagnostic corpus checks the existing Go `order.ApplyFill` against exact
rational expectations, then sends fresh canonical protobuf requests to the
stamped Rust worker. It grants no persistence, reconciliation or execution
authority. The owner still performs authorization, execution deduplication,
transactional version checks and recovery terminal-status restoration.

The numerical oracle specifies filled quantity, remaining quantity and VWAP
independently. Decimal text rounding at eight places followed by repeated
decimal digit rounding checks the existing half-away-from-zero representation,
including large-coefficient rescaling. Comparison is exact protobuf equality,
with **zero numerical tolerance**, including all untouched state fields and
optional-value presence. Time is an explicit fixture input, never wall time.
Sequential fixtures deliberately use the previously rounded stored average.

Missing price/previous average currently mean zero inside this aggregate; the
corpus records that behavior rather than silently introducing a different
admission policy. The pure fold does not deduplicate executions. Unknown wire
fields are outside the Rust shadow domain and must be refused, not discarded.
Recovery cases here cover only the normalized aggregate input, not durable
recovery authorization or the terminal restoration performed by its caller.

From `kanz/`:

```sh
go test -p 1 ./services/oms/tools/fill-conformance -count=1 -v
go run ./services/oms/tools/fill-conformance
KANZ_OMS_FOLD_BIN=/absolute/path/kanz-oms-fold-shadow \
KANZ_OMS_FOLD_VERSION=exact-build-sha \
go test -race -p 1 ./services/oms/tools/fill-conformance -count=1 -json
```

`TestRustFillFoldProcessConformance` explicitly skips without the binary; that
local skip is not Rust execution evidence. Linux CI must require the test to
finish without skips. Processes have bounded input/output, ten-second deadline,
scrubbed environment and temporary working directory. This supervises a trusted
installed test binary; it is not an operating-system sandbox.

Cancel/Expire additionally cover 108 independent status and optional-quantity
cases, actual fill-to-terminal replay, and closed protocol refusals through
the same binary's fixed --terminal mode. The evidence checker requires these
actual-process tests without skips. The PostgreSQL transaction suite continues
to qualify Rust fill candidates with existing Go cancellation; it does not
prove terminal Rust candidates were committed or transfer write authority.
