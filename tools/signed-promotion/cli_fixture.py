#!/usr/bin/env python3
"""Actual pinned ORAS + Distribution protocol test; loopback and dummy identity only.

No AWS, production registry, OIDC, real signing key or promotion entrypoint runs.
The test-only resolver maps every registry operation to a temporary TLS proxy.
"""
import base64
from contextlib import ExitStack
from http.client import HTTPConnection
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import socket
import ssl
import subprocess
import tempfile
import threading
import time

import promote as p


def port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def stop_backend(backend):
    if backend.poll() is None:
        backend.terminate()
    try:
        backend.wait(timeout=10)
    except subprocess.TimeoutExpired:
        backend.kill()
        backend.wait(timeout=5)
    p.require(backend.poll() is not None, "fixture registry cleanup incomplete")


def stop_proxy(proxy, thread):
    proxy.shutdown()
    thread.join(timeout=5)
    p.require(not thread.is_alive(), "fixture proxy cleanup incomplete")


def fixture_references(args):
    shapes = {
        ("resolve",): (3, (2,)),
        ("repo", "tags"): (4, (3,)),
        ("manifest", "fetch"): (4, (3,)),
        ("manifest", "push"): (5, (3,)),
        ("blob", "fetch", "--output", "-"): (6, (5,)),
        ("discover", "--format", "json", "--depth", "1"): (7, (6,)),
        ("cp", "-r"): (5, (3, 4)),
        ("attach", "--artifact-type", "application/vnd.kanz.fixture"): (6, (4,)),
        ("attach", "--artifact-type", "application/vnd.in-toto+json"): (6, (4,)),
        ("push", "--config", "image.json:application/vnd.oci.image.config.v1+json"): (6, (4,)),
        ("push",): (4, (2,)),
    }
    p.require(args and args[0] == "oras", "fixture accepts only ORAS")
    for prefix, (size, positions) in shapes.items():
        if len(args) == size and tuple(args[1:1 + len(prefix)]) == prefix:
            refs = [args[i] for i in positions]
            pattern = r"ghcr\.io/eighred/fixture-(?:source|destination)(?::[A-Za-z0-9_.-]{1,128}|@sha256:[0-9a-f]{64})?"
            p.require(all(re.fullmatch(pattern, ref) for ref in refs), "nonfixture reference")
            p.require(all(i in positions or args[i] in ("layer", "index.json", "attestation.json", "layer:application/vnd.oci.image.layer.v1.tar")
                          for i in range(1 + len(prefix), len(args))), "unsupported fixture argument")
            return refs
    raise p.Refused("unsupported fixture command")


