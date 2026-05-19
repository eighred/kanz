"""Cross-language serialization parity (EVT-21b), Python side.

Loads the Go-generated fixtures from ``kanz/test/contract/serialization/
fixtures/`` and asserts that Python ``unframe`` produces an envelope
whose field values match the manifest entry exactly. The fixtures are
the wire-level contract: if Python reads Go's bytes and gets different
field values, that is a real bug — the bus client is wire-incompatible.

CI generates the fixtures before running this suite:
    go run ./test/contract/serialization/cmd/genfixtures \
        -out ./test/contract/serialization/fixtures

Local dev: same command. If fixtures are absent these tests skip with
the regenerate hint.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest

from kanz_bus import EventClass, QualityFlag, unframe, validate

REPO_ROOT = Path(__file__).resolve().parents[2]
FIXTURES_DIR = REPO_ROOT / "kanz" / "test" / "contract" / "serialization" / "fixtures"
MANIFEST_PATH = FIXTURES_DIR / "manifest.json"


def _load_manifest() -> list[dict[str, Any]]:
    if not MANIFEST_PATH.exists():
        pytest.skip(
            f"{MANIFEST_PATH} not present — run: "
            "go run ./test/contract/serialization/cmd/genfixtures "
            "-out ./test/contract/serialization/fixtures"
        )
    with MANIFEST_PATH.open("r", encoding="utf-8") as f:
        return json.load(f)["fixtures"]


def _ids(fixtures: list[dict[str, Any]]) -> list[str]:
    return [f["name"] for f in fixtures]


FIXTURES = _load_manifest() if MANIFEST_PATH.exists() else []


@pytest.mark.parametrize("fixture", FIXTURES, ids=_ids(FIXTURES) or ["<no-fixtures>"])
def test_python_unframes_go_fixture(fixture: dict[str, Any]) -> None:
    bin_path = FIXTURES_DIR / fixture["file"]
    body = bin_path.read_bytes()

    env, payload = unframe(body)

    # Validate roundtripped envelope passes the same contract EVT-21a
    # established — no fixture is allowed to ship an invalid envelope.
    validate(env)

    # Payload bytes match the manifest's hex blob exactly.
    expected_payload = bytes.fromhex(fixture["payload_hex"])
    assert payload == expected_payload, (
        f"{fixture['name']}: payload mismatch"
    )

    want = fixture["envelope"]
    assert env.event_id == want["event_id"]
    assert env.event_type == want["event_type"]
    assert env.schema_version == want["schema_version"]
    assert env.envelope_version == want["envelope_version"]
    assert env.event_class == EventClass.Value(want["event_class"])
    assert env.domain == want["domain"]
    _assert_ts(env.event_time, want["event_time"])
    _assert_ts(env.ingestion_time, want["ingestion_time"])
    _assert_ts(env.publish_time, want["publish_time"])
    assert env.correlation_id == want["correlation_id"]
    assert env.causation_id == want["causation_id"]
    assert env.trace_context == want["trace_context"]
    assert env.source == want["source"]
    assert env.producer_version == want["producer_version"]
    assert env.partition_key == want["partition_key"]
    assert env.producer_sequence == want["producer_sequence"]
    assert env.idempotency_key == want["idempotency_key"]
    assert env.payload_schema_ref == want["payload_schema_ref"]

    got_flags = [QualityFlag.Name(f) for f in env.quality_flags]
    assert got_flags == want["quality_flags"]


def _assert_ts(got: Any, want: dict[str, int]) -> None:
    assert got.seconds == want["seconds"], (
        f"seconds: got {got.seconds} want {want['seconds']}"
    )
    assert got.nanos == want["nanos"], (
        f"nanos: got {got.nanos} want {want['nanos']}"
    )
