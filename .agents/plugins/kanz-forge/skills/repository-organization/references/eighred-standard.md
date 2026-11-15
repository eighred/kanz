# Eighred repository organization standard

Eighred is the company. Kanz is its institutional investment operating system.
Kanz Forge supplies engineering workflows and coordination for AI agents and
developers. Repository organization must make financial ownership, execution
boundaries and verification visible as the product grows.

This is the shared developer and Forge placement standard. The repository's
AGENTS.md owns platform invariants; component source and manifests establish
what exists. Decisions and unfinished migrations belong in GitHub Issues and
PRs. A directory or README does not confer production authority.

## Organize by ownership before technology

Choose the existing domain owner before choosing a path. A component needs a
clear responsibility, callers, typed contracts and evidence of how it is built
and operated. Execution, admission control and intelligence retain their
boundaries across languages. A shared folder is not a new architectural plane.

Group independently built runtimes at the repository root and domain owners
inside each runtime. Do not create a deployment per blueprint capability.
Split a component when ownership, release cadence, trust or scaling requires
it; a different language or a large file alone does not establish that need.

| Kanz location | Responsibility and limits |
|---|---|
| `kanz/` | Existing Go module and its application owners. Keep this established module/build anchor; its name does not imply that other runtimes are outside Kanz. |
| `kanz/services/<owner>/` | Domain service, private implementation, migrations and operational runbooks. Deployment ownership must be explicit. |
| `kanz/cmd/` | Operator and administrative executable entrypoints. Preserve released command names. |
| `kanz/internal/` | Go code shared inside the module after a second consumer establishes common ownership. |
| `kanz/pkg/` | Deliberately reusable Go APIs with an identified consumer and compatibility responsibility. It is not a miscellaneous utilities directory. |
| `kanz/test/` | Cross-component, architecture and qualification tests. Unit tests stay beside their implementation. |
| `kanz/dev/`, `kanz/infra/` | Local smoke environment and deployment/operations respectively; neither is an alternative domain implementation. |
| `schemas/` | Canonical versioned wire contracts, compatibility rules and SDK generation. Generated output stays ignored. |
| `rust/` | Cargo workspace with reusable crates and explicit service/worker owners. A diagnostic worker cannot become an authoritative writer through placement. |
| `python/` | Python bus and inference packages. Intelligence cannot become an execution dependency. |
| `julia/` | Isolated calibration/research worker; its documented caller controls acceptance. |
| `web/` | Vue/TypeScript operator interface. Server authorization and exact financial values remain authoritative. |
| `tools/` | Repository/host tooling and disposable qualification fixtures spanning components. Component-local tools remain with that component. |
| `.github/` | GitHub discovery, governance and CI. Keep workflow files at the path GitHub requires. |
| `.agents/plugins/kanz-forge/` | One maintained Forge package: skills, profiles and deterministic development checks. |
| `.agents/skills/` | Thin repository-discovery adapters when the host needs them; no copied engineering workflow. |

Keep service migrations and runbooks with their owner. Schema `gen/`, Cargo
`target/`, frontend `dist/`, package caches and local qualification artifacts are
outputs, not source. Do not introduce root `src/`, `utils/`, `common/`, `managers/`
or generic `platform/` layers without a concrete owner and actual consumers.
Existing named platform libraries retain their specific responsibilities.

## Names remain consistent at every depth

Use short English lowercase directory names. A multiword project-owned name
uses `snake_case`, at the root and at every nested depth. Depth never switches
the convention. Prefer the responsibility (`web`, `schemas`, `risk_engine`) to
a redundant company/product prefix or an unexplained language abbreviation.
Avoid numbered phases, temporary names, vague `new`/`next`/`misc` buckets and
one-file layers that add no ownership boundary.

Language and discovery contracts take precedence:

- Go package names remain short lowercase identifiers, normally without
  underscores. A Go `internal/` boundary is compiler-enforced; use it at the
  narrowest useful scope. Services use thin `cmd/<source_name>/` entrypoints,
  with private configuration in `internal/config/`.
- Cargo, Python, Vue and TypeScript keep their native `src`, `crates`, `tests`
  and package layouts where used. Do not add Go's `cmd`/`pkg` template to them.
