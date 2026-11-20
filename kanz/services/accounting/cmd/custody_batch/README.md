# Signed synthetic custody statements

The pilot imports a statement into the existing accounting reconciliation subject. The two examples in `testdata/` represent a fictional custodian, one account, one instrument and one completed UTC business day. Their signing key was discarded. The public authorization file is an operator input supplied independently of a statement.

## Signed format

A `kanz.synthetic-custody.v1` file is UTF-8 compact JSON with these outer keys in order: `format`, `source`, `tenant`, `account`, `revision`, `previous_sha256`, `complete`, `statement`. A single terminal LF is optional and participates in the signature and SHA-256. Supply a detached raw 64-byte Ed25519 signature. The public policy supplies the standard-base64 Ed25519 public key, source, tenant, portfolio, custodian, account, instrument IDs and currencies. The signed file can select only authorized values.

`statement` uses `accounting.v1.CustodianStatement` Protobuf JSON. Include explicit `positions`, `cash` and `transactions` arrays, a supported `grain` and UTC midnight `businessDate`. Use exact `coefficient` and `exponent` decimal objects. The adapter assigns `statementId` and `receivedAt`. Transaction-grain entries carry the external execution reference, instrument, signed quantity, price, signed cash, currency, trade date and any known settlement date. A line belongs to its UTC settlement day when supplied and to its UTC trade day otherwise. For example, a Monday trade settled on Tuesday appears on the Tuesday close. A balances-only statement declares its grain and an empty transaction inventory. The completeness assertion is the synthetic source's signed assertion. An omitted file, partial inventory or refusal supplies no new statement.

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

### Compare an earlier close after the next close

An authorized operator can rerun an earlier date after a later comparison has taken ownership of the live break queue. Accounting requires the latest materialized signed revision for the earlier date, an independently verified effective account assignment, and the same journal prefix at measurement and commit. The receipt records the dated comparison and existing execution measures. Use its run ID to reopen the signed evidence. The live queue, the later close, and the preceding signed receipts retain their own authority and identifiers. Replaying the same request returns the original completion.

Apply accounting migration 0034 and upgrade the Accounting readers and writers together before enabling manual historical comparison in the pilot. Both new outcome values are additive to the canonical schema. Earlier readers require a compatible SDK upgrade to display the new outcomes.

## Reproduce the operator workflow

Start in a clean checkout of the pilot revision. Use its Go toolchain (`kanz/go.mod`), Node major (`web/.nvmrc`), npm, Python 3, Buf 1.69.0 and a local Docker engine with Linux containers. Run from the repository root. The final command writes synthetic results outside the checkout.

```sh
git status --porcelain
make -C kanz bootstrap
export PATH="$(go env GOPATH)/bin:$PATH"
make -C kanz generate
rustup toolchain install 1.90.0 --profile minimal --target wasm32-unknown-unknown
(cd web && npm ci && npx playwright install chromium)
evidence=$(mktemp -d)
git rev-parse HEAD 'HEAD^{tree}' > "$evidence/source.txt"
python -B kanz/dev/custody_pilot.py --pull-image --receipt "$evidence/comparisons.json"
```

An empty `git status --porcelain` identifies a clean checkout. On Windows without make, follow the repository's Windows setup for the approved Buf executable. Install the two Go plugins pinned in `kanz/Makefile`, add their directory to PATH, then run `(cd schemas && buf generate && bash bootstrap-go-sdk.sh)` in Git Bash. The bootstrap creates the ignored `schemas/gen/go/go.mod` used by accounting and the gateway. On Linux, install Chromium's OS libraries with `npx playwright install-deps chromium` in `web` if needed. Use `--node /absolute/path/to/node` to select a separate Node installation. With the pinned PostgreSQL image already present locally, omit `--pull-image`.

The runner builds the production web bundle, accounting service, signed batch importer, custody proof importer and four test executables. It provisions a new loopback PostgreSQL database per profile, with SCRAM and separate restricted logins. JetStream receives the signed source and the accounting completion. Chromium accesses the dated comparison through the BFF and authenticated gateway. The runner removes its temporary databases and executables on exit.

On the first database, the normal accounting service must refuse startup before account enrollment and after the venue identity alone. It must answer its readiness probe after the separate governance issuer signs the custody assignment, refuse startup after withdrawal and become ready again after reinstatement. The script runs each initial signed import twice with the issuer’s restricted login. The optional JSON receipt records the five startup outcomes and the dated store verification under `account_onboarding`. A separate database on the first instance checks current and retained reads, the overlap of the two effective intervals and concurrent enrollment during commit. The other two pilot environments enroll their own issuers and verify the same initial signed evidence. See the [issuer command and provisioning requirements](../custody_proof/README.md).

