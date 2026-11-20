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
a measured zero from an unassessed period. Enter the earlier knowledge time
to reopen the original published result after a correction.

For the isolated acceptance, generate the Go schema SDK and install the
locked Web dependencies and Chromium. From the repository root run
`python -B kanz/dev/daily_return_pilot.py`. It applies the Accounting
migrations to a disposable PostgreSQL container, builds the normal importer
and Accounting service and exercises a real JetStream consumer and authenticated
gateway/BFF in Chromium.

The first estate needs a governed issuer and complete source scope before
enrolling real keys. The synthetic fixture owns its signing key for one run.
