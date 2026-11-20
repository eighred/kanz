# Signed synthetic custody statements

The pilot imports a statement into the existing accounting reconciliation subject. The two examples in `testdata/` represent a fictional custodian, one account, one instrument and one completed UTC business day. Their signing key was discarded. The public authorization file is an operator input supplied independently of a statement.

## Signed format

A `kanz.synthetic-custody.v1` file is UTF-8 compact JSON with these outer keys in order: `format`, `source`, `tenant`, `account`, `revision`, `previous_sha256`, `complete`, `statement`. A single terminal LF is optional and participates in the signature and SHA-256. Supply a detached raw 64-byte Ed25519 signature. The public policy supplies the standard-base64 Ed25519 public key, source, tenant, portfolio, custodian, account, instrument IDs and currencies. The signed file can select only authorized values.

`statement` uses `accounting.v1.CustodianStatement` Protobuf JSON. Include explicit `positions`, `cash` and `transactions` arrays, a supported `grain` and UTC midnight `businessDate`. Use exact `coefficient` and `exponent` decimal objects. The adapter assigns `statementId` and `receivedAt`. Transaction-grain entries carry the external execution reference, instrument, signed quantity, price, signed cash, currency, trade date and any known settlement date. A line belongs to its UTC settlement day when supplied and to its UTC trade day otherwise. For example, a Monday trade settled on Tuesday appears on Tuesday�s close. A balances-only statement declares its grain and an empty transaction inventory. The completeness assertion is the synthetic source's signed assertion. An omitted file, partial inventory or refusal supplies no new statement.

Revision 1 uses an empty previous digest. Each correction for the same business date increments by one and identifies the complete preceding file by lowercase SHA-256. The accounting database receives both raw signed evidence and its canonical outbox message in one transaction. Identical retry uses the original receipt and flushes pending publications. Resubmit the same signed file after a broker interruption. Conflicting or skipped revisions fail admission. An individual file is limited to 1 MiB, and an account to 16 MiB of retained signed file bytes. Exhaustion refuses new files and preserves evidence for already accepted ones.

## Isolated pilot

Set `ACCOUNTING_CUSTODY_REVIEW_READ_ONLY=true` on the pilot accounting instance. Its break list exposes the portfolio, custodian and the comparison that produced each observed discrepancy. The read-only instance omits claim and explanation handlers. A historical detection with no recorded source has no `detection` field.

Apply the existing accounting migrations to a new `kanz_custody_pilot` database. From this directory, a database administrator on that database can apply the fixture grant once with `psql -v public_key="$(jq -r .public_key testdata/authority.json)" -f pilot_grants.sql`. Assign the resulting `kanz_custody_batch_pilot` NOLOGIN group to a dedicated NOSUPERUSER NOBYPASSRLS login with no other grants. The group can append the signed fixture's receipts and publish its statement outbox partition. Point the separate accounting runtime at the same database with its accounting role. Mount connection settings through each process's own `ACCOUNTING_DATABASE_URL_FILE`.

Enable the following settings in the importing process and the accounting consumer for this synthetic tenant:

```sh
export ACCOUNTING_TENANT=synthetic-custody-pilot
export ACCOUNTING_CUSTODY_PAIRS=PF1:CUST-A
export ACCOUNTING_CUSTODY_ACCOUNTS=PF1:CUST-A:sandbox-account
export ACCOUNTING_CUSTODY_SIGNED_BATCH_ONLY=true
```

Use the normal `ACCOUNTING_NATS_URL` and bootstrap the existing stream for `accounting.custody.statement`. Provision a dedicated publisher principal in the isolated broker. Within that principal's user block, include `kanz/infra/nats/custody_batch_permissions.conf`. A remote broker additionally requires the accounting SPIFFE socket and its workload identity. The default deployment provisions no publisher. The signed-only consumer verifies the receipt and exact canonical payload before its first write and each redelivery.

From this directory run:

```sh
go run . -authority testdata/authority.json -batch testdata/statement-r1.json -signature testdata/statement-r1.sig
go run . -authority testdata/authority.json -batch testdata/statement-r2.json -signature testdata/statement-r2.sig
```

Each successful command prints a receipt ID containing the source, revision and complete signed-file digest. `r1` reports 9.5 SYN. The matching book execution and independent external reference `external-7` describe 10.5 units at USD 1 for cash of âˆ’10.5. Reconciliation therefore observes a one-unit position break. `r2` corrects the position to 10.5. The ordinary accounting run's `statement_id` links to the receipt's source, revision, digest, UTC receipt time and predecessor. An authorized accounting operator can inspect this link in the dedicated tenant session without disclosing the signed material:

```sql
SELECT r.statement_id, r.source_id, r.revision, r.sha256, r.previous_sha256,
       r.received_at, s.grain, s.business_date
FROM custody_batch_receipts r
JOIN custody_statements s USING (tenant_id, statement_id)
ORDER BY r.revision;
```

## Operator-initiated comparison

The read-only pilot keeps manual reruns disabled. For a separately authorized operator environment, configure the same dedicated custody comparison role in `API_GATEWAY_CUSTODY_RERUN_ROLE` and `ACCOUNTING_CUSTODY_RERUN_ROLE`. The gateway requires it to differ from its other grant roles. Writable accounting requires `ACCOUNTING_CUSTODY_SIGNED_BATCH_ONLY=true`, its durable database, broker, and declared custody pairs. Startup refuses a rerun role with unsigned intake. `ACCOUNTING_CUSTODY_REVIEW_READ_ONLY=true` keeps the write route closed even when a role is configured.

