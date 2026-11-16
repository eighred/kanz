"""The inference pod's /metrics surface actually carries series — #241.

WHAT THESE ASSERT THAT THE ARCH GUARDS CANNOT. kanz/test/arch/ proves the
CONFIGURATION is complete and consistent: the annotation names a declared HTTP
port, the NetworkPolicy admits it, and the source contains a /metrics route.
Every one of those is a string check on files. None of them can distinguish a
module that declares a Counter from a process that, when driven, exposes a
non-zero sample for it over HTTP — which is the difference between an alert that
can fire and a panel that renders empty while reading as healthy.

So this drives the real servicer and the real streaming worker, then reads the
real WSGI app's body and asserts the series are in it with the values the calls
should have produced.
"""

from __future__ import annotations

import asyncio

import pytest

from envelope.v1.envelope_pb2 import Envelope
from inference.v1.feature_vector_pb2 import FeatureVector
from inference.v1.prediction_pb2 import PredictionEnvelope, PredictionMode

from kanz_inference.interactive.servicer import (
    REASON_INFERENCE_UNAVAILABLE,
    REASON_POOL_SATURATED,
    InferenceServicer,
)
from kanz_inference.observability import (
    LIVE_PATH,
    METRICS_PATH,
    READY_PATH,
    ObservabilityServer,
    metrics,
)
from kanz_inference.observability.server import split_bind
from kanz_inference.service import REASON_NO_PRIMARY
from kanz_inference.streaming.worker import (
    REASON_NO_CACHED_PREDICTION,
    StreamingWorker,
)


def scrape(server: ObservabilityServer, path: str = METRICS_PATH) -> tuple[str, str]:
    """Drive the WSGI app directly and return (status, body).

    No socket: the routing and the exposition are what is under test, and a real
    listener would add a port-binding flake to a test whose subject is neither.
    ObservabilityServer.start() binding a socket is covered separately below.
    """
    captured: dict[str, str] = {}

    def start_response(status, headers):
        captured["status"] = status

    body = b"".join(server.app({"PATH_INFO": path, "REQUEST_METHOD": "GET"}, start_response))
    return captured["status"], body.decode("utf-8")


def value(body: str, series: str) -> float | None:
    """The value of a fully-qualified sample line, or None if the body has no such
    line yet.

    None is a REAL state, not a parse failure: prometheus_client emits a labelled
    child only once something has touched it, so a counter that has never been
    incremented for a given label set is genuinely absent from the exposition.
    That is why the "before" readings below tolerate None and the "after" ones do
    not — an assertion that accepted a missing series afterwards would pass on an
    instrumentation that was never wired up.
    """
    for line in body.splitlines():
        if line.startswith("#"):
            continue
        name, _, raw = line.partition(" ")
        if name == series:
            return float(raw)
    return None


def prior(body: str, series: str) -> float:
    """A reading taken BEFORE the act under test. Absent counts as zero."""
    return value(body, series) or 0.0


def sample(body: str, series: str) -> float:
    """The value of a sample line that MUST be present."""
    found = value(body, series)
    if found is None:
        raise AssertionError(f"{series!r} is not in the exposition body:\n{body}")
    return found


class _Boom:
    model_id = "boom@1"

    async def predict(self, fv):
        raise RuntimeError("model exploded")


class _Blocking:
    """Holds every call open so in-flight can reach the admission bound."""

    model_id = "blocking@1"

    def __init__(self):
        self.release = asyncio.Event()

    async def predict(self, fv):
        await self.release.wait()
        return PredictionEnvelope(
            subject_id=fv.subject_id, model=self.model_id, value=1.0, confidence=0.9
        )


class _RecordingPublisher:
    def __init__(self):
        self.published: list[PredictionEnvelope] = []

    async def publish_prediction(self, source_envelope, prediction):
        self.published.append(prediction)


class _FailingPublisher:
    async def publish_prediction(self, source_envelope, prediction):
        raise RuntimeError("bus unreachable")


class _NullSubscriber:
    async def subscribe(self, subject, group, handler):  # pragma: no cover - unused
        raise AssertionError("handle() is driven directly")


def _fv(subject_id: str = "ACC-1") -> FeatureVector:
    return FeatureVector(subject_id=subject_id, feature_set_ref="fs/v1")


# ---------------------------------------------------------------------------
# The surface itself
# ---------------------------------------------------------------------------


def test_metrics_route_serves_the_prometheus_exposition_format():
    server = ObservabilityServer()
    status, body = scrape(server)
    assert status.startswith("200")
    # The declaration alone must put the HELP/TYPE lines in the body, before any
    # call has driven a sample — that is what makes a rule over the series valid
    # from the first scrape rather than from the first request.
    assert "# HELP kanz_inference_predictions_total" in body
    assert "# TYPE kanz_inference_degraded_total counter" in body


