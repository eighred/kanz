# Run an isolated synthetic order

An operator submits one USD spot limit order to XSIM through the normal signed Go gateway. The fixture starts a private JetStream and PostgreSQL database, exercises the Rust command owner, and reads the committed order back through the real OMS query and gateway. The local network namespace has no host ports or external route. The broker, database, identities and mock venue receipts expire with the run.

## Build and run

Use Python 3.11+, Docker with Linux amd64 support, the Go toolchain in `kanz/go.mod`, and the schema generators listed in the [development setup](README.md). Build the native binary on Linux amd64 with the repository's Rust 1.90 toolchain. Use the repository root for these commands.

```sh
(cd schemas && buf generate && bash bootstrap-go-sdk.sh)
(cd rust && cargo +1.90.0 build --locked -p kanz-synthetic-oms)
python3 -B kanz/dev/synthetic_orders.py --native rust/target/debug/kanz-synthetic-oms
```

On Windows, generate the schemas as described in the development setup, build the same Linux binary in a Linux checkout or builder, and pass its path to `python -B kanz/dev/synthetic_orders.py --native <linux-binary>`. The runner prints the executable SHA-256 and PostgreSQL role attributes. A separate command runs the component and gateway checks on hosts without a Linux Rust build.

```sh
python3 -B kanz/dev/synthetic_orders.py --owners-only
```

The fixture applies the canonical OMS migrations, the isolated command schema and the XSIM receipt schema to a fresh database named `kanz_synthetic_oms`. Initialization uses a container-local bootstrap role. The application processes use separate, unprivileged SCRAM identities for control evidence, order commands, adapter observations, remote receipts, outbox delivery and presentation. Each Rust invocation receives only its assigned connection. The client receives neither a database identity nor portfolio authority from the request body.

## Operator checks

| Case | Observable result |
| --- | --- |
| Submit a fresh signed order twice | The gateway returns the same order ID and publishes one command. The Rust owner returns one acceptance and the persisted receipt on repeat. An altered price with the same key yields a conflict. |
| Start with an unknown platform mode | A permitted trader receives HTTP 423 until the gateway consumes the operator's lifecycle event. Missing authentication gives 401 and missing trade permission gives 403. |
| Evaluate market and limits | Missing, stale, future or invalid market evidence refuses. The engine projects exact post-trade cash and gross leverage against the effective tenant mandate. An expired or absent evidence set cannot admit an order. |
| Lose the venue response after sending | The stored order retains its last confirmed state and exposes quarantine. The gateway read shows the uncertainty. Redelivery queries the existing remote order before another economic effect. |
| Cancel during partial execution | The mock venue fills 1.25 units of the 2.5-unit order before confirming cancellation. The Rust owner records the exact executed quantity. Replaying the cancellation preserves the state and version. |
| Submit with capital admission enabled | A second tenant has 1000 USD and no reservation. The same signed order passes the synthetic controls and the canonical PostgreSQL activation trigger refuses it. The native receipt carries `CONTROL_DENIED` with reason `FUNDING_RESERVATION_REQUIRED`. The runner verifies rollback across cash, commitments, membership, order, command claim and outbox. It also checks `STORE_UNAVAILABLE` on the earlier anonymous database refusal and on an unrelated order constraint. |
| Refuse the database commit | A deferred trigger raises after order, command claim and outbox writes. The service reports an unconfirmed commit. An independent scoped read finds zero rows in all three tables before the retry. |
| Read the blotter | The authorized portfolio reader obtains the exact canonical state. A foreign tenant and an out-of-scope portfolio receive indistinguishable refusals. |

The HTTP interfaces used by the exercise are `POST /v1/orders`, `POST /v1/orders/{order_id}/cancel`, and `GET /v1/portfolios/{id}/orders`. The browser uses the corresponding `/api/v1/` routes through the web BFF. Submission uses an explicitly named order, `Idempotency-Key`, and canonical Protobuf JSON, including string Decimal coefficients. HTTP 202 records publication of the command. The state returned from the portfolio read records the durable outcome.

## Funded admission

