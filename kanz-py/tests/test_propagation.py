from __future__ import annotations

import asyncio

from kanz_bus import (
    get_causation_id,
    get_correlation_id,
    get_trace_context,
    propagation_context,
)


def test_defaults_are_empty():
    assert get_correlation_id() == ""
    assert get_causation_id() == ""
    assert get_trace_context() == ""


def test_propagation_context_sets_and_resets():
    with propagation_context(
        correlation_id="corr",
        causation_id="caus",
        trace_context="trace",
    ):
        assert get_correlation_id() == "corr"
        assert get_causation_id() == "caus"
        assert get_trace_context() == "trace"
    assert get_correlation_id() == ""
    assert get_causation_id() == ""
    assert get_trace_context() == ""


def test_propagation_context_partial():
    with propagation_context(correlation_id="corr"):
        assert get_correlation_id() == "corr"
        assert get_causation_id() == ""
        assert get_trace_context() == ""


def test_propagation_context_empty_args_dont_overwrite():
    with propagation_context(correlation_id="outer"):
        # Inner block with empty values must NOT clobber outer's value —
        # mirrors the Go client's "non-empty only" rule.
        with propagation_context(causation_id="caus"):
            assert get_correlation_id() == "outer"
        assert get_correlation_id() == "outer"


def test_propagation_context_nested_override_and_restore():
    with propagation_context(correlation_id="outer"):
        with propagation_context(correlation_id="inner"):
            assert get_correlation_id() == "inner"
        assert get_correlation_id() == "outer"


async def test_propagation_survives_await():
    with propagation_context(correlation_id="from-outside"):
        await asyncio.sleep(0)
        assert get_correlation_id() == "from-outside"


async def test_propagation_copied_into_new_task():
    # asyncio.create_task copies the current Context into the new task.
    captured = ""

    async def child() -> None:
        nonlocal captured
        captured = get_correlation_id()

    with propagation_context(correlation_id="from-parent"):
        await asyncio.create_task(child())
    assert captured == "from-parent"
