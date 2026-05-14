# Envelope Versioning & Field-Inclusion Policy

Governs the `Envelope` message (`proto/envelope/v1/envelope.proto`) and the
`envelope_version` field. Payload schema evolution is governed separately by
`docs/schema-evolution.md` (EVT-06).

## 1. The constitution principle

The `Envelope` is the universal wrapper on every event crossing the Kanz
backbone. Every producer stamps it; every consumer, every piece of generic
infrastructure tooling, and every replay depends on it. It cannot be migrated
cheaply once events carrying it exist in the durable log.

Therefore the Envelope is treated as a constitution:

- **Additive-only, forever.** No field is ever removed, renumbered, retyped, or
  given new semantics.
- **Breaking changes are structurally impossible** within the `Envelope`
  message. There is no process for a breaking envelope change because there is
  no acceptable one — see §6.
- Changes are rare, deliberate, and require architecture-level review (§5).

## 2. Field-inclusion bar

A field may be added to the Envelope only if it passes **at least one** of
these two tests:

- **Universality** — every event class (FACT, COMMAND, STATE_SNAPSHOT,
  OBSERVATION), in every domain, meaningfully carries it.
- **Payload-blind tooling** — generic infrastructure (schema registry, data
  lake, observability, replay tooling, routing) needs it to do its job
  *without deserializing the payload*.

If a field passes neither test, it belongs in a payload schema, not the
Envelope.

## 3. What does not belong in the Envelope

Reject a proposed field if it is any of:

- **Family-specific** — meaningful only to one event family (e.g. a price, a
  model version, a portfolio id). That is payload.
- **Derivable** — computable from other envelope fields or the payload.
- **Consumer state** — a consumer's processing status, offsets, or
  acknowledgements. The Envelope describes the event, not its handling.
- **Mutable** — anything that would need to change after publish. Envelope
  fields are immutable once stamped.

## 4. Field numbering discipline

- Field numbers are **never reused and never renumbered.**
- A retired field's number *and* name are moved to a `reserved` declaration,
  permanently.
- Numbers 1–15 (single-byte wire tags) are fully allocated by envelope v1. New
  fields take 2-byte tags; this is acceptable given hot-path events are batched
  (one Envelope amortized over N payloads).
- Protobuf's reserved range 19000–19999 is never used.

## 5. Change process

Every change to `envelope.proto` requires:

1. A written proposal stating which §2 test the field passes and why §3 does
   not exclude it.
2. Approval from `@kanz-eng/architecture` (enforced by CODEOWNERS on
   `/proto/envelope/`).
3. A passing `buf breaking` check (EVT-07) — which mechanically blocks any
   non-additive change.
4. An `envelope_version` increment in the same change (§6).

## 6. `envelope_version` semantics

`envelope_version` is a monotonic `uint32` carried in every Envelope.

- It is incremented by exactly 1 each time one or more fields are added to the
  Envelope.
- It records the highest Envelope schema version the producer was built
  against. A consumer reading an Envelope with a version higher than it knows
  simply ignores the unknown fields (proto3 forward compatibility) — a higher
  version is *always* safe to receive.
- A bump **never** signals a breaking change. Because the Envelope is
  additive-only, every version N is wire- and semantically-compatible with
  every version < N.
- It is **not** the payload version. Payload versioning is `schema_version`,
  governed by `docs/schema-evolution.md`.

If a breaking envelope change were ever genuinely unavoidable, it would not be
an `envelope_version` bump — it would require a new `Envelope` message in a new
package version (`envelope.v2`), a dual-write migration across every producer,
every consumer, and the entire durable log, measured in quarters. This is
documented only to be explicit that it is a last resort with no streamlined
path. Design to never need it.

## 7. Presence and value invariants

proto3 cannot mark fields required. The Envelope schema therefore does not
enforce presence — the **shared bus client library** (EVT-17/18/19) validates
every Envelope on the publish path and rejects malformed ones before they reach
the backbone.

Per-field presence requirements (required vs. may-be-empty) are documented
inline in `envelope.proto` and are the authoritative spec the client library
implements.
