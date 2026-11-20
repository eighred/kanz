#!/bin/sh
set -eu
umask 077
export HOME=/work/home CARGO_HOME=/work/cargo CARGO_INCREMENTAL=0 CARGO_BUILD_JOBS=3 CARGO_PROFILE_TEST_DEBUG=0
owner=/work/kanz/rust
case "$1" in
fetch)
    mkdir -p "$HOME" "$CARGO_HOME" /work/source "$owner"
    tar --no-same-owner -xf /inputs/object.tar -C /work/source
    tar --no-same-owner -xf /inputs/kanz.tar -C /work/kanz
    cat > "$owner/Cargo.toml" <<'EOF'
[workspace]
members = ["services/lake_snapshot"]
resolver = "2"

[patch.crates-io]
object_store = { path = "/work/source" }
EOF
    # Seed the consumer from its real lock. Cargo records the new source in this disposable copy.
    cp "$owner/Cargo.lock" /work/kanz.lock
    cargo fetch --manifest-path "$owner/Cargo.toml"
    python3 -B /inputs/lake_sources.py
    cargo fetch --manifest-path "$owner/Cargo.toml"
    cargo fetch --manifest-path /work/source/Cargo.toml
    ;;
test)
    export CARGO_NET_OFFLINE=true
    sha256sum /work/source/Cargo.lock "$owner/Cargo.lock"
    cargo test --locked --offline --manifest-path /work/source/Cargo.toml --features http --lib config::tests:: \
        > /work/duration.txt
    cargo test --locked --offline --manifest-path /work/source/Cargo.toml --features "aws,azure,gcp,http" --lib \
        > /work/store.txt
    cargo test --locked --offline --manifest-path /work/source/Cargo.toml --features "aws,azure,gcp,http" --lib \
        local::tests::bubble_up_io_errors -- --ignored --exact > /work/permission.txt
    cargo test --locked --offline --manifest-path "$owner/Cargo.toml" -p kanz-lake-snapshot --test worker \
        --message-format=json-render-diagnostics > /work/worker.jsonl
    cp /inputs/named_zone.rs "$owner/services/lake_snapshot/tests/named_zone.rs"
    cargo test --locked --offline --manifest-path "$owner/Cargo.toml" -p kanz-lake-snapshot --test named_zone \
        > /work/named_zone.txt
    cp /inputs/object_metadata.rs "$owner/services/lake_snapshot/tests/object_metadata.rs"
    cargo test --locked --offline --manifest-path "$owner/Cargo.toml" -p kanz-lake-snapshot --test object_metadata \
        > /work/metadata.txt
    PYTHONPATH=/inputs python3 -B - <<'PY'
from lake_sources import verify
from pathlib import Path

def check_summary(lines):
    if not any(line.startswith("test result: ok.") and "; 0 failed; 0 ignored;" in line for line in lines):
        raise ValueError("missing a test summary with zero failures and skips")

duration = Path("/work/duration.txt").read_text().splitlines()
check_summary(duration)
store = Path("/work/store.txt").read_text().splitlines()
expected_store = {
    "local::checked_modified_tests::file_mtime_preserves_nanoseconds_or_returns_an_error",
    "aws::credential::tests::test_sign_with_signed_payload",
    "azure::client::tests::test_delegated_key_response",
    "gcp::credential::tests::test_canonicalize_headers",
}
for case in expected_store:
    if f"test {case} ... ok" not in store:
        raise ValueError(f"missing object_store test: {case}")
permission = Path("/work/permission.txt").read_text().splitlines()
check_summary(permission)
if "test local::tests::bubble_up_io_errors ... ok" not in permission:
    raise ValueError("the non-root directory permission case did not run")
print(next(line for line in store if line.startswith("test result: ok.")))
for line in store:
    if line.startswith("test ") and " ... ignored" in line:
        print(line)
print("The non-root permission test also passed when selected explicitly.")
metadata = Path("/work/metadata.txt").read_text().splitlines()
check_summary(metadata)
if "test partitioned_file_metadata_uses_signed_instants ... ok" not in metadata:
    raise ValueError("the DataFusion metadata case did not run")
expected_duration = {"test_parse_duration", "test_parse_duration_formats", "test_fmt_duration_roundtrip"}
passed_duration = {line.split("::")[-1].split()[0] for line in duration if line.startswith("test config::tests::") and line.endswith(" ... ok")}
if not expected_duration.issubset(passed_duration):
    raise ValueError(f"missing duration cases: {expected_duration - passed_duration}")

lines = Path("/work/worker.jsonl").read_text().splitlines()
check_summary(lines)
verify(lines)
zone = Path("/work/named_zone.txt").read_text().splitlines()
check_summary(zone)
if "test named_zone_timestamps ... ok" not in zone:
    raise ValueError("named-zone DataFusion UDF did not run")
expected_worker = {
    "actual_sandbox_convert_preserves_exact_wire_and_unknown_coordinates",
    "actual_datafusion_queries_preserve_zero_cursor_duplicates_and_nanoseconds",
    "actual_worker_refuses_scope_hash_projection_order_and_query_capacity",
    "actual_maximum_row_snapshot_crosses_checked_arrow_versions",
    "actual_worker_refuses_hash_bound_wrong_storage_type_and_row_tenant",
}
passed_worker = {line.split()[1] for line in lines if line.startswith("test actual_") and line.endswith(" ... ok")}
if not expected_worker.issubset(passed_worker):
    raise ValueError(f"missing worker cases: {expected_worker - passed_worker}")
print(f"Passed {len(passed_duration)} duration cases and {len(passed_worker)} real lake worker cases.")
PY
    ;;
*) exit 2 ;;
esac
