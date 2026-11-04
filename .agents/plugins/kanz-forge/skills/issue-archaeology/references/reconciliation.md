# Tracker reconciliation criteria

For each candidate, inspect the issue's stated `Verified when`, blocker,
source, labels, decisive comments, linked PRs and merge commits. Match those
claims to current-main files, live callers, migrations and guard exemptions.
Use `git log -- <path>` to resolve changed architecture. Search existing issues
before filing a successor.

An issue remains open when code is partial, a production dependency was only
mocked, a required human decision is absent, or a guard exemption still admits
the defect. A merged PR is code evidence; close only after the acceptance
conditions of that scoped issue are actually met. Preserve an intentionally
deferred decision as `decision-needed` or `blocked-external`, with the exact
trigger to revisit it.

| Evidence found | Disposition |
|---|---|
| Code exists only on a branch | Implemented on that branch; still open against main |
| Merged code, acceptance depends on unrun deployment/data repair | Needs verification, with exact estate/input and runnable expected result |
| Test is green but skips the dependency or exempts the defect | That criterion is unverified; name the skipped boundary or live exemption |
| New requirement replaces old behavior | Superseded only with an attributable decision and a current owner for any surviving criterion |
| Multiple issues describe the same repair | Choose one owner after comparing acceptance; transfer unique criteria before closing duplicates |
| Evidence comes from another checkout/version | Reconcile revisions, dirty inputs and installation source before attributing the result to current main |
| No permission/access to decisive evidence | Record the missing observation; do not infer absence or create a credential workaround |

Before mutating a tracker item, re-read its current state if other work could
have changed it. Preserve human comments and unrelated acceptance. Use a small
evidence-focused edit rather than overwriting the body from an old snapshot.

Every open issue has one kind, one P0/P1/P2 priority and one area. A useful body
contains **Evidence**, **Verified when**, **Blocked by** and **Source**. For a
duplicate, transfer unique criteria, comment on both issues, and close the
duplicate as superseded. For a mixed issue, make completed and remaining scopes
distinct. A closed issue cited by a live exception is a prompt to inspect the
exception and either retire it or give its remaining work a live owner.

Do not paste credentials, unbounded history, or a CSV inventory into issues.
The tracker and PRs are the durable record; `AGENTS.md` holds stable rules.
