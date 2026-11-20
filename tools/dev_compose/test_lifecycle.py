import os
import subprocess
import unittest
from unittest.mock import patch

with patch.dict(os.environ, COMPOSE_PROJECT_NAME='kanzdev0123456789ab'):
    import lifecycle


class ObservationTests(unittest.TestCase):
    def test_diagnostics_exclude_child_streams_arguments_and_environment(self):
        marker = 'synthetic-sensitive-test-value'
        failed = subprocess.CompletedProcess([], 13, marker, marker)
        with patch.object(subprocess, 'run', return_value=failed):
            with self.assertRaisesRegex(RuntimeError, '^go exited 13$'):
                lifecycle.run('go', marker, env={'UNUSED': marker})

    def test_database_credentials_never_enter_child_arguments(self):
        marker = 'synthetic-sensitive-test-value'
        with patch.object(lifecycle, 'run', return_value='') as run:
            lifecycle.developer('go', 'test', extra={'TEST_POSTGRES_URL': marker})
            self.assertNotIn(marker, repr(run.call_args.args))
            self.assertEqual(run.call_args.kwargs['env']['TEST_POSTGRES_URL'], marker)
            lifecycle.psql('SELECT 1', marker)
            self.assertNotIn(marker, repr(run.call_args.args))
            self.assertEqual(run.call_args.kwargs['env']['PGPASSWORD'], marker)
            self.assertEqual(run.call_args.kwargs['data'], 'SELECT 1')

    def test_unlabeled_network_and_changed_builtin_identity_refuse(self):
        original = {'bridge': 'a', 'host': 'b', 'none': 'c'}
        for current in ({**original, 'unknown': 'd'}, {**original, 'bridge': 'd'}):
            with patch.object(lifecycle, 'run', return_value=''), patch.object(lifecycle, 'networks', return_value=current):
                with self.assertRaisesRegex(ValueError, 'unexpected guest'):
                    lifecycle.stopped(original)
