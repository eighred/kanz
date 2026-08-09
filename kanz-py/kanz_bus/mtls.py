"""SPIFFE mTLS material for the Python bus clients (SEC-01c / SEC-M3, #241).

WHY PEM FILES AND NOT THE WORKLOAD API. The Go services talk to the SPIFFE
Workload API socket directly (``pkg/transport`` over go-spiffe) and never hold a
key on disk. ``nats-py`` and ``aiokafka`` cannot do that — they take an
``ssl.SSLContext`` and nothing else. So this service takes the same route the
estate already uses for its other non-Go clients: a ``spiffe-helper`` sidecar
materialises the SVID as PEM files and rotates them in place. That is not a
workaround invented here — the NATS broker itself (``infra/nats/nats.yaml``) and
the bootstrap Job (``infra/nats/bootstrap-job.yaml``, *"the `nats` CLI cannot
speak the SPIFFE Workload API"*) are wired exactly this way, with these file
names.

WHAT AUTHENTICATES THE SERVER, precisely, because getting this wrong is silent.
A SPIFFE SVID identifies its bearer by a **URI SAN**, and Python's ``ssl`` module
verifies **DNS** names. Disabling ``check_hostname`` to "make SPIFFE work" would
leave every cert the trust bundle signed acceptable as the broker — meaning any
workload in the trust domain could impersonate it, which is worse than the
plaintext this replaces because it looks encrypted.

Hostname verification is therefore left ON, and it is sound here because SPIRE
issues DNS SANs for exactly this purpose. ``infra/security/spire/registration.yaml``
sets ``dnsNameTemplates`` to ``{pod}`` and ``{app}.{namespace}.svc`` and says why:
*"SAN DNS names so a peer can also be reached/verified by k8s service DNS
(NATS/Kafka clients ... DNS SANs ease any library that still checks hostnames)."*
SPIRE — not the workload — decides which pod may carry ``nats.kanz-messaging.svc``,
so a DNS SAN under this bundle is as strong a statement as the URI SAN.

The remaining gap against Go is honest and worth stating: go-spiffe additionally
AUTHORIZES the peer's SPIFFE ID (``transport.AuthorizeMesh``), so a Go client
rejects an in-mesh peer that is not the one it meant to reach. This module
authenticates the broker but does not pin its SPIFFE ID.
"""

from __future__ import annotations

import os
import ssl
from dataclasses import dataclass

# The file names spiffe-helper writes. They match infra/nats/nats.yaml and
# infra/nats/bootstrap-job.yaml so one helper config serves every client in the
# estate; changing them here alone would leave the manifest and the reader
# disagreeing, and the symptom is a pod that starts and cannot connect.
SVID_FILE = "svid.pem"
SVID_KEY_FILE = "svid_key.pem"
BUNDLE_FILE = "bundle.pem"


class MissingSVIDError(RuntimeError):
    """Raised when mTLS is configured but the SVID material is not readable.

    This is deliberately fatal rather than a fall-back to plaintext. A client
    that quietly downgrades reports itself healthy and then fails at the broker's
    TLS handshake — or, on a broker that still accepts plaintext, connects
    successfully under NO identity at all, which is the failure this whole change
    exists to remove.
    """


@dataclass(frozen=True)
class SVIDPaths:
    """The three PEM files spiffe-helper maintains in one directory."""

    cert: str
    key: str
    bundle: str

    @classmethod
    def in_dir(cls, cert_dir: str) -> "SVIDPaths":
        return cls(
            cert=os.path.join(cert_dir, SVID_FILE),
            key=os.path.join(cert_dir, SVID_KEY_FILE),
            bundle=os.path.join(cert_dir, BUNDLE_FILE),
        )

    def missing(self) -> list[str]:
        return [p for p in (self.cert, self.key, self.bundle) if not os.path.isfile(p)]


def client_context(cert_dir: str) -> ssl.SSLContext:
    """Build the mTLS client context from the SVID PEMs in ``cert_dir``.

    Raises :class:`MissingSVIDError` if any of the three files is absent — which
    on a real pod means the spiffe-helper sidecar has not produced the first SVID
    yet, or is not running at all.
    """
    if not cert_dir:
        raise MissingSVIDError("mtls: cert_dir is empty")
    paths = SVIDPaths.in_dir(cert_dir)
    if missing := paths.missing():
        raise MissingSVIDError(
            "mtls: SVID material missing: "
            + ", ".join(missing)
            + " — the spiffe-helper sidecar has not written the SVID. Without it this client "
            "has no identity to present and the broker (verify: true) refuses the handshake."
        )

    ctx = ssl.create_default_context(purpose=ssl.Purpose.SERVER_AUTH, cafile=paths.bundle)
    # Both are the defaults for create_default_context and are set again here
    # BECAUSE they are the security property: an SVID chain proves who the peer
    # is, and turning either off silently accepts anything the trust bundle
    # signed (see the module docstring).
    ctx.check_hostname = True
    ctx.verify_mode = ssl.CERT_REQUIRED
    # The client half of mutual TLS — without this the broker's `verify: true`
    # rejects the connection, and `verify_and_map` has no SPIFFE URI SAN to map
    # onto a tenancy.yaml user.
    ctx.load_cert_chain(certfile=paths.cert, keyfile=paths.key)
    return ctx


def _fingerprint(paths: SVIDPaths) -> tuple:
    """Cheap change-detector for the SVID files: (mtime, size) per file."""
    out = []
    for p in (paths.cert, paths.key, paths.bundle):
        try:
            st = os.stat(p)
            out.append((st.st_mtime_ns, st.st_size))
        except OSError:
            out.append(None)
    return tuple(out)


def reload_if_rotated(ctx: ssl.SSLContext, cert_dir: str, last: tuple | None) -> tuple:
    """Re-load ``ctx`` from disk if spiffe-helper has rewritten the SVID.

    Returns the new fingerprint, to be passed back as ``last`` next time.

    WHY THIS EXISTS — the failure it prevents is a total, permanent outage.

    ``load_cert_chain`` reads the certificate ONCE, into the context. SPIRE
    rotates SVIDs well inside their lifetime and spiffe-helper rewrites the PEMs
    in place, but the running context keeps the copy it loaded. Nothing goes wrong
    while the connection stays up — a TLS handshake happens only at connect. Then
    the pod reconnects (a broker restart, a network blip, a rolling update of
    NATS) at some point AFTER the loaded certificate expired, presents it, and is
    refused. ``nats-py`` retries forever, with the same dead certificate, so the
    streaming path never comes back and the only signal is reconnect failures long
    after the rotation that caused them.

    The broker solves this by having spiffe-helper SIGHUP ``nats-server`` on
    renewal; a Python process has no equivalent reload, and restarting the pod on
    every rotation would mean cycling a serving workload roughly hourly. Mutating
    the context in place is the in-process equivalent: the object ``nats-py``
    holds is the object updated, so the NEXT handshake uses the fresh SVID.

    The bundle is re-loaded only when it actually changes, because
    ``load_verify_locations`` ACCUMULATES roots rather than replacing them —
    calling it on every poll would grow the trust store without bound.
    """
    paths = SVIDPaths.in_dir(cert_dir)
    now = _fingerprint(paths)
    if now == last:
        return now
    if paths.missing():
        # Mid-write, or the helper died. Keep the material already loaded — it is
        # still valid until expiry — and report no change so the next poll retries.
        return last if last is not None else now

    ctx.load_cert_chain(certfile=paths.cert, keyfile=paths.key)
    if last is None or now[2] != last[2]:
        ctx.load_verify_locations(cafile=paths.bundle)
    return now
