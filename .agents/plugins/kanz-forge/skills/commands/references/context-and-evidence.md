# Spend context on decisions and proof

Use this when a task spans several tools, workers, or long test/CI output.
There is no compression proxy. Reduce irrelevant input at the source; do not
rewrite exact code, errors, identifiers, units or security constraints into an
abbreviated dialect.

## Retrieve, then expand only on an unresolved question

1. Identify the task's decision and revision. Start with the owning definition,
   direct callers, nearest tests and applicable instructions.
2. Use file lists and scoped symbol searches before opening files. Use language
   references or package metadata to resolve candidates; a text match alone is
   not an edge. Expand when wiring, ownership or a failure remains unexplained.
3. Reuse an inspected source only while its revision and relevant working bytes
   remain unchanged. Re-read after a push, edit, checkout change or conflicting
   evidence. Do not rerun a broad search simply to recreate a worker's answer.
4. Ask workers for the decision, source locations, assumptions, failed checks and
   unresolved edges. Retrieve their raw evidence when it affects integration or
   conflicts with another result. A summary is a locator, not a replacement for
   the authority behind a capital or security claim.

## Project noisy results without losing failures

| Input | Keep in the working context | Expand when |
|---|---|---|
| GitHub checks | PR/head SHA, expected jobs, state/conclusion, run URL | A job fails, is missing, cancels, or skips unexpectedly |
| Go JSON tests | Command/exit, expected and completed packages, failing and skipped test IDs, decisive output | Counts disagree, output truncates, a dependency skips, or a failure is unexplained |
| Source search | Definition/caller locations and the lines that establish the relation | Registration, tags, generated code or dispatch can hide consumers |
| Dependency alerts | Exact advisory, resolved version, affected entrypoint, fix range | Reachability or resolution is uncertain |

Inspect completion and failure fields before summarizing. Never use the last
few lines, zero `FAIL` matches, a success-only filter, or a tool output token
limit as evidence that the entire run passed. Query all relevant pages and
expected jobs; report incomplete access as unknown, not an empty result.
Retain the CI URL or original result location so the decisive evidence can be
reopened. Do not persist ambient environment, credentials or entire tool dumps.

## Coordinate without repeated human prompting

Maintain only the current acceptance, owners, dependencies, revision, completed
checks and next unresolved gate in the Chat. Reuse existing authorization within
its scope. Choose routine details from code and tests; ask for a human decision
only when it changes behavior, authority or acceptance and cannot be resolved
from the available evidence. Ask missing-requirement questions early and continue
independent work; do not guess the requirement for dependent work. For an approval
gate, finish the already-authorized preparation so the user can review the exact
action before approving it.

After a sufficient check passes, move to the next unmet gate. Repeat a test only
when its inputs changed, it failed, or new evidence exposes a gap. While CI runs,
follow the [parallel protocol](parallel-work.md); no available independent work
means waiting is appropriate. Updates carry a finding, decision or changed gate,
not a replay of every tool call. Report real measured input/output savings only
with a comparable baseline and unchanged acceptance; make no fixed percentage
claim from concise wording alone.
