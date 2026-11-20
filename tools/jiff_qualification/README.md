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

## DataFusion 54 lake worker

Run `python -B tools/jiff_qualification/object_store.py` with the pinned Linux compiler image available locally. It builds an object_store 0.13.2 candidate from the Apache release, the #908 duration parser and formatter, and the Jiff public timestamp migration. The backport retains the release's reqwest imports, credential signing and provider interfaces. It exposes `jiff::Timestamp` for object modification and conditional request times. Missing or unrepresentable Linux modification times return storage errors. The Azure CLI credential requires one known expiry instant in the system time zone. It refuses unknown zones, gaps and folds, and computes elapsed lifetime across local clock changes. The qualifier checks the complete object_store Git tree before executing its contents. [Apache release](https://github.com/apache/arrow-rs-object-store/commit/7a65b75b0d26fd8a282999462cb7030fb85fdcc3) · [Duration change](https://github.com/apache/arrow-rs-object-store/commit/67985f7e58ee8246d9f2771e9f52665f97171869) · [Public timestamp change](https://github.com/apache/arrow-rs-object-store/commit/43c51126e2b368c5b26d7a686178a86f8c2ce787)

DataFusion's public `PartitionedFile` constructors use the Jiff Unix epoch for metadata without an observed modification time. Its local test utility converts actual file metadata, and `DFHeapSize` accounts for the inline timestamp. A process-level test writes exact modification times to a temporary filesystem, reads them via object_store, passes them into DataFusion, and checks cache validity across a one-nanosecond change and a separate size change. It exercises pre-epoch, epoch, post-epoch and year-2200 instants and requires an error for a filesystem instant beyond Jiff's range. These object modification times are separate from the lake's canonical Protobuf event timestamps.

The same worker build selects the [Apache Arrow #11482 timezone provider](https://github.com/apache/arrow-rs/pull/11482) on Arrow-array 58.4.1. The qualifier checks the candidate module's size and SHA-256. It checks the source crate archives against the repository lock before adapting DataFusion 54.1.0's inherited Arrow feature and its named-zone `%Z` parser. The writer stays on the published Arrow-array 59.3.2. [DataFusion workspace dependency](https://github.com/apache/datafusion/blob/54.1.0/Cargo.toml#L87-L90) / [Released parser](https://github.com/apache/datafusion/blob/54.1.0/datafusion/functions/src/datetime/common.rs#L121-L141)

The online phase acquires dependencies in a disposable workspace. Offline tests exercise the upstream HTTP duration grammar and formatter, AWS/Azure/GCP/HTTP library tests, the non-root filesystem permission case, the actual lake worker's five process cases, and DataFusion's timestamp UDF across IANA aliases, rejected numeric offsets, transition gaps and folds, and year 2200 seasonal changes. The worker's filesystem configuration does not enable the object_store cloud formatter. The check verifies both Arrow sources, DataFusion, object_store and Jiff in the worker compiler records. It requires both `humantime` and `chrono-tz` to be absent from the changed worker lock and compiler outputs, and requires the remaining `chrono 0.4.45` target.

The candidate exercises the dependency and feature surface used by the finite worker. DataFusion's optional plan serializer, Substrait reader and other cloud consumers have separate wire and compatibility requirements. Both Arrow lines and DataFusion retain chrono in other APIs. Production selection requires maintained package inputs for those boundaries and passing workspace and shipped-binary audits. [Migration owner](https://github.com/eighred/kanz/issues/1413)

## Published Chrono API bridge

The retained snapshot binds the registry archive to the publisher's verified annotated tag and signed commit. The qualifier compares all 53 packaged repository files, including the original manifest and licenses, against the tagged Git tree. Cargo's three generated files are covered by the pinned archive checksum. A moved tag, a missing runtime source or a changed blob requires a new review.

```sh
python -B -m unittest discover -s tools/jiff_qualification -p test_chrono_advisories.py
python -B tools/jiff_qualification/object_store.py --published-bridge chrono-advisories.json
python -B tools/jiff_qualification/chrono_advisories.py --replay chrono-advisories.json
```

This candidate selects the published `sad-chrono 0.5.1` crate for APIs that still expose Chrono types. The registry archives identify the package by its real name and checksum. Eighteen Apache package manifests request that package using Cargo's dependency alias syntax. The Jiff time zone, elapsed duration and object timestamp changes remain active. The worker checks both Arrow ABI lines against the same published type and round-trips signed nanosecond endpoints. Its changed lock and compiler records must agree on the genuine registry package. The Apache source adaptations also cover the published DataFusion crates that inherit the Arrow timezone feature. [Published package](https://crates.io/crates/sad-chrono/0.5.1) · [Publisher source](https://github.com/sadco-io/sad-chrono/tree/14e7c8e1501e708496798b69e44b3e8bf82cc6c9)

The publisher's disclosure feed includes findings absent from the RustSec database. The qualifier saves the public advisory inputs, requires the five known findings to detect historical affected versions and evaluates the candidate against every published range. New global disclosures for the fork, unfamiliar ranges, changes to the publisher's identity or security policy, and changes to the upstream Chrono advisory records require review. Shared code review covers the upstream local-time vulnerability fixed in 0.4.20 and the upstream maintenance notice. CI retains the snapshot for independent replay. Promotion also requires maintained Apache patch sources, provenance in the shipped inventory and both strict release audits. [Publisher advisories](https://github.com/sadco-io/sad-chrono/security/advisories) · [Publisher security policy](https://github.com/sadco-io/sad-chrono/security/policy) · [RustSec submission](https://github.com/RustSec/advisory-db/pull/3613) · [Shared upstream records](https://github.com/RustSec/advisory-db/tree/main/crates/chrono)