The required Go quality job runs the same command with `--pull-image --receipt`. Its `custody-pilot` artifact records the checked-out commit and tree and the observed comparisons in seven isolated databases, including the signed statement IDs, run IDs, business date, JetStream completion sequences and outstanding break counts. To retain the synthetic results locally, pass `--receipt comparisons.json` with a path outside the checkout.

The runner builds the executables once and starts a second fresh PostgreSQL instance for the authorized operator. Both fixtures enforce the signed intake grant and refuse manual claim/explanation at the database. The read-only pilot additionally refuses operator rerun IDs in PostgreSQL even when a service calls the reconciler. The separate writable fixture gives its restricted accounting login permission to update only the manual receipt column on an existing run. A signed operator session repeats the same request after browser reload. The receipt and JetStream completion identify one operator, request, run and source; the signed correction then clears its queue. No customer or shared database is used.

A third disposable database uses the separately signed fixture pair in `testdata/instrument_units/`. Its public authority applies only to that database. Upgrade the signed importer and accounting readers before issuing statements with the additive `quantityInInstrumentUnits` field. Older strict Protobuf JSON parsers reject that field. Legacy signed files and saved completions keep their original interpretation. The source explicitly identifies each trade quantity as the instrument's natural units. In the first retained comparison the book shows 10.5 SYN and the statement shows 9.5 SYN for `external-7`. The correction shows 10.5 on both sides. The browser downloads both historical receipts. The runner binds them to the database run IDs and statement digests and includes them under `customer_downloads` in the optional receipt file.

### Restore the signed pilot to an earlier point

From the repository root, use the same local prerequisites as the operator workflow.

```sh
python3 -B kanz/dev/custody_pilot.py --pitr --receipt custody-recovery.json
```

The drill takes and verifies a physical PostgreSQL base backup before the synthetic book is populated. It archives a named WAL restore point after the first two signed closes, then records a signed correction and archives its later WAL segment. It removes the disposable primary and restores into an independent PostgreSQL container, paused read-only at the named target. The customer opens the original receipt through the signed Gateway, BFF and Chromium. The runner compares its downloaded bytes with the source and checks that the later signed correction is absent at the target. It reports the backup bounds, archived segments, replay LSN and local duration. This drill is for the synthetic Accounting estate. Approved estate recovery is tracked under #106 and #1100.

### Review an execution quantity discrepancy

Apply accounting migration 0031 before starting the new reader or writer. Grant the accounting writer SELECT and INSERT on `custody_execution_quantity_observations` and USAGE on its identity sequence. A separate accounting reader needs SELECT. Keep the statement source login limited to its existing intake grants.

For a signed single-account scope, **Custody breaks** can show a balance discrepancy and a separate row for the executed trade. The trade row identifies its external reference, instrument, measured book and statement quantities, and the run that detected it. A newer signed revision clears that item only when it measures equality for the same attributed execution in the same instrument units. A missing line or an older balances-only statement leaves it open. A change in account authority requires separate review.

The pilot receipt exposes the working count under `outstanding_breaks` and the original completion count under `retained_breaks`. In the quantity fixture, the original working count is two and the sealed balance comparison contains one break. After the signed correction both counts are zero. The original download still describes the earlier discrepancy.

A fourth disposable database uses the three signed files in `testdata/late_quantity/`. February 4 opens with 10.5 SYN on the book and 9.5 on the statement for execution `external-7`. February 5 closes clean. The second February 4 statement raises the execution quantity to 10.5. Accounting links the agreed quantity to the original detection and journal entry, removes that measured discrepancy from the queue, and retains all three completion FACTs. Both earlier completions keep their original bytes.

For the corrected date, the full comparison stays pending. Select **View corrected execution quantities** to inspect and download the separate saved correction. Its scope covers the listed trades and quantities. The customer can still recover the original February 4 comparison and the February 5 close by their run IDs. The pilot records the independent accounting observation and the downloaded receipt under `historical_quantity_correction`.

## Verification

With a fresh provisioned fixture database, set `TEST_CUSTODY_BATCH_DATABASE_URL` to its dedicated source role and `TEST_POSTGRES_URL` to its separate restricted accounting role. In this directory run `go test -p 1 -run TestSignedPilotCommandRestricted -count=1 -v .`. The test starts its own loopback JetStream and reads both retained publications back through the accounting consumer. Run `TestSignedCustodyBatchReconciliationOnRealSpine` in `../../internal/custody` against a **separate disposable migration-test database** and a bootstrapped `TEST_NATS_URL`. That suite resets accounting tables. It covers booking, comparison, immutable revision conflicts, publication recovery and cross-tenant refusal.

