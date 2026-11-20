# Run an isolated synthetic order

An operator submits one USD spot limit order to XSIM through the normal signed Go gateway. The fixture starts a private JetStream and PostgreSQL database, exercises the Rust command owner, and reads the committed order back through the real OMS query and gateway. The local network namespace has no host ports or external route. The broker, database, identities and mock venue receipts expire with the run.

## Build and run

Use Python 3.11+, Docker with Linux amd64 support, the Go toolchain in `kanz/go.mod`, and the schema generators listed in the [development setup](README.md). Build the native binary on Linux amd64 with the repository's Rust 1.90 toolchain. Use the repository root for these commands.

```sh
(cd schemas && buf generate && bash bootstrap-go-sdk.sh)
(cd rust && cargo +1.90.0 build --locked -p kanz-synthetic-oms)
python3 -B kanz/dev/synthetic_orders.py --native rust/target/debug/kanz-synthetic-oms
```

On Windows, generate the schemas as described in the development setup, build the same Linux binary in a Linux checkout or builder, and pass its path to `python -B kanz/dev/synthetic_orders.py --native <linux-binary>`. The runner prints the executable SHA-256 and PostgreSQL role attributes. A separate command runs the ten existing component and gateway checks on hosts without a Linux Rust build.

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
| Refuse the database commit | A deferred trigger raises after order, command claim and outbox writes. The service reports an unconfirmed commit. An independent scoped read finds zero rows in all three tables before the retry. |
| Read the blotter | The authorized portfolio reader obtains the exact canonical state. A foreign tenant and an out-of-scope portfolio receive indistinguishable refusals. |

The HTTP interfaces used by the exercise are `POST /v1/orders`, `POST /v1/orders/{order_id}/cancel`, and `GET /v1/portfolios/{id}/orders`. The browser uses the corresponding `/api/v1/` routes through the web BFF. Submission uses an explicitly named order, `Idempotency-Key`, and canonical Protobuf JSON, including string Decimal coefficients. HTTP 202 records publication of the command. The state returned from the portfolio read records the durable outcome.

## Read the same order in Chromium

The browser probe requires the pinned Node and Playwright installation from `web/`, Chromium, a production web build, and a Go test binary for `./services/web_bff/internal/server`. Run it in the same loopback namespace as the disposable database. The read role needs scoped SELECT access to `orders`. Provide a receipt from the native owner, and keep the test DSN in the process environment.

```sh
(cd web && npm ci && npm run build)
(cd kanz && go test -p 1 -c -o /tmp/kanz-synthetic-bff ./services/web_bff/internal/server)
(cd kanz && go test -p 1 -c -o /tmp/kanz-synthetic-gateway ./services/api_gateway/cmd/api_gateway)
export SYNTHETIC_BLOTTER_BROWSER_PROBE=/tmp/kanz-synthetic-bff
export TEST_MFA_NODE="$(command -v node)"
export TEST_MFA_PLAYWRIGHT="$PWD/web/node_modules/playwright"
export TEST_MFA_STATIC_DIR="$PWD/web/dist"
export TEST_NATIVE_BLOTTER_SCRIPT="$PWD/web/e2e/native-blotter.cjs"
export SYNTHETIC_BLOTTER_RECEIPT_STDIN=1
/tmp/kanz-synthetic-gateway -test.run='^TestPersistedNativeOrderBlotter$' -test.count=1 < native-receipt.json
```

The probe expects a JSON object with `tenant`, `portfolio`, and `state`. The last field contains the base64 protobuf bytes returned in the Rust receipt's `state_b64`. The probe creates short-lived synthetic login sessions. It compares the complete protobuf on the HTTP read and checks exact values, freshness, refresh and tenant isolation in Chromium.

## Decisions and diagnostics

- **Write authority.** The signed gateway publishes a tenant-scoped command. The isolated control process resolves its principal/portfolio, synthetic quote, projected buying power, leverage and versioned mandate. PostgreSQL binds the evidence to the exact command digest. The synthetic Rust writer commits order, claim and outbox atomically. The production writer handoff has its own acceptance under [#1533](https://github.com/eighred/kanz/issues/1533).
- **Time and identity.** Effective market time and the time the evidence was observed are recorded separately. The database timestamps the resulting state transition. The control owner requires evidence that is available at the decision time. Native observations retain event identity and original economic bytes for replay.
- **Remote outcomes.** The venue owns its remote receipt in a separate schema. A failed acknowledgement creates a query obligation. Replay uses the durable dispatch claim and the same order identity. Decimal arithmetic preserves price and quantity across canonical Go, Rust and browser contracts.
- **Observability.** The gateway exposes an aggregate publish count on `/metrics`. The authenticated command carries a W3C trace; the accepted outbox fact preserves the trace and the command event ID. Each native invocation emits one bounded structured diagnostic on stderr. A missing control, a disputed payload and an uncertain commit have different receipts. [Implementation and acceptance decision](https://github.com/eighred/kanz/issues/1532#issuecomment-7031072628).

Install the synthetic schema first, then apply the atomic writer migration. Preexisting records retain an unknown epoch and prevent automatic enrollment. [Writer migration](../../rust/services/synthetic_oms/writer.sql)

The native run also enrolls the empty synthetic store and checks exclusive PostgreSQL writer generations. It kills a separate broker consumer after its native transaction commits and before ACK. It observes an actual in-flight SQL writer blocking handoff and a stale surviving native backend blocked by the handoff. The new writer reconciles the original delivery and stored order version. A further generation change preserves the quarantined venue outcome before the typed query resolves it. Both transfers use fresh database logins. An unknown legacy order or missing generation refuses enrollment or writing. The fixture operator alone advances the generation. Funded reservation, production failover and an operational Go-to-Rust-to-Go handoff retain separate acceptance. [Writer handoff](https://github.com/eighred/kanz/issues/1533) · [Pending cash](https://github.com/eighred/kanz/issues/1316)