The fixture arms capital admission for a fresh tenant and submits a signed XSIM limit order. The Go capital owner calculates the physical debit and fees. A separate insert-only role records the resulting amounts against the tenant, principal, portfolio, order, command digest and expiry. The native writer validates the five control decisions and calls the same PostgreSQL reservation function as Go, within the transaction that records the order and its acceptance. An independent tenant-scoped reader checks the resulting ledger. [Go capital contract](https://github.com/eighred/kanz/pull/1317) · [Reservation migration](../services/oms/migrations/0027_capital_reserve.sql) · [Synthetic authorization](../../rust/services/synthetic_oms/funding.sql)

| Case | Expected durable result |
| --- | --- |
| Buy 1.25 SYN at 200 USD with fees of 2 USD and 1 BNB | From opening cash of 1000 USD and 8 BNB, reserve 252 USD and 1 BNB. Persist two commitments, two memberships, one order, one command claim and one accepted fact. |
| Restart and replay the accepted command | Return the same order state and version. All row counts and reserved balances stay at their first-acceptance values. |
| Submit with 99 USD or with zero BNB | Return `FUNDING_INSUFFICIENT`. Both reserved balances and all order, funding and outbox counts remain zero. |
| Compete for 150 USD | Hold the USD row until two distinct native writers are waiting. Exactly one of two orders requiring 100 USD and 1 BNB commits. |
| Fail during the accepted outbox write | The database witnesses both memberships, both commitments, order and claim before the failure. A separate reader sees zero reservations and zero orders, claims or facts. Retrying commits once. |
| Reuse an order ID across tenants | Each tenant can reserve its own cash. RLS hides the first tenant's reservation from the second. An expired or wrong-principal approval is refused. |
| Dispatch a funded acceptance without a separate settlement authorization | Return `FUNDED_LIFECYCLE_HELD`. Both reservations remain held and no adapter observation is recorded. |

### Cancel before dispatch

Use the signed gateway cancellation for an accepted funded order. The synthetic Rust OMS checks the cancellation entitlement against the command delivered by JetStream and verifies that the order never reached the venue. It locks the cash balances, releases the order's unexecuted debit in each currency and records the terminal state, cancellation receipt and `order.order.cancelled` fact in one PostgreSQL transaction. [Synthetic cancellation contract](../../rust/services/synthetic_oms/cancellation.sql)

| Case | Expected durable result |
| --- | --- |
| Cancel one of two funded orders | Release 252 USD and 1 BNB. The other order keeps its own 252 USD and 1 BNB reserved. The cancelled order keeps its original admission amounts for audit. |
| Redeliver after the transaction commits | Once the original cancellation authority expires, JetStream redelivers the same command to a fresh Rust process. It returns the stored terminal result. The outbox relay publishes the accepted and cancelled facts in order, and the reader acknowledges both. |
| Send two cancellations while the USD balance is locked | The same user sends identical command bytes with two gateway event IDs. Observe both native writers waiting in PostgreSQL. Releasing the lock permits one terminal transition. The other writer returns the existing terminal state. |
| Fail while inserting the terminal fact | The reader still sees the pending order and both full reservations. Retrying the signed command commits the cancellation and releases both currencies. |

### Dispatch to XSIM and cancel after a partial fill

The funded fixture runs an isolated mTLS XSIM adapter, the Rust OMS writer, the Go accounting folder against its own PostgreSQL database, and file-backed JetStream. Before dispatch the writer checks all five original control decisions, both reserved currencies and the separate control authorization for this operation. The adapter's lost placement acknowledgement leaves the outcome unknown. A typed query recovers the same XSIM order and its execution while the unfilled quantity is still working. The gateway signs the cancellation only after the OMS has recorded the partial execution. [Settlement authorization](../../rust/services/synthetic_oms/settlement.sql) · [Cash settlement function](../services/oms/migrations/0029_capital_settle_direct.sql)

| Observation for the 1.25 SYN buy | USD reserved | BNB reserved | Accounting outcome |
| --- | ---: | ---: | --- |
| Admission and uncertain placement | 252 | 1 | Opening cash is 1000 USD and 8 BNB. |
| XSIM reports 0.625 SYN at 200 USD and a 1 USD fee | 252 | 1 | OMS records 126 USD executed. The remaining 0.625 SYN is working. |
| Signed cancellation, confirmation still outstanding at OMS | 252 | 1 | The executed amount and original reservations stay in place. |
| OMS records XSIM confirmation that the remainder was cancelled | 126 | 0 | The unused 126 USD and 1 BNB are released. The executed 126 USD awaits the journal. |
| Accounting books the fill and OMS consumes its cash fact | 0 | 0 | One trade reduces USD cash to 874. The BNB balance stays at 8. |
| JetStream redelivers the fill to a fresh accounting process | 0 | 0 | One trade remains. The next cash revision contains no additional order debit. |

The fixture repeats the same order with an XSIM timestamp 321 nanoseconds past a whole microsecond. Accounting validates the execution identity, posting terms and stored PostgreSQL time against the original fill. Both timestamps book one 126 USD debit. A second pair of signed orders executes at 321 ns and 789 ns within one microsecond. Their respective cash cutoffs show 1000, 874, 874 and 748 USD immediately before and at those two fills. Each cancellation and replay leaves its own execution and debit intact. The original evidence supplies venue execution time. Historical accounting knowledge time has the precision retained by PostgreSQL. [Cash projection](../services/accounting/internal/ledger/cash_projection.go)

The cash announcer also uses a clock with a 321 ns remainder. The accounting relay uses the exact cutoff in the sealed cash payload after checking its portfolio, revision, immutable cash history and PostgreSQL timestamp. The OMS accepts the matching frame and still quarantines a contradictory one. [Accounting cash outbox](../services/accounting/internal/cashoutbox/postgres.go)

For the compatibility run, build the accounting test binary in a checkout of `7077c75b` with the generated Go SDK. Run these commands in the indicated checkouts on Linux.

```sh
# From the 7077c75b Go module
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -mod=readonly -p 1 -c -o /tmp/kanz-7077-accounting ./services/accounting/internal/consume
# From the current repository root
python3 -B kanz/dev/synthetic_orders.py --native rust/target/debug/kanz-synthetic-oms --prior-accounting /tmp/kanz-7077-accounting
```

The historical process commits an unattributed 126 USD trade at cash revision 2. A fresh accounting process consumes the same JetStream delivery and attributes the original debit at revision 3. OMS releases the outstanding reserve. Both the prior history and the original publication retain their bytes. For an already published contradictory opening frame, the original OMS projection retains its quarantine. The drill creates an empty replacement database and uses the OMS workload's private gateway read to replay the sealed accounting opening with its exact cutoff. It verifies the result twice without switching the active writer. [Funded upgrade and recovery](../services/api_gateway/cmd/api_gateway/synthetic_cash_rebuild_test.go)

The terminal fact carries the signed cancellation as its cause and the independently observed confirmation time. A separate cash revision acknowledges the amount attributable to this order. The USD announcement excludes BNB, so new admission that requires complete portfolio coverage still refuses with unknown funding. Go retains production write authority. [Funded OMS](https://github.com/eighred/kanz/issues/1532) · [Cash reservations](https://github.com/eighred/kanz/issues/1316) · [Writer replay](https://github.com/eighred/kanz/issues/1533) · [Accounting](https://github.com/eighred/kanz/issues/1535)

## Read the same order in Chromium

Install the locked dependencies from `web/` with `npm ci`. Put the Node major from `web/.nvmrc` and the repository's Rust/WASM build toolchain on PATH. With the generated Go SDK and the Linux native executable from the build instructions above, run from the repository root:

```sh
python3 -B kanz/dev/synthetic_orders.py --native rust/target/debug/kanz-synthetic-oms --browser
```

This builds the Vue bundle and both Go probes. During the funded run, it passes the confirmed XSIM terminal receipt from `partial_cancel_and_book` directly to the signed gateway probe. While the same database is alive, the portfolio query uses the dedicated read-only role and checks the returned protobuf against the Rust bytes. Playwright then opens the built blotter through the real BFF, checks the recorded 0.625 filled and 0.625 unfilled out of 1.25, advances the page clock to observe a stale snapshot, refreshes, and checks the second tenant's refusal.

The runner obtains the pinned Microsoft Playwright Linux/amd64 image on first use. Chromium, gateway, BFF and PostgreSQL share the disposable loopback namespace. The fixture publishes no host ports. Temporary sessions stay between the probe processes, and the runner removes its database and staging directory on exit.

## Decisions and diagnostics

- **Write authority.** The signed gateway publishes a tenant-scoped command. The isolated control process resolves its principal/portfolio, synthetic quote, projected buying power, leverage and versioned mandate. PostgreSQL binds the evidence to the exact command digest. The synthetic Rust writer commits order, claim and outbox atomically. The production writer handoff has its own acceptance under [#1533](https://github.com/eighred/kanz/issues/1533).
- **Time and identity.** Effective market time and the time the evidence was observed are recorded separately. The database timestamps the resulting state transition. The control owner requires evidence that is available at the decision time. Native observations retain event identity and original economic bytes for replay.
- **Remote outcomes.** The venue owns its remote receipt in a separate schema. A failed acknowledgement creates a query obligation. Replay uses the durable dispatch claim and the same order identity. Decimal arithmetic preserves price and quantity across canonical Go, Rust and browser contracts.
- **Observability.** The gateway exposes an aggregate publish count on `/metrics`. The authenticated command carries a W3C trace; the accepted outbox fact preserves the trace and the command event ID. Each native invocation emits one bounded structured diagnostic on stderr. A missing control, a disputed payload and an uncertain commit have different receipts. [Implementation and acceptance decision](https://github.com/eighred/kanz/issues/1532#issuecomment-7031072628).

Install the synthetic schema first, then apply the atomic writer migration. Preexisting records retain an unknown epoch and prevent automatic enrollment. [Writer migration](../../rust/services/synthetic_oms/writer.sql)

The native run also enrolls the empty synthetic store and checks exclusive PostgreSQL writer generations. It kills a separate broker consumer after its native transaction commits and before ACK. It observes an actual in-flight SQL writer blocking handoff and a stale surviving native backend blocked by the handoff. The new writer reconciles the original delivery and stored order version. A further generation change preserves the quarantined venue outcome before the typed query resolves it. Both transfers use fresh database logins. An unknown legacy order or missing generation refuses enrollment or writing. The fixture operator alone advances the generation. Production failover and an operational Go-to-Rust-to-Go handoff retain separate acceptance. [Writer handoff](https://github.com/eighred/kanz/issues/1533) · [Pending cash](https://github.com/eighred/kanz/issues/1316)
