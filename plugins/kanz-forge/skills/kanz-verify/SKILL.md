---
name: kanz-verify
description: Verify a Kanz fix, deletion, test claim, CI result, or issue completion without false green in eighred/kanz.
---

# Kanz verification

This reconstructs the verification role preserved in commit `15fd2c46`.
Follow the checkout's `AGENTS.md` and actual CI workflow when they differ from
this skill. Prove the changed risk boundary, then the smallest surrounding
gates needed to support the claim.

- Capture the tested commit, command exit code, and package list. Compare serial
  Go test output with `go list`; a truncated Windows run can exit without a
  visible `FAIL` while missing packages.
- State which dependencies actually ran. Postgres tests can silently skip
  without `TEST_POSTGRES_URL`, and a superuser can bypass RLS. A fake bus does
  not prove JetStream envelope acceptance.
- Confirm whether race detection ran with CGO on a supported host. Inspect CI
  job steps, skips, and duration before treating its result as proof.
- For an architecture guard, identify the syntax or behavior it checks and
  deliberately violate a representative artifact when practical. A passing
  guard proves only its tested boundary.
- After deleting or replacing a path, search for live callers, configuration,
  and operator workflows. Preserve unrelated working-tree changes.
- Separate merged code from verified production behavior. Close an Issue only
  when its acceptance conditions and the merged revision have been checked.

Use only disposable test dependencies and demo/testnet external credentials;
never persist credentials or point verification at a production DSN.
