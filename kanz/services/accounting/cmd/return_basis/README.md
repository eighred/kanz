# Dated Accounting close evidence

`return_basis` retains an independently signed synthetic or governed Accounting
record. An authorized migration operator enrolls the issuer public key and
complete cash-source scope for a tenant, portfolio and ISO currency in
`return_basis_authorities`. The source login may select the authority and
select and insert its receipts through `return_basis_receipts`. Keep both
roles restricted by forced tenant RLS. The issuer private key belongs with the
issuer. The reporting login reads the retained evidence.

The first format covers a single UTC day ending at midnight on `close_date`.
Opening and closing contain exact decimal totals and retained Accounting NAV
version references. The issuer signs the two NAV versions, the complete
external-flow scope, its completion reference and an explicit event count
under a single knowledge horizon. The scope identifies the entire enrolled
fund flow inventory. A missing NAV or missing completion is an explicit JSON
null. A signed zero event count records zero external flows. Offsetting
contributions and withdrawals still have a nonzero count.

An edition with exactly one off-venue subscription or redemption in the reporting
currency may include a signed `split` after `event_count`. Its fields in order
are `movement_id`, `command_digest`, `effective`, `signed_amount`, `before`
and `after`. The cash instant must be strictly inside the day. Both NAV records
refer to the same instant on either side of the booking. Their references must
be distinct, and the exact NAV change must equal the signed cash amount.
Explicit null in either NAV leaves the return unassessed. Multiple cash movements,
or a positive count with no split, cannot produce a measured return.

Accounting matches the accepted cash command to its immutable journal entry and
counts cash entries across all currencies and accounts under the signed knowledge
horizon. This includes the ledger's knowledge time and recorded booking timestamp.
The source login needs read access to the referenced columns in `cash_commands`
and `ledger_entries` under tenant RLS. It cannot write either table. The signed
issuer supplies the retained NAV versions and attests the full flow inventory.

The authority operator can enroll one total-return index and its separate
Ed25519 issuer per reporting scope in `return_basis_benchmark_authorities`.
Grant the restricted source, consumer and reader SELECT on that table. Only the
operator may enroll keys. Each benchmark observation binds the tenant,
portfolio, currency, index definition date, start and end dates, knowledge
time, and exact positive opening and closing index levels. It is signed as
compact canonical JSON in `BenchmarkObservation` field order. Its SHA-256
digest is the source version.

An optional final `benchmark` object in the close bundle carries
`observation` followed by `signature`, which is the base64-encoded 64-byte
benchmark signature. Both fields are null when the observation is unavailable.
Accounting rechecks both signatures and matches the observation's exact UTC
window, currency and knowledge time to the report. An authentic observation
for a different window or a missing observation leaves the comparison
unassessed while preserving any measured portfolio return. An observation
learned after the reporting horizon is refused at intake. A benchmark
correction is a new close edition with a later horizon and an updated
signed observation. This keeps earlier editions and their benchmark versions
addressable by their original horizon.

The signed `position_basis` object follows the optional `benchmark`. It
contains `opening`, `at_flow` and `closing` in that order. A price snapshot
names its `reference`, `effective` time, `known_at` time and a `prices` map
of instrument IDs to exact reporting-currency price strings. Use the same
marks for both sides of an intraday cash flow. A null required snapshot or
an incomplete set of prices leaves position contributions unassessed.
Known-at times must be within the signed reporting horizon.

Accounting uses the traded journal and standard NAV calculation to verify the
full held inventory and exact NAV at each snapshot. Grant the source SELECT on
the journal columns used by the recorded read. The calculation uses journal
knowledge and recording times within the signed horizon. The pilot covers
unchanged long holdings, a single reporting currency for cash and accrued
balances, and at most one verified external cash flow. Fees and booked accruals
flow into the cash and accrual component. Trades, corporate actions affecting
the window, unverified cash movements and unstated prices withhold the
breakdown. The signed format permits up to 32 prices per snapshot. The bounded
journal read withholds the breakdown above 4,096 entries.

