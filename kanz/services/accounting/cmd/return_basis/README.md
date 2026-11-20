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

For the isolated acceptance, generate the Go schema SDK and install the
locked Web dependencies and Chromium. From the repository root run
`python -B kanz/dev/daily_return_pilot.py`. It applies the Accounting
migrations to a disposable PostgreSQL container, builds the normal importer
and Accounting service and exercises a real JetStream consumer and authenticated
gateway/BFF in Chromium.

The fixture books a USD 40 subscription and values the PostgreSQL book through
Accounting using synthetic prices. With opening NAV 100, pre-flow NAV 110,
post-flow NAV 150 and close NAV 165, the linked return is
`(110/100) × (165/150) − 1 = 21%`. The close-to-close NAV change is 65%.
A later close of 132 gives −3.2%, with the original 21% still available at its
original horizon. The fixture also checks a missing pre-flow NAV, a cash amount
that differs from the booking, an unreconciled NAV split, a horizon before
booking, a second cash movement recorded in another currency, tenant and fund
denial, and byte-identical JetStream redelivery.

The first estate needs a governed issuer and complete source scope before
enrolling real keys. The synthetic fixture owns its signing key for one run.
