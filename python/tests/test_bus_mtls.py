"""The SPIFFE mTLS client context (SEC-01c / SEC-M3, #241).

The streaming path is the WIRED half of the inference service — risk-engine
really does publish ``inference.feature.computed`` — and it connected to the
broker in plaintext, under no identity, with no SVID to present. On a broker
configured ``verify: true`` (``infra/nats/nats.yaml``) it could not connect at
all.

The dangerous fix is the one that looks the same from outside: build an
``SSLContext``, turn OFF hostname verification because "SPIFFE identifies by URI
SAN, not DNS", and ship it. The connection is then encrypted and authenticates
NOTHING in particular — every certificate the trust bundle ever signed is an
acceptable broker, so any workload in the trust domain can impersonate it. That
is worse than the plaintext it replaces, because it reads as solved.

So the tests below do not assert on flags alone. Two of them complete a REAL TLS
handshake against a local server and require that a certificate for the wrong
name is REJECTED — which is the only way to tell a context that verifies from one
that merely encrypts.
"""

from __future__ import annotations

import datetime
import socket
import ssl
import threading
from pathlib import Path

import pytest

from kanz_bus import mtls

cryptography = pytest.importorskip(
    "cryptography",
    reason="cryptography builds the throwaway SVID-shaped certs these tests handshake with",
)

from cryptography import x509  # noqa: E402
from cryptography.hazmat.primitives import hashes, serialization  # noqa: E402
from cryptography.hazmat.primitives.asymmetric import ec  # noqa: E402
from cryptography.x509.oid import NameOID  # noqa: E402

TRUST_DOMAIN = "spiffe://kanz.internal"
BROKER_DNS = "nats.kanz-messaging.svc"


def _ca() -> tuple[ec.EllipticCurvePrivateKey, x509.Certificate]:
    key = ec.generate_private_key(ec.SECP256R1())
    name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "kanz-test-ca")])
    now = datetime.datetime.now(datetime.timezone.utc)
    cert = (
        x509.CertificateBuilder()
        .subject_name(name)
        .issuer_name(name)
        .public_key(key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - datetime.timedelta(minutes=5))
        .not_valid_after(now + datetime.timedelta(hours=1))
        .add_extension(x509.BasicConstraints(ca=True, path_length=None), critical=True)
        # Real SPIRE certs carry SKI/AKI, and modern OpenSSL requires the pair to
        # build a chain. Without them the handshake fails "Missing Authority Key
        # Identifier" — a fixture defect that would look exactly like the client
        # context rejecting a valid broker.
        .add_extension(x509.SubjectKeyIdentifier.from_public_key(key.public_key()), critical=False)
        .add_extension(
            x509.KeyUsage(
                digital_signature=False, content_commitment=False, key_encipherment=False,
                data_encipherment=False, key_agreement=False, key_cert_sign=True,
                crl_sign=True, encipher_only=False, decipher_only=False,
            ),
            critical=True,
        )
        .sign(key, hashes.SHA256())
    )
    return key, cert


def _svid(ca_key, ca_cert, spiffe_path: str, dns: str | None):
    """A leaf shaped like a SPIRE SVID: a SPIFFE URI SAN, optionally a DNS SAN.

    The DNS SAN is what infra/security/spire/registration.yaml's dnsNameTemplates
    add, and what makes ordinary hostname verification meaningful here.
    """
    key = ec.generate_private_key(ec.SECP256R1())
    sans: list[x509.GeneralName] = [x509.UniformResourceIdentifier(f"{TRUST_DOMAIN}{spiffe_path}")]
    if dns:
        sans.append(x509.DNSName(dns))
    now = datetime.datetime.now(datetime.timezone.utc)
    cert = (
        x509.CertificateBuilder()
        .subject_name(x509.Name([]))
        .issuer_name(ca_cert.subject)
        .public_key(key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - datetime.timedelta(minutes=5))
        .not_valid_after(now + datetime.timedelta(hours=1))
        .add_extension(x509.SubjectAlternativeName(sans), critical=True)
        .add_extension(
            x509.AuthorityKeyIdentifier.from_issuer_public_key(ca_key.public_key()), critical=False
        )
        .sign(ca_key, hashes.SHA256())
    )
    return key, cert