def test_readyz_is_503_until_marked_ready_and_503_again_while_draining():
    server = ObservabilityServer()

    # THE POINT OF MOVING OFF tcpSocket. A TCP probe cannot express either of
    # these states: the socket is equally open in all three.
    assert scrape(server, READY_PATH)[0].startswith("503")
    server.mark_ready()
    assert scrape(server, READY_PATH)[0].startswith("200")
    server.mark_draining()
    assert scrape(server, READY_PATH)[0].startswith("503")


def test_livez_answers_regardless_of_readiness():
    server = ObservabilityServer()
    assert scrape(server, LIVE_PATH)[0].startswith("200")


def test_unknown_path_is_404_not_a_silent_200():
    server = ObservabilityServer()
    assert scrape(server, "/healthz")[0].startswith("404")


def test_server_binds_a_real_socket_and_serves_over_http():
    """The WSGI-level tests above never open a port; this one does.

    A router that works in-process and a listener that never binds look identical
    from every other test in this file — and the manifest's probes and scrape go
    through the socket, not the callable.
    """
    import urllib.request

    server = ObservabilityServer("127.0.0.1:0")
    server.start()
    try:
        server.mark_ready()
        with urllib.request.urlopen(
            f"http://127.0.0.1:{server.port}{METRICS_PATH}", timeout=5
        ) as resp:
            assert resp.status == 200
            assert "kanz_inference_predictions_total" in resp.read().decode("utf-8")
        with urllib.request.urlopen(
            f"http://127.0.0.1:{server.port}{READY_PATH}", timeout=5
        ) as resp:
            assert resp.status == 200
    finally:
        server.stop()


@pytest.mark.parametrize(
    "bind,want",
    [
        ("[::]:8093", ("::", 8093, True)),
        ("0.0.0.0:8080", ("0.0.0.0", 8080, False)),
        ("127.0.0.1:0", ("127.0.0.1", 0, False)),
    ],
)
def test_split_bind_accepts_both_forms(bind, want):
    assert split_bind(bind) == want


@pytest.mark.parametrize("bind", ["8093", "[::]8093", "[::]:http"])
def test_split_bind_refuses_a_malformed_address(bind):
    """FAIL LOUDLY. Falling back to a default here would bind a port the pod's own
    prometheus.io/port annotation does not name — a DOWN target and two failing
    probes, from a config typo that produced no error."""
    with pytest.raises(ValueError):
        split_bind(bind)


# ---------------------------------------------------------------------------
# Label cardinality — the one way this could take down the monitoring
# ---------------------------------------------------------------------------


def test_every_wire_degraded_reason_maps_to_a_named_label():
    """metrics.py cannot import these constants (the modules that define them
    import metrics.py), so this is what stops the mapping drifting away from
    them. A reason that starts landing in "other" is a degraded mode that has
    silently stopped being distinguishable on a dashboard."""
    for reason in (
        REASON_POOL_SATURATED,
        REASON_INFERENCE_UNAVAILABLE,
        REASON_NO_CACHED_PREDICTION,
        REASON_NO_PRIMARY,
    ):
        assert metrics.reason_label(reason) != metrics.REASON_OTHER, reason


def test_the_interpolated_no_primary_reason_does_not_become_a_label_per_feature_set():
    """service.py builds ``f"{REASON_NO_PRIMARY}: {fv.feature_set_ref}"`` and
    feature_set_ref arrives on the bus from another service. Using it raw would be
    one new time series per distinct value, for the life of the process."""
    labels = {
        metrics.reason_label(f"{REASON_NO_PRIMARY}: fs/{i}") for i in range(500)
    }
    assert labels == {"no_primary_model"}


def test_an_unrecognised_reason_collapses_rather_than_creating_a_series():
    assert metrics.reason_label("something new nobody mapped") == metrics.REASON_OTHER
    assert metrics.reason_label("") == metrics.REASON_UNSPECIFIED


# ---------------------------------------------------------------------------
# Driving the real servicer, then reading the real body
# ---------------------------------------------------------------------------


async def test_admission_reject_is_visible_in_the_metrics_body():
    model = _Blocking()
    servicer = InferenceServicer(model, max_in_flight=1)
    server = ObservabilityServer()

    start = prior(
        scrape(server)[1],
        'kanz_inference_degraded_total{path="interactive",reason="pool_saturated"}',
    )

    held = asyncio.create_task(servicer.Predict(_fv(), None))
    await asyncio.sleep(0)  # let it take the only slot
    assert servicer.in_flight == 1

    rejected = await servicer.Predict(_fv("ACC-2"), None)
    assert rejected.mode == PredictionMode.PREDICTION_MODE_DEGRADED
    assert rejected.degraded_reason == REASON_POOL_SATURATED

    model.release.set()
    await held

    body = scrape(server)[1]
    assert (
        sample(
            body,
            'kanz_inference_degraded_total{path="interactive",reason="pool_saturated"}',
        )
        == start + 1
    )
    # The bound is exported, so the in-flight gauge can be read against it.
    assert sample(body, "kanz_inference_admission_limit") == 1.0
    # And the reject did NOT enter the latency histogram: exactly one admitted
    # call ran, so the count is the one that was allowed through.
    assert (
        sample(body, 'kanz_inference_predict_seconds_count{path="interactive"}') >= 1.0
    )