- `.github`, `.codex`, `.agents`, `.githooks`, `.codex-plugin`, plugin names and
  skill entrypoints preserve host discovery rules. Imported, generated and
  vendored trees preserve their upstream contracts. Register exact owned-tree
  exceptions in the naming guard with a reason; an entire subtree exemption
  must not conceal new project-owned directories.
- This convention governs directories. File names follow their owning tool
  (`Cargo.toml`, `README.md`, `_test.go`, Vue components, workflow YAML).
  Kubernetes/DNS names, released binaries, npm/Cargo/Python package names,
  Go module identities, Protobuf packages, subjects, database names and stored
  object keys are separate compatibility contracts. Do not transliterate them.

For example, the source `kanz/services/risk_engine/cmd/risk_engine/` still
builds the released `risk-engine` command and image. Scaffolding and release
recipes must preserve that distinction. A folder rename that silently changes
an executable called by the TUI is a regression.

## Sharing and dependency direction

Start private. Promote code only when a second actual consumer shares its
semantics, failure behavior and owner. Keep venue-specific behavior at the
adapter, domain concepts in their canonical owner, and wire shapes at typed
boundaries. A shared helper must not import a consuming service's `internal/`
package or pull intelligence into execution. Prefer explicit contracts to a
cross-language utility layer.

Public-library layouts such as `go-github` optimize a client API; compiler
repositories such as TypeScript optimize compiler/package ownership. Kanz is
a multi-runtime institutional system with independent operational and trust
boundaries. Reuse those projects' clear ownership and real entrypoints without
copying their directory trees.

## Documentation contract

Root README explains the product, architecture and entrypoints. AGENTS.md
contains stable engineering constraints and directory responsibilities.
Component READMEs explain their own purpose, caller/authority, prerequisites,
working directory, configuration, build/test/run commands and operational
limits. Keep an existing useful runbook intact rather than replacing it with
a generic template. State when a command requires a disposable cluster, database,
external account or operator approval; never embed credentials.

Link to shared rules instead of duplicating them. Commands must identify where
they run, use real targets and preserve missing/unsupported/unknown distinctions.
Separate source implementation from executed qualification and deployed behavior.
Remove obsolete completion tables and future task lists; link the owning Issue
for decisions and unfinished acceptance. Durable protocol specifications may
remain in their schema owner's README. Write documentation and names in English.

Review every tracked README in scope, including apparently unchanged ones.
Resolve local links against the document's directory and inspect actual
manifests/entrypoints before correcting usage. Generated and third-party
documentation is assessed under its provenance contract, not silently rewritten.

## A move changes a build contract

Before moving a component, inspect active branches and PRs, dirty files and
Windows case-folded path collisions. Recover useful existing work. Preserve
module/package, deployed, wire and stored-history identities unless a separately
accepted change explicitly owns them.

Trace and update literal and constructed path consumers together: module
replacements, SDK generation, Cargo members/dependency paths, Docker contexts
and COPY destinations, executable names, CI filters/matrices/caches, deployment
and migration paths, scripts, scaffolds, architecture guards and documentation.
Sibling modules can move only with verified local resolution and build contexts.
Review path edits separately from semantic changes.

Validate the complete effective source inventory, including new nonignored files
and excluding deletions. Run naming/link checks and affected generation/build/test
entrypoints. Prove a guard rejects a representative violation and detect dead
exceptions. Linux CI must cover native/browser/container consumers unavailable
on Windows. Record exact revision, package coverage, skips and blocked inputs in
the owning PR. A path-only diff and a clean string search do not prove all callers
still build. Preserve rollback through git and the previous reviewed package;
do not create compatibility copies that become a second implementation.

## Reference choices

The community [golang-standards/project-layout](https://github.com/golang-standards/project-layout)
explicitly disclaims official Go-standard status. Its selective `cmd`, `internal`,
`pkg`, `web`, tooling and operational groupings inform this standard; its optional
directories are not a required skeleton. The official
[Go module layout guidance](https://go.dev/doc/modules/layout) supports private
server implementation and multiple command entrypoints. Eighred applies those
principles within its existing Go module and keeps other language ecosystems native.

[go-github](https://github.com/google/go-github) and
[TypeScript](https://github.com/microsoft/TypeScript) are comparison examples for
ownership and developer entrypoints, not templates or Forge dependencies.
