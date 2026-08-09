"""The prediction surface refuses to serve plaintext (SEC-M3, #241).

`servicer.py` called `server.add_insecure_port` unconditionally. Every other
gRPC surface in the estate at least *has* an mTLS code path; this one had none,
so `inference:50051` answered any workload that could reach the port — protected
only by a NetworkPolicy that has never run on an enforcing CNI.

Two properties are pinned here, and they fail in opposite directions:

  * with NO configuration the server REFUSES TO START, rather than logging a
    warning and serving plaintext anyway (which is what the Go servers do, and
    the reason nobody notices an unprotected surface); and
  * with certificates it requires a CLIENT certificate too. `require_client_auth`
    is the half that is easy to omit: without it the server presents a
    certificate and accepts anybody, which reads as "TLS is on" everywhere a
    human would look while authenticating nobody.

The second is proven by a real connection attempt, not by inspecting the
credentials object.
"""

from __future__ import annotations

import asyncio
from pathlib import Path

import grpc
import pytest

from kanz_bus import mtls
from kanz_inference.interactive.servicer import InsecureServicerRefused, serve

# The certificate helpers live with the bus mTLS tests; both need SVID-shaped
# throwaway certs and a second copy would be a second thing to keep correct.
from tests.test_bus_mtls import _ca, _svid, _write  # noqa: E402


class _Model:
    model_id = "test-model@1.0.0"

    async def predict(self, fv):  # pragma: no cover - never reached here
        raise AssertionError("no call should get this far")


@pytest.fixture
def certs(tmp_path: Path) -> Path:
    ca_key, ca_cert = _ca()
    key, cert = _svid(ca_key, ca_cert, "/ns/kanz-services/sa/inference", dns="inference")
    d = tmp_path / "certs"
    d.mkdir()
    _write(d, key, cert, ca_cert)
    return d


# ------------------------------------------------------------- refuse to start


async def test_with_no_tls_configuration_the_surface_refuses_to_start():
    with pytest.raises(InsecureServicerRefused) as e:
        await serve(_Model(), "127.0.0.1:0")

    msg = str(e.value)
    assert "KANZ_INFERENCE_GRPC_CERT_DIR" in msg, (
        "the refusal must name the setting that fixes it — an operator meeting this at "
        "rollout needs the next action, not just a complaint"
    )


async def test_plaintext_requires_an_explicit_opt_in():
    """The dev escape hatch works, and only when asked for by name."""
    server = await serve(_Model(), "127.0.0.1:0", allow_insecure=True)
    try:
        assert server is not None
    finally:
        await asyncio.wait_for(server.stop(None), timeout=5)


async def test_missing_svid_material_is_named_rather_than_downgraded(tmp_path: Path):
    with pytest.raises(mtls.MissingSVIDError) as e:
        await serve(_Model(), "127.0.0.1:0", cert_dir=str(tmp_path / "absent"))
    assert mtls.SVID_FILE in str(e.value)


# --------------------------------------------------- the mTLS surface is mutual


async def _ready(port: int, creds, timeout: float = 4.0) -> bool:
    """Try to complete a TLS handshake against the servicer. True if it came up.

    The channel is closed EXPLICITLY, with its own timeout, rather than via
    `async with`. grpc.aio's __aexit__ does not return under pytest-asyncio once
    a handshake has been refused, and the whole file hangs — which reads as a
    broken test rather than the refusal it is actually observing.
    """
    opts = (("grpc.ssl_target_name_override", "inference"),)
    chan = grpc.aio.secure_channel(f"127.0.0.1:{port}", creds, options=opts)
    try:
        await asyncio.wait_for(chan.channel_ready(), timeout=timeout)
        return True
    except (asyncio.TimeoutError, grpc.aio.AioRpcError):
        return False
    finally:
        try:
            await asyncio.wait_for(chan.close(), timeout=5)
        except asyncio.TimeoutError:
            pass


async def test_a_tls_client_with_NO_certificate_is_refused(certs: Path):
    """THE TEST THAT MATTERS, and the reason it is shaped this way.

    An earlier version pointed a PLAINTEXT client at the TLS port. That passed
    with require_client_auth=False — because a plaintext client cannot talk to a
    TLS port either way. It proved the port was encrypted and said nothing about
    who may use it.

    This client speaks TLS and trusts the CA. Its ONLY deficiency is that it
    presents no certificate of its own. If require_client_auth were dropped, it
    would be served — and the surface would answer any workload that can reach
    the port while every log line still says TLS.
    """
    bundle = (certs / mtls.BUNDLE_FILE).read_bytes()
    port = 50553
    server = await serve(_Model(), f"127.0.0.1:{port}", cert_dir=str(certs))
    try:
        anonymous = grpc.ssl_channel_credentials(root_certificates=bundle)
        assert not await _ready(port, anonymous), (
            "a client presenting NO certificate completed the handshake. The server is "
            "presenting a certificate and authenticating nobody, which reads as 'TLS is on' "
            "everywhere a human would look."
        )
    finally:
        await asyncio.wait_for(server.stop(None), timeout=5)


async def test_a_client_holding_an_svid_IS_served(certs: Path):
    """NON-VACUITY for the test above: the same setup, with a client certificate,
    must succeed. Without this pair, a servicer that refused EVERY connection —
    or never started — would look like a security property being enforced."""
    bundle = (certs / mtls.BUNDLE_FILE).read_bytes()
    chain = (certs / mtls.SVID_FILE).read_bytes()
    key = (certs / mtls.SVID_KEY_FILE).read_bytes()
    port = 50554
    server = await serve(_Model(), f"127.0.0.1:{port}", cert_dir=str(certs))
    try:
        authed = grpc.ssl_channel_credentials(
            root_certificates=bundle, private_key=key, certificate_chain=chain
        )
        assert await _ready(port, authed), (
            "a client holding a valid SVID could not reach the prediction surface — the mTLS "
            "wiring rejects everything, so the test above proves nothing"
        )
    finally:
        await asyncio.wait_for(server.stop(None), timeout=5)
