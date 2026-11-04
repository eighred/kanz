# GitHub governance and delivery

The Eighred **enterprise account** owns billing and enterprise policy. Its
Eighred **organization** owns `eighred/kanz`, membership and repository access.
These are complementary GitHub layers, not duplicate repositories. Changing
the subscription does not require moving Kanz or changing its service planes.

## Change and release authority

Use a PR into `main`. The repository ruleset requires current-base checks and
resolved review threads. `kanz-ci-required` aggregates every application job:
scope comes from complete Git history, unknown paths run the suite, and missing,
failed, cancelled or unexpectedly skipped results reject the gate. Known
non-application changes receive an explicit successful scope result. Existing
secret, CodeQL and Terraform checks remain separate required checks.

`.github/CODEOWNERS` routes review to the actual custodian. It cannot provide
independent approval while there is only one maintainer. Add real owners/teams
and then enforce independent review when a second authorized person joins;
do not create placeholder teams or require self-approval.

PR image validation has read-only repository/package permissions. A separate
canonical-main push job publishes the same build definition with registry and
OIDC permissions. No deployment environment is added implicitly: changing the
OIDC subject requires coordinated review of the AWS trust policy.

Release tags use `vMAJOR.MINOR.PATCH` (optional SemVer prerelease/build suffix).
Before creating one, verify the selected main commit's applicable CI. Release
source validation, for tags containing the new gate, requires the event, checkout and peeled tag to identify the
same commit, reachable from current main. This ancestry check alone does not
prove that CI passed. Tag creation is restricted to repository administrators;
a separate rule prevents updating or deleting existing release tags without
an ordinary bypass. An administrator can still change repository policy: record
exceptional recovery decisions in an Issue before doing so. The tagged workflow
cannot protect itself against an authorized creator selecting obsolete code.
Historical tags also retain their own manually dispatchable workflows; new
source checks do not retrofit those revisions. Retiring that signing authority
requires an admission/trust migration that preserves legitimate deployed images.
That decision and its verification are tracked in
[#1406](https://github.com/eighred/kanz/issues/1406).

## Inspect effective controls

The live GitHub settings are authoritative. Do not infer activation from this
guide or a passing local test. Use repository-admin access to inspect:

```sh
gh api repos/eighred/kanz/rulesets
gh api repos/eighred/kanz/rules/branches/main
gh api repos/eighred/kanz/actions/permissions
gh api repos/eighred/kanz/actions/permissions/workflow
gh api repos/eighred/kanz/codeowners/errors
gh pr checks <number> --repo eighred/kanz
```

Read individual rulesets for bypass actors and tag rules. Required contexts
should come from the GitHub Actions integration. Keep Actions enabled with
full-commit action pinning, default read-only workflow tokens, and automated
PR approval disabled. Require a new check only after GitHub has run it
successfully; preserve existing contexts when updating a ruleset. Normal main
changes must go through checks, including changes made by administrators.

For CI policy development:

```sh
python -B -m unittest discover -s .github/scripts -p test_required_ci.py
python -B -m unittest discover -s .github/scripts -p release_source_test.py
cd kanz
go test -mod=readonly -p 1 ./test/arch -count=1
```

Python here is a standard-library CI helper, not a Kanz Forge runtime. The
scope tests exercise real disposable Git histories; architecture tests check
workflow authority and shared build wiring. Ubuntu CI supplies platform-specific
coverage unavailable on Windows. No release or deployment should be triggered
merely to test repository policy.

## Account and production boundaries

Retain the working Enterprise trial until the owner chooses a paid plan.
Confirm current billing and security-product entitlements before that transition;
trial availability is not a permanent license. SSO/SCIM require an actual identity
provider and a recovery plan. Organization-wide access changes affect repositories
outside Kanz and need their own scope. A second trusted owner is a continuity
decision, not an account an agent should invent.

Governance configuration is not production acceptance. Recovery proof, tenant
isolation, capital admission and controlled execution remain governed by their
component tests, runbooks and open Issues. Track implementation and live
verification in Issues/PRs, not a duplicated feature-status file.
