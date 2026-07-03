# Audit hash chain + WORM (AUDIT-01b)

Tamper-evidence for the audit log has two halves, and both are needed:

1. **Hash chain (this package).** Each record stores `hash = sha256(prev_hash ||
   canonical(record))`. Because every hash folds in its predecessor, altering,
   reordering, or deleting any record makes its hash — and every hash after it —
   no longer recompute. `Verify` is a linear walk that returns the first index
   that breaks. This *detects* tampering after the fact.

2. **WORM storage (the migration + object-lock).** The chain proves a rewrite
   happened, but only if the attacker couldn't also recompute the whole chain in
   place. Write-once storage prevents that silent rewrite:
   - Postgres: `0001_audit_log.sql` installs a `BEFORE UPDATE OR DELETE` trigger
     that raises — the table is insert-only even to the app role.
   - Object store (cold/export tier): S3 Object Lock / GCS retention in
     COMPLIANCE mode on the exported log, so retained objects cannot be deleted
     or overwritten even by an admin until retention elapses.

Detection (1) without prevention (2) is weak — an attacker with write access
rewrites the record *and* the chain. Prevention without detection is also weak —
WORM can be misconfigured. Together they are tamper-*evident*: a change is either
blocked or provable.

## Design notes

- **Payload-agnostic.** `chain` hashes a caller-supplied `[]byte` and never
  imports the audit package — so `audit` depends on `chain`, not the reverse.
  `audit.Record.Canonical()` is the deterministic serialization that gets hashed
  (sorted map keys, UTC RFC3339Nano times) so the hash is stable across runs and
  machines.
- **Genesis.** The first record chains from the constant `Genesis`, so the head
  of an empty log is well-defined and the first hash is reproducible.
- **Append serialization.** The store assigns `seq` + hashes under a single
  writer (a Postgres advisory lock per append), so concurrent appends cannot
  fork the chain.
