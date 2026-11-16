"""The inference service's HTTP surface: /metrics, /livez, /readyz (#241).

THE GAP THIS CLOSES. inference-deploy.yaml opened exactly one port — grpc 50051 —
and probed it with ``tcpSocket``, because gRPC answers no GET. So there was no
port a ``prometheus.io/port`` annotation could name: annotating 50051 would have
produced a target that is permanently DOWN, which is worse than none at all
(kanz/test/arch/prometheus_scrape_test.go's exemption said exactly this). The gap
was never a missing annotation; it was a missing surface. This is the surface.

WHY THE PROBES MOVE OFF tcpSocket, WHICH WAS NOT WRONG. The old manifest's
argument held: the gRPC port binds only AFTER a PRIMARY model is loaded, so
"listening" genuinely meant "has a validated model". What a TCP check CANNOT
express is the other end of the pod's life. On SIGTERM the listening socket stays
accepted-by-the-kernel until the process exits, so a draining pod passes its
readiness probe for the whole grace period and keeps receiving traffic it is
about to drop. ``/readyz`` goes 503 the moment the drain starts, which is what
takes the pod out of its Service endpoints BEFORE the gRPC server stops.

WHY WSGI AND A THREAD, NOT THE asyncio LOOP. The scrape and the probes must
answer while the event loop is busy — a pod whose loop is wedged is exactly the
pod you need ``/metrics`` from. Serving them from the same loop makes the health
surface report healthy precisely when it is not, because a blocked loop simply
never gets to the handler and the probe times out into a restart with no series
explaining why. A daemon thread answers regardless.

STDLIB + prometheus_client ONLY. No aiohttp, no flask: three routes and a
registry do not justify a web framework on the image that scores risk models.
"""

from __future__ import annotations

import logging
import socket
import threading
from socketserver import ThreadingMixIn
from typing import Callable, Iterable
from wsgiref.simple_server import WSGIRequestHandler, WSGIServer, make_server

from prometheus_client import REGISTRY, CollectorRegistry, make_wsgi_app

logger = logging.getLogger(__name__)

# 8093 — the next free port in the estate's metrics range (8080..8092 are taken;
# see infra/security/runtime/network-policies.yaml's allow-observability-scrape,
# whose port list this must be added to or the scrape is refused by the CNI).
#
# A SEPARATE PORT FROM gRPC, not a multiplexed one: allow-observability-scrape's
# peer is the whole kanz-observability namespace, so anything sharing the metrics
# port is reachable from the monitoring plane. Nothing but these three routes is.
DEFAULT_METRICS_BIND = "[::]:8093"

METRICS_PATH = "/metrics"
LIVE_PATH = "/livez"
READY_PATH = "/readyz"


class _ThreadingWSGIServer(ThreadingMixIn, WSGIServer):
    """One thread per request, and none of them keeps the process alive.

    Single-threaded would be enough for two probes and a scrape until the day a
    collector is slow, at which point the readiness probe queues behind it and
    the kubelet restarts a pod whose only fault was a slow scrape.
    """

    daemon_threads = True


class _ThreadingWSGIServer6(_ThreadingWSGIServer):
    """The IPv6 variant. ``address_family`` is a CLASS attribute on
    socketserver.TCPServer, so the family cannot be chosen per instance."""

    address_family = socket.AF_INET6


class _QuietHandler(WSGIRequestHandler):
    """Route access logs through logging at DEBUG instead of stderr.

    Two probes every five seconds per pod, plus a scrape every fifteen, is ~1500
    lines an hour of successful health checks per replica in the log pipeline the
    real failures have to be found in.
    """

    def log_message(self, format: str, *args) -> None:  # noqa: A002 - stdlib signature
        logger.debug("observability http: " + format, *args)