def fixture():
    p.require(os.name == "posix", "Linux CLI fixture required")
    registry = shutil.which("registry")
    p.require(registry is not None, "pinned Distribution executable required")
    original = p.command
    p.require(b"1.3.0" in original("oras", "version"), "pinned ORAS required")
    p.require(b"3.0.0" in original(registry, "--version"), "pinned Distribution required")
    with tempfile.TemporaryDirectory(prefix="kanz-registry-fixture-") as directory:
        root = Path(directory)
        backend_port, proxy_port = port(), port()
        cert, key = root / "cert.pem", root / "key.pem"
        original("openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                 "-subj", "/CN=ghcr.io", "-addext", "subjectAltName=DNS:ghcr.io",
                 "-keyout", str(key), "-out", str(cert))
        config = root / "registry.yml"
        config.write_text("version: 0.1\nlog:\n  level: error\nstorage:\n  filesystem:\n    rootdirectory: " +
                          str(root / "data") + "\nhttp:\n  addr: 127.0.0.1:" + str(backend_port) +
                          "\n  host: https://ghcr.io\n  secret: disposable-fixture-only\n")
        # Trusted fixture child receives no ambient cloud/GitHub credentials.
        registry_env = {"PATH": os.environ["PATH"], "HOME": str(root), "OTEL_TRACES_EXPORTER": "none"}
        with ExitStack() as resources:
            backend = subprocess.Popen([registry, "serve", str(config)], env=registry_env,
                                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            resources.callback(stop_backend, backend)
            accepted, denied = [], []
            hidden_subject, hidden_referrer = None, None
            hidden_discovery = []
            expected_auth = "Basic " + base64.b64encode(b"x-access-token:fixture-token").decode()

            class Proxy(BaseHTTPRequestHandler):
                def log_message(self, *_args):
                    pass

                def request(self):
                    if self.headers.get("Authorization") != expected_auth:
                        if self.headers.get("Authorization"):
                            denied.append(True)
                        self.send_response(401)
                        self.send_header("WWW-Authenticate", 'Basic realm="disposable-fixture"')
                        self.send_header("Content-Length", "0")
                        self.end_headers()
                        return
                    if not self.path.startswith("/v2/") or self.headers.get("Transfer-Encoding"):
                        self.send_error(400)
                        return
                    count = int(self.headers.get("Content-Length", "0"))
                    if not 0 <= count <= 2 * 1024 * 1024:
                        self.send_error(413)
                        return
                    accepted.append(self.command)
                    headers = {k: v for k, v in self.headers.items()
                               if k.lower() not in ("authorization", "connection", "host")}
                    conn = HTTPConnection("127.0.0.1", backend_port, timeout=10)
                    try:
                        conn.request(self.command, self.path, self.rfile.read(count), headers)
                        response = conn.getresponse()
                        body = response.read(2 * 1024 * 1024 + 1)
                        p.require(len(body) <= 2 * 1024 * 1024, "fixture response bound")
                        changed = False
                        # Reproduce an older source registry's incomplete discovery
                        # index without hiding the content-addressed manifest bytes.
                        hidden_path = "/v2/eighred/fixture-source/referrers/" + (hidden_subject or "")
                        if self.command == "GET" and response.status == 200 and hidden_referrer and self.path.split("?", 1)[0] == hidden_path:
                            index = json.loads(body)
                            entries = index.get("manifests")
                            p.require(isinstance(entries, list), "fixture referrers response shape")
                            kept = [entry for entry in entries if entry.get("digest") != hidden_referrer]
                            if len(kept) != len(entries):
                                p.require(len(entries) - len(kept) == 1, "ambiguous hidden fixture referrer")
                                index["manifests"] = kept
                                body = json.dumps(index, separators=(",", ":")).encode()
                                hidden_discovery.append(True)
                                changed = True
                        self.send_response(response.status)
                        for name, value in response.getheaders():
                            if changed and name.lower() == "etag":
                                continue
                            if changed and name.lower() == "docker-content-digest":
                                value = "sha256:" + hashlib.sha256(body).hexdigest()
                            if name.lower() not in ("transfer-encoding", "connection", "content-length"):
                                self.send_header(name, value)
                        self.send_header("Content-Length", response.getheader("Content-Length", "0") if self.command == "HEAD" else str(len(body)))
                        self.end_headers()
                        if self.command != "HEAD":
                            self.wfile.write(body)
                    finally:
                        conn.close()

                do_GET = do_HEAD = do_POST = do_PATCH = do_PUT = request

            proxy = ThreadingHTTPServer(("127.0.0.1", proxy_port), Proxy)
            resources.callback(proxy.server_close)
            tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            tls.minimum_version = ssl.TLSVersion.TLSv1_2
            tls.load_cert_chain(cert, key)
            proxy.socket = tls.wrap_socket(proxy.socket, server_side=True)
            thread = threading.Thread(target=proxy.serve_forever, daemon=True)
            thread.start()
            resources.callback(stop_proxy, proxy, thread)
            saved_env, saved_cwd = dict(os.environ), Path.cwd()
            try:
                for _ in range(100):
                    p.require(backend.poll() is None, "fixture registry exited")
                    try:
                        with socket.create_connection(("127.0.0.1", backend_port), timeout=.2):
                            break
                    except OSError:
                        time.sleep(.1)
                else:
                    raise p.Refused("fixture registry readiness timeout")
                helper = Path(__file__).resolve().parent
                os.environ.clear()
                os.environ.update({"PATH": str(helper) + os.pathsep + saved_env["PATH"], "HOME": str(root),
                                   "DOCKER_CONFIG": str(root), "PROMOTION_AWS_ACCOUNT": p.ACCOUNT,
                                   "GH_TOKEN": "fixture-token"})
                (root / "config.json").write_text(json.dumps({"credHelpers": {"ghcr.io": "kanz-promotion"}}))
                os.chdir(root)

                def local(*args, **kwargs):
                    fixture_references(args)
                    mapping = "ghcr.io:443:127.0.0.1:" + str(proxy_port)
                    if args[1] == "cp":
                        args += ("--from-resolve", mapping, "--to-resolve", mapping,
                                 "--from-ca-file", str(cert), "--to-ca-file", str(cert))
                    else:
                        args += ("--resolve", mapping, "--ca-file", str(cert))
                    return original(*args, **kwargs)

                p.command = local
                source, target = "ghcr.io/eighred/fixture-source", "ghcr.io/eighred/fixture-destination"
                (root / "layer").write_bytes(b"fixture-only-layer")
                manifests = []
                for arch in ("amd64", "arm64"):
                    (root / "image.json").write_text(json.dumps({"architecture": arch, "os": "linux"}))
                    local("oras", "push", "--config", "image.json:application/vnd.oci.image.config.v1+json",
                          source + ":" + arch, "layer:application/vnd.oci.image.layer.v1.tar")
                    identity = p.resolve(source + ":" + arch)
                    raw = local("oras", "manifest", "fetch", source + "@" + identity)
                    manifests.append({"digest": identity, "size": len(raw), "mediaType": json.loads(raw)["mediaType"],
                                      "platform": {"architecture": arch, "os": "linux"}})
                # This unsigned, dummy in-toto artifact is both an index child
                # and a referrer of a platform manifest. No signing claim is made.
                platform = manifests[0]["digest"]
                (root / "attestation.json").write_text(json.dumps({
                    "_type": "https://in-toto.io/Statement/v1", "predicateType": "https://example.invalid/fixture",
                    "subject": [{"name": "fixture-only", "digest": {"sha256": platform.removeprefix("sha256:")}}],
                    "predicate": {"fixture": True},
                }))
                local("oras", "attach", "--artifact-type", "application/vnd.in-toto+json",
                      source + "@" + platform, "attestation.json")
                visible = p.document("oras", "discover", "--format", "json", "--depth", "1", source + "@" + platform)
                refs = [ref for ref in visible["referrers"] if ref.get("artifactType") == "application/vnd.in-toto+json"]
                p.require(len(refs) == 1, "attestation discovery positive control missing")
                attestation = refs[0]["digest"]
                raw = local("oras", "manifest", "fetch", source + "@" + attestation)
                artifact = json.loads(raw)
                p.require(artifact.get("subject", {}).get("digest") == platform, "attestation subject missing")
                manifests.append({"digest": attestation, "size": len(raw), "mediaType": artifact["mediaType"],
                                  "platform": {"architecture": "unknown", "os": "unknown"}})
                hidden_subject, hidden_referrer = platform, attestation
                invisible = p.document("oras", "discover", "--format", "json", "--depth", "1", source + "@" + platform)
                p.require(hidden_discovery and all(ref["digest"] != attestation for ref in invisible["referrers"]),
                          "source discovery edge was not hidden")
                index = {"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": manifests}
                (root / "index.json").write_text(json.dumps(index, separators=(",", ":")))
                local("oras", "manifest", "push", source + ":multi", "index.json")
                identity = p.resolve(source + ":multi")
                local("oras", "attach", "--artifact-type", "application/vnd.kanz.fixture", source + "@" + identity, "layer")
                legacy = identity.replace(":", "-") + ".sig"
                local("oras", "push", source + ":" + legacy, "layer")
                observed = p.graph(source, identity)
                p.require(len(observed["legacy"]) == 1 and len(observed["edges"][identity]) >= 5, "incomplete fixture graph")
                local("oras", "cp", "-r", source + "@" + identity, target + ":multi")
                for tag, value in observed["legacy"].items():
                    local("oras", "cp", "-r", source + "@" + value, target + ":" + tag)
                source_refs = p.document("oras", "discover", "--format", "json", "--depth", "1", source + "@" + platform)
                target_refs = p.document("oras", "discover", "--format", "json", "--depth", "1", target + "@" + platform)
                p.require(all(ref["digest"] != attestation for ref in source_refs["referrers"])
                          and any(ref["digest"] == attestation for ref in target_refs["referrers"]),
                          "raw source/destination discovery asymmetry missing")
                p.require(attestation in observed["edges"][platform], "verified subject reverse edge missing")
                p.require(p.graph(source, identity) == observed, "source canonical graph changed")
                p.require(p.graph(target, identity) == observed, "actual recursive graph copy mismatch")
                # Canonicalizing a verified subject relation must not conceal a
                # genuine destination-only artifact. Preserve every discovered edge.
                local("oras", "attach", "--artifact-type", "application/vnd.kanz.fixture",
                      target + "@" + platform, "layer")
                p.require(p.graph(target, identity) != observed, "destination-only artifact was ignored")
                p.require(p.graph(source, identity) == observed, "negative control changed the source graph")
                p.require(p.resolve(target + ":multi") == identity and accepted, "actual helper/copy missing")
                before = len(denied)
                os.environ["GH_TOKEN"] = "wrong-fixture-token"
                try:
                    p.resolve(target + ":multi")
                except p.Refused:
                    p.require(len(denied) > before, "wrong credential lacked authenticated 401 evidence")
                else:
                    raise p.Refused("wrong credential unexpectedly accepted")
                os.environ["GH_TOKEN"] = "fixture-token"
                p.require(p.resolve(target + ":multi") == identity, "post-denial positive control failed")
                p.require("fixture-token" not in (root / "config.json").read_text(), "credential persisted")
            finally:
                p.command = original
                os.chdir(saved_cwd)
                os.environ.clear()
                os.environ.update(saved_env)
    print("Actual ORAS 1.3.0 registry/index/asymmetric-referrers/legacy/blob/helper proof passed; no AWS or release signing proof")


if __name__ == "__main__":
    fixture()
