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

The six elapsed-duration tests use Jiff's signed/unsigned parsers and formatter. They cover fixed-unit and calendar semantics, the full unsigned range and the raw parser's truncation of `0.0001us` to zero. A narrow qualification function preserves bare zero and requires exact precision for the tested fractional forms. Full legacy parser parity and integration into the upstream consumers remain open. [Published Jiff package](https://crates.io/crates/jiff/1.0.1) Â· [object_store compatibility examples](https://github.com/apache/arrow-rs-object-store/pull/908)

## DataFusion 54 lake worker

The production builder restores and verifies each reviewed Apache tree before
compiling the lake library into a clean release target. It seals the paths and
SHA-256 hashes of the reviewed Cargo archives. The final worker build reuses
those exact archives and recompiles the executable with its version. The
exported library and binary compiler logs bind each package identity, feature
set, release profile and artifact path across the two steps.

```sh
python -B -m unittest discover -s tools/jiff_qualification -p test_chrono_advisories.py
python -B tools/jiff_qualification/object_store.py --advisories chrono-advisories.json
python -B tools/jiff_qualification/chrono_advisories.py --replay chrono-advisories.json
```

The qualifier uses the committed Rust workspace, the same Cargo.lock and the same Apache source preparation as the normal lake build. It verifies the original archive, source tree, delta and resulting tree for each of 32 patched Apache packages. Jiff owns object modification instants, conditional request times, elapsed durations and the Arrow named-zone implementation. The published sad-chrono 0.5.1 registry package supplies the residual Chrono APIs shared across both Arrow versions. Cargo's dependency aliases preserve the published package's real name and checksum. [Production source recipe](../../rust/temporal_sources/manifest.json) · [Published bridge](https://crates.io/crates/sad-chrono/0.5.1) · [Publisher source](https://github.com/sadco-io/sad-chrono/tree/14e7c8e1501e708496798b69e44b3e8bf82cc6c9)

The offline phase runs object_store's cloud-provider, HTTP duration and non-root filesystem tests. The five lake worker process cases exercise the real conversion and query paths. The workspace's object metadata test transfers signed filesystem nanoseconds into DataFusion PartitionedFile and its cache, preserves year 2200 and refuses an unrepresentable filesystem instant. The named-zone UDF test covers IANA aliases, numeric offsets, civil-time gaps and folds, and future seasonal changes. A cross-ABI test round-trips both signed nanosecond endpoints, epoch and the adjacent instants through the same Rust type. Compiler records must identify both Arrow versions, all prepared Apache sources and the genuine published temporal packages. [Apache object_store release](https://github.com/apache/arrow-rs-object-store/commit/7a65b75b0d26fd8a282999462cb7030fb85fdcc3) · [Upstream public timestamp change](https://github.com/apache/arrow-rs-object-store/commit/43c51126e2b368c5b26d7a686178a86f8c2ce787) · [Arrow timezone provider](https://github.com/apache/arrow-rs/pull/11482)

The retained snapshot binds the registry archive to the publisher's verified annotated tag and signed commit. The qualifier compares all 53 packaged repository files, including the original manifest and licenses, against the tagged Git tree. Cargo's three generated files are covered by the pinned archive checksum. A moved tag, a missing runtime source or a changed blob requires a new review.

The separate advisory check verifies the registry identity, ownership and yank status, the publisher's security policy and disclosure feed, and reviewed, unreviewed and malware records for the fork and upstream Chrono. Historical affected versions must detect the known publisher findings. New shared upstream RustSec records and altered local-time fix or maintenance records require review. CI saves the input for independent replay. The protected runtime-image verifier and strict workspace and extracted-binary audits provide release acceptance. [Publisher advisories](https://github.com/sadco-io/sad-chrono/security/advisories) · [Publisher security policy](https://github.com/sadco-io/sad-chrono/security/policy) · [RustSec submission](https://github.com/RustSec/advisory-db/pull/3613) · [Shared upstream records](https://github.com/RustSec/advisory-db/tree/main/crates/chrono)
