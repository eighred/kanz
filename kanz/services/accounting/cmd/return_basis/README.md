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
restricted source login.

```sh
go run ./services/accounting/cmd/return_basis -evidence close.json -signature close.sig
```
