"""Wire frame helper — :func:`unframe` for consumer-side decoding."""

from __future__ import annotations

from envelope.v1.envelope_pb2 import Envelope
from envelope.v1.event_frame_pb2 import EventFrame


def unframe(body: bytes) -> tuple[Envelope, bytes]:
    """Deserialize a ``bus.Message.body`` into ``(envelope, payload bytes)``.

    Consumers look up the payload schema in the registry (EVT-16) and decode
    the payload against it. The envelope is returned as-is; callers that
    want defensive validation should run :func:`kanz_bus.validate` themselves.

    Raises :class:`ValueError` on a malformed frame.
    """
    frame = EventFrame()
    try:
        frame.ParseFromString(body)
    except Exception as e:
        raise ValueError(f"frame unmarshal: {e}") from e
    if not frame.HasField("envelope"):
        raise ValueError("frame missing envelope")
    return frame.envelope, frame.payload