def _write(dir_: Path, key, cert, ca_cert) -> None:
    """Write the three PEMs spiffe-helper maintains, under its own file names."""
    (dir_ / mtls.SVID_FILE).write_bytes(cert.public_bytes(serialization.Encoding.PEM))
    (dir_ / mtls.SVID_KEY_FILE).write_bytes(
        key.private_bytes(
            encoding=serialization.Encoding.PEM,
            format=serialization.PrivateFormat.PKCS8,
            encryption_algorithm=serialization.NoEncryption(),
        )
    )
    (dir_ / mtls.BUNDLE_FILE).write_bytes(ca_cert.public_bytes(serialization.Encoding.PEM))


@pytest.fixture
def svid_dir(tmp_path: Path):
    """A client SVID directory plus the CA that signed it."""
    ca_key, ca_cert = _ca()
    key, cert = _svid(ca_key, ca_cert, "/ns/kanz-services/sa/inference", dns="inference")
    d = tmp_path / "certs"
    d.mkdir()
    _write(d, key, cert, ca_cert)
    return d, ca_key, ca_cert


# ---------------------------------------------------------------- fail loudly


def test_a_missing_cert_dir_raises_rather_than_downgrading(tmp_path: Path):
    with pytest.raises(mtls.MissingSVIDError) as e:
        mtls.client_context(str(tmp_path / "nope"))
    assert mtls.SVID_FILE in str(e.value)


@pytest.mark.parametrize("absent", [mtls.SVID_FILE, mtls.SVID_KEY_FILE, mtls.BUNDLE_FILE])
def test_each_missing_file_is_named(svid_dir, absent: str):
    d, _, _ = svid_dir
    (d / absent).unlink()

    with pytest.raises(mtls.MissingSVIDError) as e:
        mtls.client_context(str(d))
    assert absent in str(e.value), (
        "the error must name the file that is missing — the operator's next action is to look "
        "at the spiffe-helper sidecar, and 'SVID material missing' alone does not say which"
    )


def test_an_empty_cert_dir_is_refused():
    with pytest.raises(mtls.MissingSVIDError):
        mtls.client_context("")


# ------------------------------------------------------- the security property


def test_the_context_requires_and_verifies_a_peer_certificate(svid_dir):
    d, _, _ = svid_dir
    ctx = mtls.client_context(str(d))

    assert ctx.verify_mode == ssl.CERT_REQUIRED
    assert ctx.check_hostname is True, (
        "hostname verification is OFF. Every certificate the trust bundle signed is now an "
        "acceptable broker, so any workload in the trust domain can impersonate it — encrypted, "
        "authenticated by nothing, and indistinguishable from a correct setup from the outside."
    )
    assert ctx.minimum_version >= ssl.TLSVersion.TLSv1_2, (
        f"the TLS floor is {ctx.minimum_version!r}. It happens to be TLS 1.2 by default on the "
        "interpreters we ship, which is exactly why it is asserted: the floor must be a property "
        "of this connection, not of however the host OpenSSL was built."
    )


def _handshake(client_ctx: ssl.SSLContext, server_ctx: ssl.SSLContext, server_hostname: str):
    """Complete one real TLS handshake over a loopback socket.

    Returns None on success, or the client-side exception. A flag assertion cannot
    distinguish a context that verifies from one that merely encrypts; this can.
    """
    lsock = socket.socket()
    lsock.bind(("127.0.0.1", 0))
    lsock.listen(1)
    port = lsock.getsockname()[1]
    server_err: list[BaseException] = []

    def serve():
        try:
            raw, _ = lsock.accept()
            with server_ctx.wrap_socket(raw, server_side=True):
                pass
        except BaseException as exc:  # noqa: BLE001 - reported, not swallowed
            server_err.append(exc)
        finally:
            lsock.close()

    t = threading.Thread(target=serve, daemon=True)
    t.start()
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=10) as raw:
            with client_ctx.wrap_socket(raw, server_hostname=server_hostname):
                pass
    except Exception as exc:
        t.join(timeout=10)
        return exc
    t.join(timeout=10)
    return None


def _server_ctx(key, cert, tmp_path: Path, ca_cert) -> ssl.SSLContext:
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.minimum_version = ssl.TLSVersion.TLSv1_2
    p = tmp_path / "server"
    p.mkdir(exist_ok=True)
    _write(p, key, cert, ca_cert)
    ctx.load_cert_chain(certfile=str(p / mtls.SVID_FILE), keyfile=str(p / mtls.SVID_KEY_FILE))
    return ctx


