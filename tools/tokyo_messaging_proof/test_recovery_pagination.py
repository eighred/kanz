"""Exercise the production selector with the AWS CLI paginator on loopback."""

import os
import shutil
import subprocess
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlsplit


ROOT = Path(__file__).resolve().parents[2]
FIRST = "testnet/data-plane/20261118T010000Z-123456789abc/bundle.tar.gz"
SECOND = "testnet/data-plane/20261119T010000Z-fedcba987654/bundle.tar.gz"


class RecoveryPagination(unittest.TestCase):
    def select(self, second_time, expected, broken_page=False):
        tokens = []

        class Listing(BaseHTTPRequestHandler):
            def do_GET(self):
                url = urlsplit(self.path)
                query = parse_qs(url.query)
                token = query.get("continuation-token", [""])[0]
                tokens.append(token)
                if (url.path != "/fixture-bucket" or query.get("list-type") != ["2"]
                        or query.get("prefix") != ["testnet/data-plane/"] or token not in ("", "page2")):
                    self.send_error(400)
                    return
                if token and broken_page:
                    self.send_error(503)
                    return
                key, stamp = (SECOND, second_time) if token else (FIRST, "2026-11-19T01:00:00.000Z")
                pagination = ("<IsTruncated>false</IsTruncated>" if token else
                              "<IsTruncated>true</IsTruncated><NextContinuationToken>page2</NextContinuationToken>")
                body = ('<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">'
                        f"{pagination}<Contents><Key>{key}</Key><LastModified>{stamp}</LastModified>"
                        f"</Contents></ListBucketResult>").encode()
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args):
                pass

        with HTTPServer(("127.0.0.1", 0), Listing) as server:
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                env = {key: value for key, value in os.environ.items() if not key.startswith("AWS_")}
                env.update(FIXTURE_ENDPOINT=f"http://127.0.0.1:{server.server_port}", AWS_CONFIG_FILE=os.devnull,
                           AWS_SHARED_CREDENTIALS_FILE=os.devnull, AWS_MAX_ATTEMPTS="1", AWS_PAGER="",
                           AWS_EC2_METADATA_DISABLED="true", NO_PROXY="127.0.0.1")
                run = subprocess.run(
                    [shutil.which("bash") or "bash", "-c", """
                        source tools/testnet-data-plane-recovery.sh restore fixture-bucket fixture-kms
                        aws() { command aws --endpoint-url "$FIXTURE_ENDPOINT" --no-sign-request "$@"; }
                        latest_recovery_key
                    """],
                    cwd=ROOT, capture_output=True, text=True, timeout=30, env=env,
                )
            finally:
                server.shutdown()
                thread.join()
        self.assertEqual(tokens, ["", "page2"], run.stderr)
        if expected is None:
            self.assertNotEqual(run.returncode, 0)
            self.assertEqual(run.stdout, "")
        else:
            self.assertEqual(run.returncode, 0, run.stderr)
            self.assertEqual(run.stdout.splitlines(), [expected])

    def test_global_latest(self):
        self.select("2026-11-18T01:00:00.000Z", FIRST)

    def test_newest_on_second_page(self):
        self.select("2026-11-19T01:00:00.001Z", SECOND)

    def test_tied_time_uses_greatest_key(self):
        self.select("2026-11-19T01:00:00.000Z", SECOND)

    def test_failure_reading_second_page_refuses(self):
        self.select("2026-11-20T01:00:00.000Z", None, broken_page=True)
