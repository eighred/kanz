# EVT-21b — cross-language serialization fixtures

This directory holds the generator output consumed by all three EVT-21b
parity test suites:

- Go:    `kanz/test/contract/serialization/parity_test.go`
- Python: `kanz-py/tests/test_serialization_parity.py`
- TS:    `kanz/ts/kanz-bus/test/serialization-parity.test.ts`

## Layout

- `<name>.bin` — raw `envelope.v1.EventFrame` wire bytes for one fixture
- `manifest.json` — language-agnostic, snake_case field assertions

The fixture set lives in `../fixtures.go` (`Fixtures()`). Each entry covers
a corner case the wire format must round-trip identically across
languages — every event class, lineage populated, no-partition zero
sequence, and a repeated `QualityFlag` enum to assert repeated-enum wire
encoding parity.

## Regenerate

```
go run ./test/contract/serialization/cmd/genfixtures \
    -out ./test/contract/serialization/fixtures
```

CI runs this before invoking any of the three language test suites. The
Go parity test fails loudly with this exact command if committed
fixtures drift from `BuildAll()`.

## Why Go is the canonical generator

The Envelope is the constitution and the Go bus client (EVT-17) is the
reference implementation. Building fixtures from Go envelopes guarantees
the fixtures track the live Go bindings; any drift in `envelope.proto`
surfaces in the Go test before Python or TS load the file.
