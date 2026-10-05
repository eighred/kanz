# Capital-state review

Use this only when an edit can change an order, cash, position, margin, ledger,
or permission to trade. Map these identities before implementation:

- tenant, portfolio, account, order, parent/child, venue client ID, fill ID;
- exact quantity, debit currency, fee currency, source timestamp and knowledge
  timestamp;
- control decision and mandate version permitting the transition;
- transaction/outbox boundary and replay key;
- ambiguous external outcome and the state retained while it is unresolved.

Check the transition under duplicate delivery, concurrent replicas, late facts,
partial fills, cancellation, restart, and an unknown input. Unknown safety data
must refuse; do not turn absent source coverage into zero. The typed OMS →
adapter transport and exchange-auth boundary are settled in `AGENTS.md`.

For #1316 specifically, pending cash commitments must be shared by ordinary
orders, scheduled parents, amendments and rebalances. CI can prove reservation
arbitration against real PostgreSQL and JetStream; it cannot approve a maximum
fee policy, create corporate-action coverage, authenticate historical cash, or
authorize removal of the admission halt. Read the current issue and draft PR
before changing that boundary; their evidence may have advanced since this
reference was written.
