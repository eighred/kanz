"""Watch refusals and local HTTP fixture checks; no Docker or Prometheus runs."""

import contextlib
import http.client
import io
import json
from pathlib import Path
import subprocess
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

import kube_fixture
import qualify


COMPLETE = {"initial_events_requested": True, "initial_bookmark_sent": True}
EMPTY = {"initial_events_requested": False, "initial_bookmark_sent": False}


class WatchStatus(unittest.TestCase):
    def read(self, raw):
        result = subprocess.CompletedProcess([], 0, raw, b"")
        with patch.object(qualify, "command", return_value=result):
            return qualify.watch_status("owned-fixture")

    def test_complete_boolean_status_is_required(self):
        self.assertEqual(self.read(json.dumps(COMPLETE).encode()), COMPLETE)

    def test_absent_malformed_and_oversized_status_refuse(self):
        for raw in [b"", b"{", b"x" * 4097, b"[]", b"true", b"null"]:
            with self.subTest(raw=raw[:20]), self.assertRaises(ValueError):
                self.read(raw)

    def test_missing_false_nonboolean_and_unknown_fields_refuse(self):
        cases = [EMPTY, {"initial_events_requested": True}, dict(COMPLETE, extra=True)]
        for name in COMPLETE:
            for value in [False, None, 0, 1, "true", [], {}]:
                cases.append(dict(COMPLETE, **{name: value}))
        for value in cases:
            with self.subTest(value=value), self.assertRaises(ValueError):
                self.read(json.dumps(value).encode())

    def test_duplicate_status_keys_refuse(self):
        raw = (b'{"initial_events_requested":false,"initial_events_requested":true,'
               b'"initial_bookmark_sent":true}')
        with self.assertRaisesRegex(ValueError, "duplicate"):
            self.read(raw)


class RuntimeControlFlow(unittest.TestCase):
    """Mocked Docker responses test enforcement placement, not OCI behavior."""

    def exercise(self, directory, states, image="candidate", lose_history=False):
        calls = []
        statuses = iter(states)

        def command(*args, **_kwargs):
            calls.append(args)
            if args[0] == "openssl":
                Path(args[args.index("-keyout") + 1]).write_bytes(b"dummy-key")
                Path(args[args.index("-out") + 1]).write_bytes(b"dummy-cert")
            if args[:2] in [("docker", "inspect"), ("docker", "volume")]:
                if args[1] == "inspect" or "inspect" in args:
                    return subprocess.CompletedProcess(args, 1, b"", b"")
            raw = json.dumps(next(statuses)).encode() if args[-1] == "status" else b""
            return subprocess.CompletedProcess(args, 0, raw, b"")

        def query(_api, expression, at=None):
            if expression == "vector(1)":
                return [{"value": [100, "1"]}]
            if at is not None and lose_history:
                return []
            return [{"value": [100, "1"]}]

        self.calls = calls
        with patch.object(qualify, "command", side_effect=command), \
                patch.object(qualify, "query", side_effect=query), \
                patch.object(qualify, "probe", return_value="Ready"), \
                patch.object(qualify, "wait_for", side_effect=lambda check: check()):
            return qualify.runtime(image, "helper", Path(directory), "unit")

    def assert_cleanup(self):
        for name in ["prometheus-unit-api", "prometheus-unit", "prometheus-unit-holder"]:
            self.assertIn(("docker", "rm", "-f", "-v", name), self.calls)
        self.assertIn(("docker", "volume", "rm", "prometheus-unit-data"), self.calls)

    def test_both_images_require_fresh_protocol_after_restart(self):
        for image in ["upstream", "candidate"]:
            with self.subTest(image=image), tempfile.TemporaryDirectory() as directory:
                result = self.exercise(directory, [COMPLETE, COMPLETE], image)
                self.assertEqual(result["watch_protocol"], COMPLETE)
                self.assertEqual(result["watch_protocol_after_restart"], COMPLETE)
                self.assertTrue(result["retained_history_after_restart"])
                status_calls = [i for i, call in enumerate(self.calls) if call[-1] == "status"]
                restart = self.calls.index(("docker", "start", "prometheus-unit"))
                self.assertEqual(len(status_calls), 2)
                self.assertLess(status_calls[0], restart)
                self.assertGreater(status_calls[1], restart)
                self.assert_cleanup()

    def test_incomplete_initial_watch_refuses_and_cleans(self):
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaises(ValueError):
                self.exercise(directory, [EMPTY])
            self.assertNotIn(("docker", "start", "prometheus-unit"), self.calls)
            self.assert_cleanup()

    def test_incomplete_restarted_watch_refuses_and_cleans(self):
        for field in COMPLETE:
            with self.subTest(field=field), tempfile.TemporaryDirectory() as directory:
                with self.assertRaises(ValueError):
                    self.exercise(directory, [COMPLETE, dict(COMPLETE, **{field: False})])
                self.assertIn(("docker", "start", "prometheus-unit"), self.calls)
                self.assert_cleanup()

    def test_history_loss_remains_a_refusal(self):
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(ValueError, "pre-restart observation"):
                self.exercise(directory, [COMPLETE, COMPLETE], lose_history=True)
            self.assert_cleanup()