Production source rights, custody account assignment, file delivery, authorized key rotation and customer acceptance are separate operating inputs tracked by #1326 and #106.

### Review a dated result

Set `API_GATEWAY_CUSTODY_READ_ROLE` to a distinct identity role for customer readers. Issue it with the gateway baseline and an explicit portfolio allow-list. A session with the customer role can inspect its permissions and read the custody queue and dated receipts. This role restricts the session even if its token also carries a staff grant. Keep staff identities for Fund and authorized reruns separate from customer identities. Send the customer to `/custody-breaks` to start the review after sign-in.

In **Custody breaks**, enter the portfolio, custodian and completed UTC business date under **Dated custody result**. The gateway forwards `GET /v1/portfolios/{id}/custody/comparison?custodian_id=...&business_date=YYYY-MM-DD`. Accounting reads the stored runs and statement receipts in one PostgreSQL snapshot. A recorded result identifies the run and completion time. A confirmed comparison also identifies the signed statement, scope mode and exact journal position. The result requires an immutable signed-source digest recorded with that run and a matching current receipt and materialized statement.

**No statement received** means a run recorded the absence for that date. **No comparison recorded** means no run exists. **Statement awaiting comparison** identifies a newer statement that still needs a matching run. A later journal append effective for the selected close also puts the result back into a pending state. Ambiguous receipts, conflicting comparisons at an equal journal position, or unverified source evidence yield **Result unknown**. An empty outstanding-break queue alone establishes none of these outcomes.

Before an ordinary signed comparison can update the queue, Accounting checks
its journal prefix under the same portfolio lock used for ledger appends. A
posting that changes the selected close leaves the earlier break and completion
in place. The scheduler can compare again on its next tick. A manual retry
uses the same request ID. A posting for an unrelated future close can advance
the journal without invalidating the selected comparison.

A signed comparison must fit the retained receipt limits of 4,096
breaks and 1 MiB of serialized completion data. Accounting records a failed
comparison if either limit is exceeded. The previous receipts and working
queue stay in place.

The importer and reconciler check the serialized FACT, stamped envelope and
NATS headers against the connection's advertised payload limit before recording
a new receipt. A source file that cannot be published leaves no intake receipt.
A comparison that cannot be published retains the existing queue and records a
compact failure when the failure FACT fits. If the connection has no current
capacity, retry the same operator request after it reconnects. Previously
committed receipts are replayed from the outbox. Broker acknowledgement records
actual publication.


An enrolled account’s current comparison becomes pending when its assignment has changed or the original run lacks assignment evidence. A saved run keeps the assignment version that governed the comparison. The verified indicator in its download means that accounting rechecked both issuers’ signatures, their authorized scopes and the effective business date. Earlier receipts keep their original evidence fields.

If the scheduled close finds no statement, **Dated custody result** shows **No statement received**. Save its download or run ID. After a late signed file for that date is received, the same date shows **Statement awaiting comparison** until the next scheduled run for that close. While the signed intake is waiting for delivery to Accounting, the scheduler records neither an absence nor a comparison of an older revision. Leave **Saved run ID (optional)** empty to see the resulting comparison. Enter the earlier run ID to reopen and download the original absence receipt for that portfolio, custodian and date. The absence receipt records what Accounting observed at that run's completion time. It carries no statement digest, account-level balances or transaction claims. The pilot writes this receipt and its download digest under `missing_statement_recovery`.

Choose **Download comparison receipt** to keep the dated result. Transaction coverage identifies a balances-only statement, a comparison of independent references, or a historical run with unknown coverage. A reference comparison reports the book and statement counts and the number of book trades that lacked an independent reference. These counts describe external reference membership at the selected UTC close. Equal counts and matching balances can coexist with two offsetting trade-quantity errors on the same references. For signed lines with explicit instrument units, the saved-run view also shows independently matched quantities in a separate section. The execution economics section shows the separate comparison for lines that declare a supported convention. Earlier membership-only receipts preserve their original scope. In the instrument-unit pilot, the first revision opens two working rows for the net position quantity difference and an execution quantity difference on reference `external-7`. The corrected revision clears both. The original saved completion still contains its one position break and its separate per-execution quantity figures. The read-only customer can inspect both working rows and download the original completion.