An operator with custody read access and the comparison role can open **Custody breaks**, enter a portfolio, custodian and completed UTC business date, review them, and run the comparison. The gateway accepts `POST /v1/portfolios/{id}/custody/reruns` with `custodian_id`, `business_date` and a stable lowercase UUID-v4 `request_id`. The session supplies tenant, subject and portfolio grants. Accounting reads the stored statement and journal basis, then returns the committed comparison receipt. Retry the same request ID after an unconfirmed outcome. A new request starts a new comparison.

## Reproduce the operator workflow

From the repository root, prepare the local generated Go SDK using `schemas/README.md`. Install the Go toolchain from `kanz/go.mod`, Node matching `web/.nvmrc`, npm, Python 3, and a local Docker engine with Linux containers. Then run:

```sh
rustup toolchain install 1.90.0 --profile minimal --target wasm32-unknown-unknown
(cd web && npm ci && npx playwright install chromium)
docker pull postgres@sha256:aa6cb33cf4c19c765cefcf48d96176258800870a9a1e930ad126200f9d825139
python kanz/dev/custody_pilot.py
```

On Linux, install Chromium's OS libraries with `npx playwright install-deps chromium` from `web` if needed. Use `--node /absolute/path/to/node` for a separate Node installation. The runner builds `web/dist` through the normal npm build, compiles the three Go test executables, and starts a fresh loopback PostgreSQL instance with SCRAM and separate restricted accounting and source logins. It uses the two checked-in signed statements, the publisher grant and a real loopback JetStream. Chromium reads the r1 difference and its retained run and statement through the BFF and authenticated gateway on initial load and reload. The three restricted sessions exercise foreign tenant, excluded portfolio and missing Fund permission. Accounting then verifies that the r2 correction clears the break. The command exits successfully only after these checks pass. It removes the temporary database and test executables on exit.

The required Go quality job runs the same command with `--pull-image --receipt`. Its `custody-pilot` artifact records the checked-out commit and tree and the observed comparisons in both isolated databases, including the signed statement IDs, run IDs, business date, JetStream completion sequences and outstanding break counts. To retain the synthetic results locally, pass `--receipt comparisons.json` with a path outside the checkout.

The runner builds the executables once and starts a second fresh PostgreSQL instance for the authorized operator. Both fixtures enforce the signed intake grant and refuse manual claim/explanation at the database. The read-only pilot additionally refuses operator rerun IDs in PostgreSQL even when a service calls the reconciler. The separate writable fixture gives its restricted accounting login permission to update only the manual receipt column on an existing run. A signed operator session repeats the same request after browser reload. The receipt and JetStream completion identify one operator, request, run and source; the signed correction then clears its queue. No customer or shared database is used.

## Verification

With a fresh provisioned fixture database, set `TEST_CUSTODY_BATCH_DATABASE_URL` to its dedicated source role and `TEST_POSTGRES_URL` to its separate restricted accounting role. In this directory run `go test -p 1 -run TestSignedPilotCommandRestricted -count=1 -v .`. The test starts its own loopback JetStream and reads both retained publications back through the accounting consumer. Run `TestSignedCustodyBatchReconciliationOnRealSpine` in `../../internal/custody` against a **separate disposable migration-test database** and a bootstrapped `TEST_NATS_URL`. That suite resets accounting tables. It covers booking, comparison, immutable revision conflicts, publication recovery and cross-tenant refusal.

Production source rights, custody account assignment, file delivery, authorized key rotation and customer acceptance are separate operating inputs tracked by #1326 and #106.

### Review a dated result

Set `API_GATEWAY_CUSTODY_READ_ROLE` to a distinct identity role for customer readers. Issue the role together with the gateway baseline and an explicit portfolio allow-list. The Fund role retains its existing access. In the read-only pilot, both custody reads work with the customer role alone beyond baseline.

In **Custody breaks**, enter the portfolio, custodian and completed UTC business date under **Dated custody result**. The gateway forwards `GET /v1/portfolios/{id}/custody/comparison?custodian_id=...&business_date=YYYY-MM-DD`. Accounting reads the stored runs and statement receipts in one PostgreSQL snapshot. A recorded result identifies the run and completion time. A confirmed comparison also identifies the signed statement, scope mode and exact journal position. The result requires an immutable signed-source digest recorded with that run and a matching current receipt and materialized statement.

**No statement received** means a run recorded the absence for that date. **No comparison recorded** means no run exists. **Statement awaiting comparison** identifies a newer statement that still needs a matching run. A later journal append effective for the selected close also puts the result back into a pending state. Ambiguous receipts, conflicting comparisons at an equal journal position, or unverified source evidence yield **Result unknown**. An empty outstanding-break queue alone establishes none of these outcomes.


Choose **Download comparison receipt** to keep the dated result. Transaction coverage identifies a balances-only statement, a comparison of independent references, or a historical run with unknown coverage. A reference comparison reports the book and statement counts and the number of book trades that lacked an independent reference. These counts describe the selected UTC close.

To recover a prior comparison after a correction, enter the original run ID with the same portfolio, custodian and date. The same GET accepts the optional `run_id` query parameter. A verified historical receipt includes the original book, custodian and difference figures, their break keys, and the signed statement digest. An exact measured zero stays zero. A missing amount stays unavailable. The service reads the retained completion and checks the original signed statement. It returns unknown when the historical evidence cannot establish the result. A later queue state cannot supply the missing figures.

The accounting writer inserts the completion bytes with the run and enqueues its FACT in the same transaction. Migration 0028 protects a retained row and its book basis after insertion. Older scheduled runs can replay the original relay record while it exists; a historical customer read requires retained, signed evidence. A legacy operator writer can finish its original receipt only with the matching completion FACT.
