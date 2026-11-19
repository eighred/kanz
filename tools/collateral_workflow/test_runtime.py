import json
import subprocess
import unittest
from unittest.mock import patch

from runtime import Estate, docker, nats_configuration


class RuntimeTest(unittest.TestCase):
    def test_broker_requires_an_authenticated_jetstream_response(self):
        estate = Estate('.', '.')
        for raw, ready in [('{}', False), ('', False),
                           ('{"type":"io.nats.jetstream.api.v1.account_info_response","error":{"code":503}}', False),
                           ('{"type":"io.nats.jetstream.api.v1.account_info_response","streams":0}', True)]:
            with self.subTest(raw=raw), patch.object(estate, 'invoke', return_value=raw):
                self.assertEqual(estate.broker_ready({}), ready)

    def test_reservation_evidence_requires_the_exact_amount(self):
        estate = Estate('.', '.')
        changed = json.dumps(['rehearsal', 'release-workflow', 'CSA1', 'lot-1', '59'])
        with patch.object(estate, 'observed', return_value=changed):
            with self.assertRaisesRegex(ValueError, 'reserved obligation differs'):
                estate.ledger(['a' * 64, 'b' * 64])

    def test_fixture_publishers_and_observer_are_scoped(self):
        config = nats_configuration()
        for user, subject in [('feed', 'accounting.collateral.snapshot'),
                              ('custody', 'accounting.collateral.confirmation.custody')]:
            line, = [s for s in config.splitlines() if s.startswith('{user: ' + user + ',')]
            grants = json.loads(line.split('permissions: ', 1)[1][:-2])
            self.assertEqual(grants, {'publish': {'allow': [subject]}, 'subscribe': {'allow': ['_INBOX.>']}})
        observer, = [s for s in config.splitlines() if s.startswith('{user: observer,')]
        api = json.loads(observer.split('permissions: ', 1)[1][:-1])['publish']['allow']
        self.assertCountEqual(api, ['$JS.API.STREAM.INFO.ACCOUNTING', '$JS.API.STREAM.MSG.GET.ACCOUNTING',
                                    '$JS.API.CONSUMER.LIST.ACCOUNTING',
                                    '$JS.API.DIRECT.GET.ACCOUNTING.accounting.collateral.snapshot',
                                    '$JS.API.DIRECT.GET.ACCOUNTING.accounting.collateral.recorded',
                                    '$JS.API.DIRECT.GET.ACCOUNTING.accounting.collateral.confirmation.custody'])

    def test_diagnostics_do_not_disclose_output(self):
        with patch('runtime.subprocess.run') as run:
            run.return_value = subprocess.CompletedProcess([], 1, stdout='secret', stderr='private DSN')
            with self.assertRaisesRegex(RuntimeError, '^approve exited 1$'):
                docker('approve', 'run')
            run.return_value.stderr = 'gateway approve returned 409, expected 200'
            with self.assertRaisesRegex(RuntimeError, '^approve: gateway approve returned 409, expected 200$'):
                docker('approve', 'run')