If a quantity correction arrives after the following close, open the original business date and choose **View corrected execution quantities**. Copy the **Original detection run** for the execution into **Saved run ID (optional)** and choose **View dated result**. From a verified full comparison for a later signed revision of the same date, choose **View previous statement comparison** to open the first full comparison of the immediately preceding signed revision. The previous receipt is available when the newer revision still has discrepancies. Accounting checks both original receipts, the signature chain, and their matching account assignments. An absent or ambiguous first comparison yields **Result unknown**. A saved run ID also remains usable with its original portfolio, custodian and date. The same GET accepts the optional `run_id` query parameter. A verified historical receipt includes the original book, custodian and difference figures, their break keys, and the signed statement digest. An exact measured zero stays zero. A missing amount stays unavailable. The service reads the retained completion and checks the original signed statement. It returns unknown when the historical evidence cannot establish the result. A later queue state cannot supply the missing figures.

On a saved comparison with a verified account and a completed transaction-reference check, choose **View custodian-reported price and cash**, then **Download comparison receipt**. Each line identifies the execution reference, instrument, reported unit price, and signed cash with its currency. An absent amount appears as **Unreported**. An empty signed list shows zero execution lines. The download binds these observations to the original statement digest. The currency describes the cash leg. To compare these values with booked executions, open **View original comparison receipt** and inspect **Execution economics**. If the optional details exceed the receipt limit, the original comparison is still available to download.

The series in `testdata/execution_economics/` contains four signed revisions for the same execution. The pilot runs the `custody_batch` executable with each file, signature and source authorization. A running `accounting` service consumes the statement from JetStream. The configured scheduler compares it with an independently booked synthetic fill and serves the result to the normal dated view. After the fourth revision, a fresh accounting consumer replays and acknowledges all four statements. The pilot checks the stored completions, financial state and completion count across this replay. Its convention, `unit_price_net_cash_same_currency_commission_v1`, specifies signed natural units, a unit price in the cash currency, an explicit commission in that currency, and same-day posted settlement with no other charges. Gross cash is the negated signed quantity times the unit price. Net and settled cash equal gross cash less commission. A zero commission must be explicit. Compare book, signed custodian and book-minus-custodian for each measure. The book uses the original journal position and account assignment. Apply accounting migration 0033 for the approved commission lookup.

For the 10.5 SYN purchase at USD 1, both sources show gross cash of −10.50 USD. The book records a 0.25 USD fee and −10.75 USD settlement. The first signed statement declares a 0.50 USD fee and −11.00 USD settlement. The second agrees with the book. The third contradicts its own cash calculation and the fourth omits the convention. Both show **Unknown** with a reason. The runner reopens the first receipt after the correction and verifies identical download bytes. In the exported `execution_economics` list, compare the four statement digests, statuses, reasons and monetary figures across clean runs. Run IDs and completion times identify each independent execution. Download hashes identify the bytes recovered within that execution. An actual custodian must declare its own convention before these measures can be assessed.

The booked-fee profile keeps the first signed file unchanged while an
independent synthetic fee correction raises the booked commission from
0.25 to 0.50 USD. After the fee posting, the live comparison is pending
until Accounting compares the new journal position. The two execution
measurements then agree at −11 USD of settled cash. The statement's unchanged
aggregate cash of −10.75 USD leaves a separate −0.25 USD balance difference.
Choose **Find earlier book comparison** on the newer result to reopen the
saved receipt for the same signed statement at the nearest earlier journal
position. A saved run ID also opens its specific receipt. The lookup requires
verified matching account scope and tolerance. If two candidate captures tie
at the selected position, the report shows that no earlier comparison could
be verified. This lookup also works for the first signed statement revision.
The pilot verifies identical
original download bytes and records both receipts in `booked_fee_correction`.

The accounting writer inserts the completion bytes with the run and enqueues its FACT in the same transaction. Migration 0028 protects a retained row and its book basis after insertion. Older scheduled runs can replay the original relay record while it exists; a historical customer read requires retained, signed evidence. A legacy operator writer can finish its original receipt only with the matching completion FACT.

The working queue at `GET /v1/custody/breaks` returns the complete authorized result through 4096 rows. Above that bound it returns HTTP 422 with code `custody_queue_pagination_required`. Request `?limit=100` to page, and send the opaque `next_cursor` in the next request as `?limit=100&after=...`. Limits range from 1 to 4096. Each response's `count` describes that page. An absent cursor marks the end. Each page reads both working sources in one snapshot, ordered by break ID, under the current principal's portfolio scope. As operators and corrections change the live queue, refresh from the first page to see newly inserted earlier keys.