For each subperiod, divide its component P&L by the invested opening NAV.
With one flow, scale each first-subperiod contribution by the second period's
growth factor and add its second-subperiod contribution. The two subperiods
exclude the capital movement from cash and accrual P&L. Accounting verifies
that the exact P&L sum equals the NAV change net of external capital and that
linked contributions sum to the signed portfolio TWR. P&L is protobuf Decimal
and linked fractions are statistical returns. This method measures absolute
portfolio TWR contribution. Benchmark comparison remains a separate result.

To assess intraday trading, add `trade_coverage` after the three position
snapshots. Its `reference` versions the full `fills` list. Each fill names the
immutable ledger `entry_id` and exact UTC `effective` time. An explicit
`fills: null` records missing coverage. The importer and Accounting consumer
read the booked execution evidence only for trades inside the report window
and need SELECT on `ledger_entries.execution_evidence` for trading reports.
The raw fill evidence stays in Accounting. Reports carry the coverage
reference, not individual fill payloads.

The booked-trades method reconciles the signed fill inventory and timestamps
to the journal. It requires complete scope, price and explicit execution fee
evidence, and verifies signed quantity, execution price, and net cash against
the booked fill. For each subperiod, security P&L is closing marked value minus
opening marked value minus signed traded principal. Execution costs are the
negative of the booked commissions. Other cash and accrual P&L removes each
fill's net cash from the booked cash/accrual change. Link all three before
converting to statistical fractions. The external subscription is excluded at
the NAV boundary. An execution at the same instant as that flow, intraday
short holdings or a separate fee correction withholds this pilot breakdown.
The original unchanged-holdings method keeps its existing eligibility.

Set the signed top-level `sector_attribution` flag to request a daily
Brinson–Hood–Beebower breakdown. This method covers a single UTC day with
fully invested unchanged long holdings, no flows or other in-window activity,
and a measured benchmark in the same currency and window. Intraday absolute
contribution uses its existing method and remains available.

The benchmark signer can append `composition` to the signed observation.
This object contains a `reference` and complete `constituents` list. Each item
contains `instrument_id`, exact opening `weight` and exact period `return`.
Accounting checks the weights sum to one and their weighted return equals
the signed overall index return. Portfolio weights and returns come from the
dated reconciled journal valuation.

Datamaster owns the classification. Its mounted reference CSV accepts optional
`sector_effective_date` for a sector-only row. The date covers one exact UTC
valuation day and `as_of` identifies the source revision. Both sector taxonomy
and code are required for a known classification. A dated row with both fields
empty records an explicitly unknown sector. These rows use the existing
survivorship engine for that day and do not change the current golden projection.
The Datamaster projector appends a batch transaction to
`sector_classification_history` with a PostgreSQL-assigned knowledge time.
It rejects conflicting contents under the same source revision and re-reading
an older drop leaves the newest known revision intact.

Set `DATAMASTER_SECTOR_HISTORY_LISTEN` on the Datamaster process and
`ACCOUNTING_DATAMASTER_HISTORY_URL` on both the importer and Accounting
consumer. The private endpoint uses an Accounting-only workload certificate
allow-list with SPIFFE configured. Plaintext is limited to literal loopback.
It is separate from the ordinary Datamaster listener. The Datamaster runtime
requires SELECT and column-level INSERT with no write privilege on `known_at`.
Accounting uses HTTP, with no Datamaster database credentials.

Accounting queries the exact UTC start day and signed knowledge horizon.
The result references the digest of the Datamaster observations used. It
evaluates missing dates, missing or unresolved instruments, incomplete
benchmark composition and a sector absent on either side as unassessed.
A transport failure leaves a new report retryable. Observed reports and
redeliveries use retained bytes, including reports originally published from
the earlier synthetic snapshot. Drain any pending editions from that earlier
writer before switching the report consumer. The legacy Accounting snapshot
table remains in the migration history for already published editions.

The authorized Exposure screen shows allocation, selection, interaction and
total relative effects in percentage points. The pilot uses two synthetic
sectors and a benchmark-only constituent whose historical classification
changes. Portfolio return is 14.5% and index return is 6%. The first
classification yields +0.8, +7.125 and +0.575 points. Its restatement yields
+2, +3 and +3.5 points. Both sum to the same +8.5-point active return and the
earlier knowledge horizon reopens the earlier distribution.

