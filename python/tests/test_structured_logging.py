"""Log output is JSON, keyed like the Go services (#241).

Every Go service emits slog JSON, so the estate's log pipeline queries on
`level`, `msg` and `time`. This service emitted
`%(asctime)s %(levelname)s %(name)s %(message)s`, which parses as nothing: its
lines reached the log store as opaque strings, so an estate-wide filter for
level=ERROR silently excluded the inference service. An absent service and a
healthy one look identical in that view — which is the failure mode, not the
untidiness.
"""

from __future__ import annotations

import json
import logging

from kanz_inference.__main__ import _configure_logging


def _emit(capsys, fn) -> list[dict]:
    _configure_logging("INFO")
    fn(logging.getLogger("kanz_inference.test"))
    for h in logging.getLogger().handlers:
        h.flush()
    out = capsys.readouterr().out.strip().splitlines()
    return [json.loads(line) for line in out if line.strip()]


def test_every_line_is_one_json_object(capsys):
    records = _emit(capsys, lambda log: log.info("model loaded"))

    assert len(records) == 1, (
        "expected exactly one JSON object. More than one means a second handler is "
        "installed and every line is emitted twice; zero means nothing reached stdout."
    )
    rec = records[0]
    assert rec["msg"] == "model loaded"
    assert rec["level"] == "INFO"
    assert rec["logger"] == "kanz_inference.test"
    assert "time" in rec


def test_the_level_key_matches_what_the_estate_filters_on(capsys):
    records = _emit(capsys, lambda log: log.error("primary model missing"))
    assert records[0]["level"] == "ERROR", (
        "an estate-wide filter for level=ERROR is how this service's failures are found "
        "at all; a differently-named or differently-cased key excludes it silently"
    )


def test_an_exception_stays_on_one_line(capsys):
    def boom(log):
        try:
            raise ValueError("weights failed to load")
        except ValueError:
            log.exception("model load failed")

    records = _emit(capsys, boom)

    # One object, not an object followed by a bare traceback: the collector's
    # contract is one JSON document per line, and a multi-line traceback breaks
    # every line after the first.
    assert len(records) == 1
    assert "weights failed to load" in records[0]["err"]


def test_interpolated_messages_are_rendered(capsys):
    records = _emit(capsys, lambda log: log.info("bound %s on %d", "grpc", 50051))
    assert records[0]["msg"] == "bound grpc on 50051", (
        "the %-style arguments were not applied — the log store would hold the format "
        "string instead of the fact"
    )
