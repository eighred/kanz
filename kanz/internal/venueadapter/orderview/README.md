# Adapter order and close ownership

Both deployed adapters use the same tenant-scoped PostgreSQL store for their
order view and pending cancellation intents. `CancelOrder` commits the intent
before calling the exchange. If that write fails, cancellation is not dispatched.
The in-memory implementation remains available for local, non-durable operation.

The watchdog treats venue outcomes as follows:

| Observation | Action |
| --- | --- |
| Confirmed terminal, matching order identity, valid exact quantity | Publish discrepancy evidence; resolve ownership after publication succeeds. |
| Confirmed working | Keep ownership and report an unconfirmed close. |
| Not found | Keep ownership: absence from this lookup does not establish cancellation or fill history. |
| Timeout, denied request, rate limit, malformed or unsupported response | Keep ownership and report an incomplete observation. |
| Instrument cannot be queried | Keep ownership and report the existing unhealable reason. |

No watchdog outcome authorizes a residual market order. Residual execution must
go through normal OMS controls using authoritative exposure. `StateHealed` remains
operator-facing evidence, not an economic restatement or a substitute for fills.

Claims are limited to 100 intents per batch. Retry delays are 1, 2, 4, 8, 16, 32,
then 60 seconds, without an expiry that discards unanswered ownership. PostgreSQL
persists claim scheduling atomically using `FOR UPDATE SKIP LOCKED`. A failed
publication or failed deletion leaves the intent recoverable; evidence delivery
is at least once. Redelivery preserves the original intent and retry clock.
Typed pass errors expose total, checked, and failed counts plus individual order
failures. Metrics retain bounded labels; logs report coverage without raw errors.

Apply each adapter's `0003_pending_closes.sql` before running the new binary.
The migration is additive and enforces FORCE RLS. It cannot reconstruct intents
already lost by the previous in-memory implementation: outstanding cancellations
must be checked against exchange/audit records during that upgrade. Database
placement verification remains the onboarding gap tracked by ONBOARD-M6; the new
tables are explicitly included in the onboarding script's unverified inventory.