@contextlib.contextmanager
def server():
    instance = kube_fixture.PodServer(("127.0.0.1", 0))
    thread = threading.Thread(target=instance.serve_forever,
                              kwargs={"poll_interval": .01}, daemon=True)
    thread.start()
    try:
        yield instance
    finally:
        instance.shutdown()
        instance.server_close()
        thread.join(timeout=2)
        if thread.is_alive():
            raise AssertionError("owned HTTP fixture survived shutdown")


def get(instance, path):
    connection = http.client.HTTPConnection(*instance.server_address, timeout=2)
    try:
        connection.request("GET", path)
        response = connection.getresponse()
        raw = response.read(4097)
        if response.status != 200 or len(raw) > 4096:
            raise AssertionError("unexpected fixture response")
        return raw
    finally:
        connection.close()


class HTTPFixture(unittest.TestCase):
    def test_initial_stream_has_added_and_end_bookmark(self):
        with server() as instance:
            self.assertEqual(json.loads(get(instance, "/fixture-status")), EMPTY)
            raw = get(instance, "/api/v1/pods?watch=true&sendInitialEvents=true")
            events = [json.loads(line) for line in raw.splitlines()]
            self.assertEqual([event["type"] for event in events], ["ADDED", "BOOKMARK"])
            self.assertEqual(events[-1]["object"]["metadata"]["annotations"],
                             {"k8s.io/initial-events-end": "true"})
            end = time.monotonic() + 1
            while time.monotonic() < end:
                state = json.loads(get(instance, "/fixture-status"))
                if state == COMPLETE:
                    break
                time.sleep(.005)
            self.assertEqual(state, COMPLETE)

    def test_list_and_noninitial_watch_cannot_satisfy_protocol(self):
        with server() as instance:
            listing = json.loads(get(instance, "/api/v1/pods"))
            self.assertEqual(listing["kind"], "PodList")
            events = [json.loads(line) for line in get(instance, "/api/v1/pods?watch=true").splitlines()]
            self.assertEqual([event["type"] for event in events], ["ADDED"])
            self.assertEqual(json.loads(get(instance, "/fixture-status")), EMPTY)

    def test_ambiguous_watch_query_does_not_claim_initial_events(self):
        with server() as instance:
            get(instance, "/api/v1/pods?watch=true&watch=false&sendInitialEvents=true")
            self.assertEqual(json.loads(get(instance, "/fixture-status")), EMPTY)

    def test_new_server_cannot_inherit_another_servers_watch(self):
        with server() as previous, server() as restarted:
            with previous.watch_lock:
                previous.watch_status.update(COMPLETE)
            self.assertEqual(json.loads(get(previous, "/fixture-status")), COMPLETE)
            self.assertEqual(json.loads(get(restarted, "/fixture-status")), EMPTY)


class WriteBoundary(unittest.TestCase):
    def handler(self, output):
        handler = kube_fixture.Pods.__new__(kube_fixture.Pods)
        handler.path = "/api/v1/pods?watch=true&sendInitialEvents=true"
        handler.server = type("State", (), {"watch_lock": threading.Lock(),
                                           "watch_status": dict(EMPTY)})()
        handler.wfile = output
        handler.send_response = lambda *_args: None
        handler.send_header = lambda *_args: None
        handler.end_headers = lambda: None
        return handler

    def test_failed_write_never_claims_bookmark_sent(self):
        class Failed(io.BytesIO):
            def write(self, _raw):
                raise BrokenPipeError("dummy disconnected reader")
        handler = self.handler(Failed())
        with self.assertRaises(BrokenPipeError):
            handler.do_GET()
        self.assertEqual(handler.server.watch_status,
                         dict(EMPTY, initial_events_requested=True))

    def test_failed_flush_never_claims_bookmark_sent(self):
        class Failed(io.BytesIO):
            def flush(self):
                raise OSError("dummy flush failure")
        handler = self.handler(Failed())
        with self.assertRaises(OSError):
            handler.do_GET()
        self.assertFalse(handler.server.watch_status["initial_bookmark_sent"])

    def test_partial_write_never_claims_bookmark_sent(self):
        class Short(io.BytesIO):
            def write(self, raw):
                return super().write(raw[:-1])
        handler = self.handler(Short())
        with self.assertRaisesRegex(OSError, "incomplete"):
            handler.do_GET()
        self.assertFalse(handler.server.watch_status["initial_bookmark_sent"])

    def test_success_is_recorded_only_after_flush(self):
        seen = []
        class Observed(io.BytesIO):
            def flush(self):
                seen.append(handler.server.watch_status["initial_bookmark_sent"])
                return super().flush()
        handler = self.handler(Observed())
        handler.do_GET()
        self.assertEqual(seen, [False])
        self.assertEqual(handler.server.watch_status, COMPLETE)


if __name__ == "__main__":
    unittest.main()
