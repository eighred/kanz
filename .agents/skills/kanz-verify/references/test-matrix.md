# Test matrix

## Go gates

From `kanz/`, run build, vet, the relevant serial test slice, architecture tests,
lint and tracked-file formatting. For a full run, execute serial chunks rather
than one long `./...` process and prove package completeness against `go list`.

Postgres-backed tests silently skip when `TEST_POSTGRES_URL` is unset. Verify the
role is `NOSUPERUSER`; superusers bypass RLS. Never use a production DSN.

The in-memory `fakeBus` does not validate real broker envelopes. A green fakeBus
test is not NATS proof. External integrations use disposable demo/testnet
credentials supplied for one run and rotated afterward.

## Evidence semantics

- A merged change that has not run in its target environment is
  `needs-verification`, not complete.
- A CI job with zero steps and only seconds of runtime did not test the code.
- A concurrency claim remains unproven until `-race` runs on a supported CGO
  host.
- A guard that was never observed failing under a representative mutation has
  not proved its enforcement boundary.
