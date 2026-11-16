"""Lineage propagation via :mod:`contextvars` — the Python idiom for what
Go does with ``context.Context``.

A :class:`Consumer` stashes the inbound envelope's ``correlation_id``, the
inbound ``event_id`` (as the *next* event's ``causation_id``), and the
inbound ``trace_context`` onto module-level contextvars. ``Producer.publish``
reads them when the corresponding :class:`Event` field is empty (precedence:
explicit Event field > contextvar > root default).

asyncio copies contextvars across ``await`` boundaries within the same
coroutine and copies a snapshot into newly-created tasks, so lineage
propagates through ``await`` chains without further effort.
"""

from __future__ import annotations

from contextlib import contextmanager
from contextvars import ContextVar
from typing import Iterator

_correlation_id: ContextVar[str] = ContextVar("kanz_bus.correlation_id", default="")
_causation_id: ContextVar[str] = ContextVar("kanz_bus.causation_id", default="")
_trace_context: ContextVar[str] = ContextVar("kanz_bus.trace_context", default="")
# MT-01a: the tenant rides the same mechanism as lineage. A derived event belongs
# to the tenant of the event that caused it — a prediction about acme's portfolio
# is acme's, and it cannot be re-attributed by whatever the consuming process was
# configured with. The Go client stashes the inbound tenant on ctx for exactly the
# same reason.
_tenant_id: ContextVar[str] = ContextVar("kanz_bus.tenant_id", default="")


def get_correlation_id() -> str:
    """Return the correlation_id from the current context, or ``""``."""
    return _correlation_id.get()


def get_causation_id() -> str:
    """Return the causation_id from the current context, or ``""``."""
    return _causation_id.get()


def get_tenant_id() -> str:
    """The inbound event's tenant, for a derived event to inherit."""
    return _tenant_id.get()


def get_trace_context() -> str:
    """Return the trace_context from the current context, or ``""``."""
    return _trace_context.get()


@contextmanager
def propagation_context(
    *,
    correlation_id: str = "",
    causation_id: str = "",
    trace_context: str = "",
    tenant_id: str = "",
) -> Iterator[None]:
    """Set lineage fields on the current context for the block's duration.

    Empty values are not set — that avoids overwriting an outer context's
    value with an empty string (matches the Go client's "non-empty only"
    stashing rule).

    Usage::

        with propagation_context(correlation_id="root", trace_context="00-..."):
            await producer.publish(event)
    """
    tokens: list[tuple[ContextVar[str], object]] = []
    if correlation_id:
        tokens.append((_correlation_id, _correlation_id.set(correlation_id)))
    if causation_id:
        tokens.append((_causation_id, _causation_id.set(causation_id)))
    if trace_context:
        tokens.append((_trace_context, _trace_context.set(trace_context)))
    if tenant_id:
        tokens.append((_tenant_id, _tenant_id.set(tenant_id)))
    try:
        yield
    finally:
        for var, tok in reversed(tokens):
            var.reset(tok)  # type: ignore[arg-type]
