# Custody account evidence import

A venue identity issuer attests the binding between a local account, the venue and the exchange’s identifier for a stated business-date interval. A separately authorized governance issuer assigns that exact proof and account to a portfolio and custodian for a bounded interval. Both intervals use an inclusive start and exclusive end in UTC. An observation covering a single day supports that day; a longer interval requires independent evidence for the full interval.

The accounting database administrator provisions each login, public Ed25519 key and its allowed scope rows before import. Each login belongs to one tenant and one issuer kind. Each venue scope names the permitted local account, venue and exchange identifier. Each assignment scope names the local account, portfolio and custodian. The two issuer roles need SELECT on their authorized registry/scope rows and SELECT/INSERT for their own evidence table. Grant the accounting reader SELECT only and withhold authority administration, opposite-kind writes, mutation and table ownership from both issuers. Apply accounting migration 0030 before importing.

Run this command as the enrolled issuer using its own database credential and explicit `ACCOUNTING_TENANT`. The existing `ACCOUNTING_DATABASE_URL_FILE` secret mount or a process-scoped `ACCOUNTING_DATABASE_URL` supplies the connection. Run the venue import first, then the assignment import under its separate login.

```sh
custody_proof -kind venue_identity -evidence venue_identity.json -signature venue_identity.sig
custody_proof -kind custody_assignment -evidence custody_assignment.json -signature custody_assignment.sig
```

The signature file contains the 64 raw Ed25519 signature bytes over the entire JSON file. Exact duplicate imports succeed. A changed document requires a distinct immutable identifier and an approved assignment revision. The stored bytes, projected identifiers, dates and signature can be checked when the comparison is read. Keep private signing keys with their respective issuers.

The public testdata files represent independent synthetic issuers for `synthetic-custody-pilot` and the bounded interval 2020-02-04 through 2029-12-31. Production onboarding requires the genuine venue-identity issuer and custody-governance owner to attest the data. An exchange adapter’s authenticated identity observation can support a venue issuer’s decision. Its existence alone establishes neither a past date range nor the custodian assignment.
