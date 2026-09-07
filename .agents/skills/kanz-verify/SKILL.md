---
name: kanz-verify
description: Verify a Kanz change, deletion, test claim, CI result, or issue completion without false green; use before saying work in eighred/kanz is fixed or done.
---

# Kanz verification

Verify the changed risk boundary, then the smallest relevant surrounding gates.
Do not repeat broad suites unless later changes or failures invalidate them.

## Required checks

- Capture the exit code of the command under test, not a formatter or pipeline.
- For serial Go suites, compare reported packages with `go list` so a truncated
  Windows run cannot look green.
- State whether `TEST_POSTGRES_URL` was set and whether its role was
  `NOSUPERUSER`.
- State whether race detection actually ran on a supported CGO host.
- Inspect CI job steps and duration; a job that dies before executing steps is
  infrastructure evidence, not code evidence.
- Mutation-prove new architecture guards and restore from a private backup,
  never with a command that discards unrelated working-tree edits.
- After deleting anything, search for live references and verify the intended
  replacement.

Read [references/test-matrix.md](references/test-matrix.md) for full repository
or integration verification. On Windows, first read
[references/windows.md](references/windows.md).