async def test_a_raising_model_counts_a_degraded_prediction_not_a_normal_one():
    servicer = InferenceServicer(_Boom(), max_in_flight=4)
    server = ObservabilityServer()

    series = 'kanz_inference_degraded_total{path="interactive",reason="inference_unavailable"}'
    before = prior(scrape(server)[1], series)

    envelope = await servicer.Predict(_fv(), None)
    assert envelope.degraded_reason == REASON_INFERENCE_UNAVAILABLE

    assert sample(scrape(server)[1], series) == before + 1


async def test_in_flight_returns_to_its_prior_value_after_a_failed_call():
    """A gauge that leaks on the error path reads as permanent saturation, which
    is the alert firing forever on a service that is fine."""
    server = ObservabilityServer()
    before = sample(scrape(server)[1], "kanz_inference_predict_in_flight")
    servicer = InferenceServicer(_Boom(), max_in_flight=4)
    await servicer.Predict(_fv(), None)
    assert sample(scrape(server)[1], "kanz_inference_predict_in_flight") == before


# ---------------------------------------------------------------------------
# Driving the real streaming worker
# ---------------------------------------------------------------------------


async def test_a_handled_event_counts_only_once_it_is_published():
    publisher = _RecordingPublisher()

    class _Ok:
        model_id = "ok@1"

        async def predict(self, fv):
            return PredictionEnvelope(
                subject_id=fv.subject_id,
                model=self.model_id,
                value=2.0,
                confidence=0.8,
                mode=PredictionMode.PREDICTION_MODE_NORMAL,
            )

    worker = StreamingWorker(_NullSubscriber(), _Ok(), publisher)
    server = ObservabilityServer()
    series = 'kanz_inference_stream_events_total{outcome="handled"}'
    before = prior(scrape(server)[1], series)

    await worker.handle(Envelope(event_id="ev-1"), _fv().SerializeToString())

    assert len(publisher.published) == 1
    body = scrape(server)[1]
    assert sample(body, series) == before + 1
    assert sample(body, 'kanz_inference_predictions_total{mode="normal",path="streaming"}') >= 1.0


async def test_a_publish_failure_is_its_own_outcome_and_still_propagates():
    worker = StreamingWorker(_NullSubscriber(), _Boom(), _FailingPublisher())
    server = ObservabilityServer()
    handled = 'kanz_inference_stream_events_total{outcome="handled"}'
    failed = 'kanz_inference_stream_events_total{outcome="publish_failed"}'
    before_failed = prior(scrape(server)[1], failed)
    before_handled = prior(scrape(server)[1], handled)

    with pytest.raises(RuntimeError):
        await worker.handle(Envelope(event_id="ev-2"), _fv().SerializeToString())

    body = scrape(server)[1]
    assert sample(body, failed) == before_failed + 1
    # NOT counted as handled: it was scored and never reached the bus. Counting
    # it in both is how a broken publisher reads as a working pipeline.
    assert prior(body, handled) == before_handled


async def test_an_undecodable_payload_is_counted_before_it_is_re_raised():
    worker = StreamingWorker(_NullSubscriber(), _Boom(), _RecordingPublisher())
    server = ObservabilityServer()
    series = 'kanz_inference_stream_events_total{outcome="unmarshal_failed"}'
    before = prior(scrape(server)[1], series)

    with pytest.raises(Exception):
        await worker.handle(Envelope(event_id="ev-3"), b"\xff\xff\xff\xff not a protobuf")

    assert sample(scrape(server)[1], series) == before + 1


async def test_a_cache_miss_fallback_is_labelled_no_cached_prediction():
    worker = StreamingWorker(_NullSubscriber(), _Boom(), _RecordingPublisher())
    server = ObservabilityServer()
    series = 'kanz_inference_degraded_total{path="streaming",reason="no_cached_prediction"}'
    before = prior(scrape(server)[1], series)

    await worker.handle(Envelope(event_id="ev-4"), _fv().SerializeToString())

    assert sample(scrape(server)[1], series) == before + 1
    assert metrics.reason_label(REASON_NO_CACHED_PREDICTION) == "no_cached_prediction"
