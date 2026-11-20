# Disposable operator GitOps verification

The fixture reads the tracked operator component, directory settings, AppProject
and complete identity manifests. It creates a fresh owned Kind/Cilium cluster
through the existing `feed_identity` constructor, installs the same digest-pinned
Argo CD Core used by native broker qualification, and serves two small immutable
Git commits inside that cluster. It never adopts an existing kubeconfig or
contacts a deployed estate.

The Application is a concrete rendering of the tracked primary/operator matrix
entry. The fixture runs the actual Argo Application controller, rather than an
ApplicationSet controller. Only the repository URL and target revision change to
the local fixture source. The initial fixture commit keeps ordinary YAML and JSON manifests byte-for-byte.
The source check refuses a controller skip directive on an identity or a dynamic
Jsonnet manifest. A single test annotation on `kanz-model` supplies a reversible second revision. The
excluded manual Job is suspended in both fixture commits as an independent
brake against accidental execution. No NATS, SPIRE server, halt process, order
or resume operation runs.

The required observations are initial sync, deletion/self-heal of one actual
ServiceAccount, same-revision resync, revision change and rollback. Every phase
requires the six real ServiceAccounts, six exact egress policies, SPIRE namespace
label and complete Argo resource inventory, with zero Jobs. Rollback must remove
the second revision's annotation. The rollback changes only the desired Git
revision. Argo must record an automated operation and restore the entire
inventory before the fixture requests an explicit resync. That separate
operation carries a fresh `operation.info` token and repeats the readback.
Both rollback observations share one 120-second deadline. Subprocess
diagnostics are byte bounded and are not emitted.

CI prerequisites reuse `bash tools/feed_identity/install.sh`, Docker and
PyYAML 6.0.3. Run the source/readback refusal controls before the actual proof:

```sh
python3 -B -m unittest discover -s tools/operator_gitops_proof -p 'test_*.py'
python3 -B tools/operator_gitops_proof/proof.py
```

The actual proof refuses execution outside its Linux GitHub Actions job. It
prints a closed exact-source/case/cleanup result to stdout. The separate
`RUNNER_TEMP/operator-gitops-owner.json` is issued only at the actual owned Kind
create-command edge, after the cluster name is confirmed absent. A pre-existing
name is refused without an owner file or delete. The schema-1 record contains
`cluster`, `source`, `runID`, `runAttempt`, a 32-hex `creationID` whose first twelve
characters form the cluster suffix, and `creationAuthorized: true`. Both normal
and interrupted-run cleanup must call `validate_creation_authorization` with
the current workflow source/run/attempt before deleting that single cluster.
Normal cleanup additionally requires the exact in-memory creation record and
consumes the file after confirmed deletion. An ambiguous create failure retains
the same authorization for cleanup; an absent-name preflight alone issues none.
No broker
execution-result receipt or raw command output is written.

Passing this disposable proof does not show that a deployed Argo instance has
loaded the revision, that SPIRE has issued a live SVID or that an operator has
authority to halt/resume an estate. Those remain explicit operational evidence.