def split_bind(bind: str) -> tuple[str, int, bool]:
    """Split ``host:port`` into (host, port, is_ipv6).

    Accepts the bracketed form the gRPC listener uses (``[::]:8093``) so both
    ports in this process are configured the same way.

    IT RAISES ON ANYTHING ELSE. A bind string this cannot parse is a
    misconfiguration, and the alternative — falling back to a default — is a pod
    that serves metrics on a port its own annotation does not name, i.e. a target
    that is DOWN while the process reports healthy.
    """
    raw = bind.strip()
    if raw.startswith("["):
        host, sep, port = raw[1:].partition("]:")
        if not sep:
            raise ValueError(f"malformed bracketed bind address {bind!r} (want '[::]:8093')")
        ipv6 = True
    else:
        host, sep, port = raw.rpartition(":")
        if not sep:
            raise ValueError(f"malformed bind address {bind!r} (want 'host:port')")
        ipv6 = ":" in host
    try:
        number = int(port)
    except ValueError as e:
        raise ValueError(f"bind address {bind!r} has a non-numeric port {port!r}") from e
    return host, number, ipv6


class ObservabilityServer:
    """Serves /metrics, /livez and /readyz on its own port, off the event loop.

    Lifecycle, and the order matters:

        server = ObservabilityServer(bind)
        server.start()          # /livez answers; /readyz is 503
        ...                     # gRPC bound, streaming worker subscribed
        server.mark_ready()     # /readyz answers 200 — the pod joins its Service
        ...
        server.mark_draining()  # /readyz 503 — the pod LEAVES its Service
        ...                     # only now stop the gRPC server
        server.stop()

    ``_ready`` is a threading.Event because the flag is written on the asyncio
    loop and read on the HTTP thread.
    """

    def __init__(self, bind: str = DEFAULT_METRICS_BIND, registry: CollectorRegistry = REGISTRY):
        self._bind = bind
        self._registry = registry
        self._ready = threading.Event()
        self._httpd: WSGIServer | None = None
        self._thread: threading.Thread | None = None

    @property
    def ready(self) -> bool:
        return self._ready.is_set()

    def mark_ready(self) -> None:
        self._ready.set()

    def mark_draining(self) -> None:
        self._ready.clear()

    def app(self, environ: dict, start_response: Callable) -> Iterable[bytes]:
        """The WSGI router. Exposed so a test can drive it without a socket."""
        path = environ.get("PATH_INFO", "")
        if path == METRICS_PATH:
            return make_wsgi_app(self._registry)(environ, start_response)
        if path == LIVE_PATH:
            return _plain(start_response, "200 OK", b"ok\n")
        if path == READY_PATH:
            if self._ready.is_set():
                return _plain(start_response, "200 OK", b"ready\n")
            # 503, not 500: this is the documented drain/startup state, and it is
            # what the endpoints controller acts on.
            return _plain(start_response, "503 Service Unavailable", b"not ready\n")
        return _plain(start_response, "404 Not Found", b"not found\n")

    def start(self) -> None:
        """Bind and serve. RAISES if the port cannot be bound.

        Deliberately not swallowed. A process that keeps running after failing to
        open its metrics port is a pod carrying a scrape annotation for a port
        nothing listens on — an unreachable target, which shows up nowhere.
        """
        if self._httpd is not None:
            raise RuntimeError("observability server already started")
        host, port, ipv6 = split_bind(self._bind)
        server_class = _ThreadingWSGIServer6 if ipv6 else _ThreadingWSGIServer
        self._httpd = make_server(
            host, port, self.app, server_class=server_class, handler_class=_QuietHandler
        )
        self._thread = threading.Thread(
            target=self._httpd.serve_forever, name="observability-http", daemon=True
        )
        self._thread.start()
        logger.info(
            "observability http listening on %s (%s, %s, %s)",
            self._bind,
            METRICS_PATH,
            LIVE_PATH,
            READY_PATH,
        )

    def stop(self) -> None:
        self.mark_draining()
        if self._httpd is None:
            return
        self._httpd.shutdown()
        self._httpd.server_close()
        if self._thread is not None:
            self._thread.join(timeout=5)
        self._httpd = None
        self._thread = None

    @property
    def port(self) -> int:
        """The port actually bound. Differs from the configured one only when the
        bind asked for :0, which is how the tests get a free port."""
        if self._httpd is None:
            raise RuntimeError("observability server is not running")
        return self._httpd.server_port


def _plain(start_response: Callable, status: str, body: bytes) -> list[bytes]:
    start_response(
        status,
        [("Content-Type", "text/plain; charset=utf-8"), ("Content-Length", str(len(body)))],
    )
    return [body]
