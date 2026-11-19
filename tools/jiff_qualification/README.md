# Jiff temporal qualification

This tool tests Jiff 1.0.1 for the proposed Arrow, DataFusion and object_store temporal migration. It compiles the published Jiff sources with Kanz's pinned Rust 1.90.0 compiler on Linux/amd64. The production Cargo workspace and the release audits have separate entrypoints. [Migration issue #1413](https://github.com/eighred/kanz/issues/1413)

## Run

Use CPython 3.14.4 and Docker. Pull the repository's Rust mirror using normal read access, then run from the repository root.

```sh
docker pull --platform linux/amd64 ghcr.io/eighred/base/rust@sha256:3914072ca0c3b8aad871db9169a651ccfce30cf58303e5d6f2db16d1d8a7e58f
python -B tools/jiff_qualification/run.py
```

The runner downloads four public inputs from fixed upstream locations and checks their exact sizes and SHA-256 digests before use. For an offline run, supply a directory with `jiff.crate` (1.0.1), `jiff-core.crate` (1.0.1), `jiff-tzdb.crate` (1.0.0) and `zoneinfo.zip` (Go 1.26.1). The runner applies the same checksum checks to local files.

```sh
python -B tools/jiff_qualification/run.py --inputs /path/to/inputs
```

The compiler receives a read-only directory of public inputs, no credentials or network, and a writable temporary filesystem. The timezone process additionally denies filesystem opens and socket creation before loading any zone. The workflow gives its registry credential only to the image pull step and deletes that step's private Docker profile before running the repository code.

## What the tests cover

| Jiff entrypoint | Input | Names | UTC offsets | Civil times |
| --- | --- | ---: | ---: | ---: |
| `TimeZone::tzif` | Go 1.26.1 IANA 2025c corpus | 598 | 112,238 | 84,497 |
| `TimeZone::get` | Jiff's embedded IANA 2026f corpus | 598 | 112,505 | 84,710 |

Python's standard-library zoneinfo computes the expected UTC offsets and possible instants for each civil time. The corpus includes both sides of stored transitions, future recurring changes and half-hour or whole-day discontinuities. A Casablanca assertion distinguishes the two data releases. Jiff's maximum timestamp is 9999-12-30T22:00:00.999999999Z. The worker must preserve valid Protobuf seconds and nanoseconds beyond that limit.

The six elapsed-duration tests use Jiff's signed/unsigned parsers and formatter. They cover fixed-unit and calendar semantics, the full unsigned range and the raw parser's truncation of `0.0001us` to zero. A narrow qualification function preserves bare zero and requires exact precision for the tested fractional forms. Full legacy parser parity and integration into the upstream consumers remain open. [Published Jiff package](https://crates.io/crates/jiff/1.0.1) · [object_store compatibility examples](https://github.com/apache/arrow-rs-object-store/pull/908)
