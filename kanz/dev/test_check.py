"""Regressions for omitted/untracked files and incomplete Go test streams."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

import check


class FormattingTests(unittest.TestCase):
    def test_untracked_spaces_deleted_and_ignored_files(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            subprocess.run(['git', 'init', '-q', tmp], check=True)
            (root / '.gitignore').write_text('ignored/\n')
            (root / 'deleted.go').write_text('package deleted\n')
            subprocess.run(['git', 'add', '.'], cwd=root, check=True)
            (root / 'deleted.go').unlink()
            (root / 'ignored').mkdir()
            (root / 'ignored' / 'bad.go').write_text('not Go')
            added = root / 'new file.go'
            added.write_text('package sample\nfunc f( ){ }\n')
            with self.assertRaisesRegex(ValueError, 'new file.go'):
                check.formatting(root)
            subprocess.run(['git', 'add', 'new file.go'], cwd=root, check=True)
            with self.assertRaisesRegex(ValueError, 'new file.go'):
                check.formatting(root)
            subprocess.run(['gofmt', '-w', str(added)], check=True)
            check.formatting(root)

    def test_gofmt_failure_is_not_a_clean_result(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            subprocess.run(['git', 'init', '-q', tmp], check=True)
            (root / 'broken.go').write_text('not Go')
            with self.assertRaises(subprocess.CalledProcessError):
                check.formatting(root)


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


if __name__ == '__main__':
    unittest.main()
