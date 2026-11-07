# Finite snapshot worker

This Linux x86-64 worker converts already authorized, tenant-pure landed rows
to Parquet and executes three fixed DataFusion operations: event listing,
event-type counts and decode-failure listing. The Go lake owner authenticates,
authorizes, validates canonical envelopes, owns files and commits lifecycle
facts. The worker accepts no credentials, SQL, paths or object-store URLs.
It is not an order, accounting, backtest or complete-history authority.

The versioned protocol is `lake.worker.v1`. One big-endian u32 length prefixes
one request. Conversion accepts at most 64 MiB encoded and 100,000 rows. Query
control accepts 64 KiB followed by exactly its digest-bound Parquet body, at
most 96 MiB. The control limit is checked after the global 64 MiB decode cap.
Both operations require EOF; output is one framed response and EOF. Query
output is bounded to 1 MiB and 1,000 rows; an excessive count-group result
refuses instead of truncating. Optional ordinal zero and absent Kafka
coordinates retain their distinct meanings. Original frames, including
unknown fields and Decimal payload bytes, are stored unchanged.

Parquet is uncompressed with a closed schema and tenant-bound metadata.
Metadata is checked before record-array decoding; footer/schema parsing itself
allocates under the process limit. Decoded batches and the DataFusion memory
pool have separate limits. Queries use only a private in-memory table; disk
spill is disabled. This is not an Iceberg catalog, object-store commit or
source-range completeness implementation.

Before reading input, the executable closes inherited descriptors above stderr
(Linux 5.9+), creates a current-thread runtime and installs hard address-space
(512 MiB), CPU (20 seconds), descriptor and core limits plus a checked seccomp
filter. The filter refuses file opens, network sockets, exec and thread/process
creation; stdio descriptors and bounded memory/entropy operations are explicit.
Unsupported systems or failed confinement refuse startup. The parent still
owns one wall-clock deadline, bounded IO, cancellation, kill and actual join.
Its concurrency slot remains occupied until child and pipe owners have joined.

Use DataFusion 54.1.0 and Arrow/Parquet 58.4.1 with the pinned Rust 1.90
toolchain. DataFusion 55 requires a newer toolchain; version selection does
not replace dependency/advisory qualification.

Linux qualification builds the separate syscall probe before running tests:

```sh
export KANZ_BUILD_VERSION="$(git rev-parse HEAD)"
cargo +1.90.0 build --release --locked -p kanz-lake-snapshot --bin kanz-lake-snapshot --example sandbox-probe
cargo +1.90.0 test --release --locked -p kanz-lake-snapshot --all-targets -- --test-threads=1
cargo +1.90.0 clippy --release --locked -p kanz-lake-snapshot --all-targets -- -D warnings
```

The architecture workflow requires this process/query proof in addition to
the existing compatibility job. Successful compilation or pure BPF tests do
not prove kernel enforcement, service authorization or deployed immutability.
The executable must be a hash-verified, root-owned, nonwritable ELF in a
qualified read-only image, launched by a nonroot Go owner through its verified
descriptor. Filesystem ownership/manifest hashes alone do not prevent privileged
same-inode mutation. Go service wiring, registry/outbox, broker/network policy,
actual deployment and retained-history migration require their own acceptance.
