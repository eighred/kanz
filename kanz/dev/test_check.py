"""Serial accounting, causal timeout and fixed-origin readiness regressions."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
import urllib.error
import urllib.request
from unittest.mock import Mock, patch

import check


class AccountingTests(unittest.TestCase):
    def run_fixture(self, mode):
        packages = [f'github.com/eighred/kanz/fixture/p{i}' for i in range(21)]
        calls = []

        def run(command, **kwargs):
            calls.append(command)
            self.assertIn('-race', command)
            self.assertEqual(command[command.index('-p') + 1], '1')
            self.assertEqual(command[command.index('-count=1')], '-count=1')
            batch = command[command.index('-json') + 1:]
            self.assertLessEqual(len(batch), 20)
            for package in batch:
                kwargs['stdout'].write(json.dumps({'Action': 'start', 'Package': package}) + '\n')
                if mode != 'truncated':
                    kwargs['stdout'].write(json.dumps({'Action': 'pass', 'Package': package}) + '\n')
            return subprocess.CompletedProcess(command, 1 if mode == 'failure' else 0)

        with patch.object(check.subprocess, 'check_output', return_value='\n'.join(packages)), \
                patch.object(check, 'run_tests', side_effect=run):
            check.test_packages(['./...'], race=True)
        return calls

    def test_complete_serial_batches(self):
        self.assertEqual(len(self.run_fixture('complete')), 2)

    def test_success_exit_with_truncated_results_is_refused(self):
        with self.assertRaisesRegex(ValueError, 'missing/truncated'):
            self.run_fixture('truncated')

    def test_failed_process_with_pass_events_is_refused(self):
        with self.assertRaisesRegex(ValueError, 'exit 1'):
            self.run_fixture('failure')


class TimeoutTests(unittest.TestCase):
    @unittest.skipUnless(os.name == 'posix', 'POSIX process-group fixture')
    def test_timeout_stops_child_before_it_can_write(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            marker = root / 'orphan-wrote'
            ready = root / 'ready'
            child = ('import pathlib,time; '
                     f'pathlib.Path({str(ready)!r}).touch(); '
                     'time.sleep(2); '
                     f'pathlib.Path({str(marker)!r}).touch()')
            parent = ('import subprocess,sys,time; '
                      f'subprocess.Popen([sys.executable,"-c",{child!r}]); '
                      'time.sleep(60)')
            with open(os.devnull, 'w') as output:
                with self.assertRaises(subprocess.TimeoutExpired):
                    check.run_tests([sys.executable, '-c', parent], cwd=root,
                                    stdout=output, timeout=1)
            self.assertTrue(ready.exists(), 'child must have started to exercise cleanup')
            time.sleep(2)
            self.assertFalse(marker.exists(), 'timed-out child survived the Go parent')


class ReadinessTests(unittest.TestCase):
    def setUp(self):
        spec = importlib.util.spec_from_file_location(
            'dev_readiness', Path(__file__).with_name('wait-ready.py'))
        self.readiness = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.readiness)
        self.client = Mock()

        def ready(url, *, timeout):
            self.assertEqual(timeout, 2)
            response = Mock(status=200)
            response.geturl.return_value = url
            context = Mock()
            context.__enter__ = Mock(return_value=response)
            context.__exit__ = Mock(return_value=False)
            return context

        self.client.open.side_effect = ready

    def test_only_two_fixed_endpoints_are_queried(self):
        self.readiness.wait_for_services(self.client)
        self.assertEqual(
            [call.args[0] for call in self.client.open.call_args_list],
            ['http://127.0.0.1:8082/readyz', 'http://127.0.0.1:8080/readyz'])

    def test_client_disables_proxy_and_redirect_handlers(self):
        with patch.dict(os.environ, {'http_proxy': 'http://198.51.100.1:3128'}), \
                patch.object(self.readiness.urllib.request, 'build_opener',
                             return_value=self.client) as build:
            self.readiness.wait_for_services()
        handlers = build.call_args.args
        self.assertEqual(len(handlers), 2)
        self.assertIsInstance(handlers[0], urllib.request.ProxyHandler)
        self.assertEqual(handlers[0].proxies, {})
        self.assertIsInstance(handlers[1], self.readiness.NoRedirect)

    def test_redirect_handler_refuses_local_and_foreign_destinations(self):
        request = urllib.request.Request('http://127.0.0.1:8082/readyz')
        for destination in ('/readyz', 'http://127.0.0.1:8082/readyz',
                            'http://127.0.0.1:9000/readyz', 'https://example.invalid/readyz'):
            with self.subTest(destination=destination):
                self.assertIsNone(self.readiness.NoRedirect().redirect_request(
                    request, None, 302, 'Found', {}, destination))

    def test_http_redirect_is_a_fatal_refusal(self):
        for code in (301, 302, 303, 307, 308):
            with self.subTest(code=code):
                self.client.open.side_effect = urllib.error.HTTPError(
                    'http://127.0.0.1:8082/readyz', code, 'redirect', {}, None)
                with self.assertRaisesRegex(ValueError, 'redirect refused'):
                    self.readiness.wait_for_services(self.client)

    def test_changed_response_origin_is_refused(self):
        for destination in ('http://127.0.0.1:9000/readyz', 'https://example.invalid/readyz'):
            with self.subTest(destination=destination):
                response = Mock(status=200)
                response.geturl.return_value = destination
                context = Mock()
                context.__enter__ = Mock(return_value=response)
                context.__exit__ = Mock(return_value=False)
                self.client.open.side_effect = None
                self.client.open.return_value = context
                with self.assertRaisesRegex(ValueError, 'fixed local endpoint'):
                    self.readiness.wait_for_services(self.client)

    def test_unready_endpoints_time_out(self):
        with patch.object(self.readiness.time, 'monotonic', side_effect=(0, 121)):
            with self.assertRaisesRegex(ValueError, 'readiness timed out'):
                self.readiness.wait_for_services(self.client)
        self.client.open.assert_not_called()


if __name__ == '__main__':
    unittest.main()
