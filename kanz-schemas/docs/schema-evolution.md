# Payload Schema Evolution Rules

Governs every **payload** schema under `proto/{domain}/v1/` and the envelope
fields that version it — `schema_version` and `payload_schema_ref`. Envelope
evolution is governed separately by `docs/envelope-policy.md` (EVT-03); the
envelope is a constitution, payloads are not.

## 1. The core asymmetry

The Envelope is additive-only forever because breaking it is structurally
impossible to migrate. Payload schemas are allowed to break — but a break is
still a one-way door:

- Kafka is the durable log. Events carrying a payload schema outlive the code
  that produced them, for the full retention window of their topic.
- Replay (EVT-20) reads historical events with **current** code.
- Producers and consumers run mixed versions simultaneously during any rollout.

So payload evolution has two modes, and the whole of this doc is about telling
them apart: **non-breaking** changes edit a schema in place; **breaking**
changes cut a new package version and require a dual-write migration.

## 2. Compatibility direction

Because the log is durable and rollouts are mixed-version, every payload schema
must hold **full compatibility** — backward *and* forward — across a single
package version:

- **Backward** — current code reads events written by any older v1 producer.
  Non-negotiable for the entire Kafka retention window, because replay depends
  on it.
- **Forward** — older code reads events written by a newer v1 producer. proto3
  gives this for unknown fields (they are preserved, not dropped); the rule
  exists so we never *rely* on a consumer seeing a new field.

Full compatibility is what makes a non-breaking change safe to ship without
coordinating producers and consumers. A change that cannot preserve it is
breaking, by definition — see §5.

## 3. Field-number discipline

Identical discipline to the Envelope (`envelope-policy.md` §4), restated because
it applies to every payload message and enum:

- Field numbers are **never reused and never renumbered.**
- A retired field's number *and* name move to a `reserved` declaration,
  permanently. Same for a retired enum value's number and name.
- New fields take the next free number. Numbers 1–15 (single-byte wire tags)
  should be spent on the highest-frequency fields of hot-path messages.
- The protobuf reserved range 19000–19999 is never used.
- Every enum has an `*_UNSPECIFIED = 0` zero value. Consumers MUST treat an
  unknown enum value as `UNSPECIFIED` — this is what makes §4's "add an enum
  value" non-breaking.

## 4. Non-breaking changes — edit in place, no `schema_version` bump

These preserve full compatibility (§2). `buf breaking` (EVT-07) passes; the
schema is edited in place in `proto/{domain}/v1/`; `schema_version` does **not**
change. Only the registry version in `payload_schema_ref` advances (§6).

- Adding a new field whose zero value is a valid, safe default.
- Adding a new value to an existing enum (consumers already handle unknowns).
- Adding a new message type, or a new field to a nested message under these
  same rules.
- Loosening a value invariant (a previously-rejected value is now accepted).
- Documentation-only changes to comments.

## 5. Breaking changes — new package version + dual-write

A breaking change is **anything `buf breaking` rejects**, plus the semantic
breaks it cannot see:

- Removing, renumbering, or retyping a field. *(buf catches these.)*
- Changing a field's meaning or units — same wire type, different semantics.
- Tightening a value invariant — a previously-valid value is now rejected.
- Promoting an effectively-optional field to effectively-required.

The last three are wire-compatible, so `buf breaking` passes them. They are
caught **only by CODEOWNERS review** — reviewers of `proto/{domain}/` must
check this list, not just trust CI.

Process for a breaking change:

1. The affected message is re-declared in a new package version directory,
   `proto/{domain}/v2/` (`package {domain}.v2`). The `v1` schema is left
   untouched. Unchanged sibling messages are **not** force-migrated.
2. Producers **dual-write** v1 and v2 for the duration of the migration.
3. Consumers migrate to v2 on their own schedule (the durable log lets them).
4. v1 is retired — its topic and `reserved` tombstone kept — only once the
   Kafka retention window has rolled past the last v1 event.

## 6. `schema_version` vs `payload_schema_ref`

The envelope carries two payload-versioning fields; they are not redundant.

- **`schema_version`** (`uint32`) — the **major** payload version. It equals
  the `N` of the message's `{domain}.vN` package. It increments by exactly 1
  on a breaking change (§5) and at no other time. It is per-event-type:
  `market.equity.trade` and `risk.portfolio.exposure_recomputed` advance
  independently. It is the field a consumer branches on to pick a decoder.
- **`payload_schema_ref`** (`string`, `<schema-id>:<version>`) — the
  fine-grained registry pointer (EVT-16). Its version advances on **every**
  schema change, breaking or not, so generic tooling and replay can pin the
  exact schema an event was written against. A non-breaking §4 change moves
  this and nothing else.

A consumer that only needs "can I decode this" reads `schema_version`. A
consumer or tool that needs "exactly which schema" resolves `payload_schema_ref`
against the registry.

## 7. Version-bump triggers — summary

| Change | `buf breaking` | New `vN` package | `schema_version` | `payload_schema_ref` |
|---|---|---|---|---|
| Add field (safe default) | passes | no | unchanged | bump |
| Add enum value | passes | no | unchanged | bump |
| Add message type | passes | no | unchanged | bump |
| Loosen invariant | passes | no | unchanged | bump |
| Doc-only change | passes | no | unchanged | bump |
| Remove / renumber / retype field | **fails** | yes | +1 | new |
| Change semantics or units | passes* | yes | +1 | new |
| Tighten invariant | passes* | yes | +1 | new |
| Optional → required | passes* | yes | +1 | new |

\* wire-compatible but semantically breaking — invisible to `buf breaking`,
caught only by CODEOWNERS review (§5).

## 8. Enforcement

- **`buf breaking`** in CI (EVT-07) mechanically blocks every wire-breaking
  change to a `v1` package against its committed baseline.
- **CODEOWNERS review** on `proto/{domain}/` is the only gate for semantic
  breaks that pass `buf breaking` (§5). Reviewers own that the §7 table was
  applied honestly.
- **The schema registry** (EVT-16) is the runtime source of truth.
  `payload_schema_ref` must resolve there before an event is publishable.
- **Replay determinism** (EVT-21d) depends on §2 holding: replaying an old log
  with current code must produce identical output, which is only true if every
  intervening change was genuinely non-breaking or genuinely a new `vN`.