Submit compact canonical JSON in the order shown by the Go Evidence type, with
a separate raw 64-byte Ed25519 signature. The `as_of` timestamp uses RFC 3339
UTC with at most microsecond precision. The first edition has
`previous_digest` set to an empty string. Each correction signs the digest of
the last edition and a strictly later knowledge time. Exact replay returns
the same digest. There is one enrolled issuer per scope in this pilot.

Run with explicit `ACCOUNTING_DATABASE_URL` and `ACCOUNTING_TENANT` using the
restricted source login. Enable the controlled pilot with
`ACCOUNTING_DAILY_RETURN_ENABLED=true` on the command and Accounting service.

```sh
go run ./services/accounting/cmd/return_basis -evidence close.json -signature close.sig
```


The importer commits the signed receipt and its typed return FACT to the
Accounting outbox in one transaction. Grant the source conditional insert
columns `portfolio_id,currency,close_date,as_of` on
`return_basis_publications`, plus the normal Accounting outbox insert/read
and sequence privileges. Grant only the Accounting consumer update access to
`observed_payload`. The consumer verifies the broker payload against the
signed source before saving it. Replays preserve the exact first payload.

Open **Daily return** on the portfolio's Exposure page. Enter the UTC close
date, reporting currency and exact knowledge time. The panel distinguishes
a measured zero from an unassessed period. For a measured split it shows the cash
amount and effective time along with the linked result. Enter the earlier knowledge
time to reopen the original published result after a correction.
The same result shows the benchmark return, active return in percentage points,
and signed benchmark digest when a comparison is supported.

For the isolated acceptance, generate the Go schema SDK and install the
locked Web dependencies and Chromium. From the repository root run
`python -B kanz/dev/daily_return_pilot.py`. It applies Accounting and Datamaster
migrations to separate disposable databases, builds the normal Datamaster,
importer and Accounting binaries, and exercises real JetStream redelivery and
the authenticated gateway/BFF in Chromium. The Datamaster fixture loads two
effective dates, corrects the earlier one, verifies persisted revisions after
restart and stops Datamaster before the broker replay.

The fixture books a USD 40 subscription and values the PostgreSQL book through
Accounting using synthetic prices. With opening NAV 100, pre-flow NAV 110,
post-flow NAV 150 and close NAV 165, the linked return is
`(110/100) × (165/150) − 1 = 21%`. The close-to-close NAV change is 65%.
A later close of 132 gives −3.2%, with the original 21% still available at its
original horizon. The fixture also checks a missing pre-flow NAV, a cash amount
that differs from the booking, an unreconciled NAV split, a horizon before
booking, a second cash movement recorded in another currency, tenant and fund
denial, and byte-identical JetStream redelivery.

The benchmark fixture values the independently signed synthetic total-return
index from 250 to 270 over the same UTC window. Its 8% return gives +13
percentage points against 21%. The corrected portfolio close gives −11.2 points.
A benchmark-only correction to 275 gives 10% and −13.2 points, with portfolio
return remaining −3.2%. Missing and wrong-date editions withhold the comparison
and retain the portfolio return. The fixture checks foreign scope, altered
signatures, future knowledge and the enrolled index identity.

The position fixture uses two long holdings and a booked USD 3 fee. The first
edition has exact P&L of USD 24, USD 4 and USD −3, totaling USD 25 after the
external capital is excluded. Its linked contributions are +21.2, +1.8 and −2
percentage points. A closing-price restatement produces exact P&L of USD 4,
USD −9 and USD −3, with linked contributions of `98/1875`, `−241/3750` and
`−1/50` of portfolio return, totaling −3.2%. A missing price, unreconciled NAV
and any in-window trade under the unchanged-holdings method each withhold the
breakdown while preserving the measured portfolio return. The booked-trades
fixture then reports the sale before the USD 40 subscription and the purchase
after it. Signed NAVs are 100, 110, 150 and 166. Exact position P&L is
USD 24.8 and 6.2, execution costs are USD −2 and other cash P&L is USD −3.
The total is USD 26 and linked return is `163/750` (about 21.733333%).
A closing-price restatement to NAV 158 produces USD 18 and `119/750`.
Missing fill coverage, an omitted fill, a fill signed in the wrong
subperiod and a missing position price withhold the breakdown.

The first estate needs a governed issuer and complete source scope before
enrolling real keys. The synthetic fixture owns its signing key for one run.