def test_a_broker_svid_with_the_right_dns_san_is_accepted(svid_dir, tmp_path: Path):
    d, ca_key, ca_cert = svid_dir
    skey, scert = _svid(ca_key, ca_cert, "/ns/kanz-messaging/sa/nats", dns=BROKER_DNS)

    err = _handshake(
        mtls.client_context(str(d)),
        _server_ctx(skey, scert, tmp_path, ca_cert),
        server_hostname=BROKER_DNS,
    )
    assert err is None, (
        f"the handshake against a correctly-named broker SVID failed: {err!r}. "
        "SPIRE issues these DNS SANs (registration.yaml dnsNameTemplates) precisely so a "
        "library that checks hostnames can verify the broker."
    )


def test_an_in_trust_domain_certificate_for_ANOTHER_workload_is_rejected(svid_dir, tmp_path: Path):
    """THE TEST THAT MATTERS.

    This peer's certificate is signed by the same trust bundle and carries a
    perfectly valid SPIFFE ID — it is simply not the broker. If hostname
    verification were disabled it would be accepted, and an attacker holding any
    SVID in the trust domain could stand in for the bus.
    """
    d, ca_key, ca_cert = svid_dir
    skey, scert = _svid(ca_key, ca_cert, "/ns/kanz-services/sa/impostor", dns="impostor.kanz-services.svc")

    err = _handshake(
        mtls.client_context(str(d)),
        _server_ctx(skey, scert, tmp_path, ca_cert),
        server_hostname=BROKER_DNS,
    )
    assert isinstance(err, ssl.SSLCertVerificationError), (
        f"a certificate for a DIFFERENT workload was accepted as the broker (got {err!r}). "
        "It is signed by the trust bundle and has a valid SPIFFE ID, so bundle-only validation "
        "admits it. Every workload in the trust domain can now impersonate the bus."
    )


# ------------------------------------------------------------------- rotation


def test_a_rotated_svid_is_picked_up_without_a_restart(svid_dir, tmp_path: Path):
    """The whole point of reload_if_rotated, proven through a real handshake.

    SPIRE rotates inside the SVID lifetime and spiffe-helper rewrites the PEMs in
    place. Without a reload the context keeps the copy it loaded at startup, and
    the first reconnect after expiry is refused forever.

    Here the CA is REPLACED — the strongest form of the change, since the old
    bundle cannot validate the new broker at all — so a context that failed to
    reload could not possibly pass.
    """
    d, _, _ = svid_dir
    ctx = mtls.client_context(str(d))
    fp = mtls.reload_if_rotated(ctx, str(d), None)

    # A completely new trust root + a broker signed by it.
    new_ca_key, new_ca_cert = _ca()
    ckey, ccert = _svid(new_ca_key, new_ca_cert, "/ns/kanz-services/sa/inference", dns="inference")
    _write(d, ckey, ccert, new_ca_cert)
    skey, scert = _svid(new_ca_key, new_ca_cert, "/ns/kanz-messaging/sa/nats", dns=BROKER_DNS)

    fp2 = mtls.reload_if_rotated(ctx, str(d), fp)
    assert fp2 != fp, "the rotation was not detected — the SVID files changed on disk"

    err = _handshake(ctx, _server_ctx(skey, scert, tmp_path, new_ca_cert), server_hostname=BROKER_DNS)
    assert err is None, (
        f"after rotation the handshake still failed: {err!r}. The context is holding the "
        "certificate it loaded at startup, so the first reconnect after the old SVID expires "
        "is refused — permanently, because nats-py retries with the same dead cert."
    )


def test_an_unchanged_svid_is_not_reloaded(svid_dir):
    """No write, no work — and no unbounded growth of the trust store.

    load_verify_locations ACCUMULATES roots, so a poll loop that reloaded
    unconditionally would grow the context every few minutes for the life of
    the pod.
    """
    d, _, _ = svid_dir
    ctx = mtls.client_context(str(d))
    fp = mtls.reload_if_rotated(ctx, str(d), None)
    assert mtls.reload_if_rotated(ctx, str(d), fp) == fp


def test_a_half_written_svid_does_not_destroy_the_loaded_material(svid_dir):
    """spiffe-helper writes three files, not atomically across all three.

    Catching a poll mid-write must not leave the client with nothing: the
    material already loaded is still valid until expiry, and discarding it would
    turn a transient race into a connection outage.
    """
    d, _, _ = svid_dir
    ctx = mtls.client_context(str(d))
    fp = mtls.reload_if_rotated(ctx, str(d), None)

    (d / mtls.BUNDLE_FILE).unlink()  # mid-write: bundle not there yet
    assert mtls.reload_if_rotated(ctx, str(d), fp) == fp
    assert ctx.check_hostname is True
