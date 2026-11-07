import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import consume as c


class Consumer(unittest.TestCase):
    def claims(self):
        return [{"critical": {"type": "https://sigstore.dev/cosign/sign/v1", "image": {"docker-manifest-digest": c.DIGEST},
                 "identity": {"docker-reference": c.IMAGE}},
                 "optional": {"source_sha": c.PRODUCER_SOURCE}}]

    def test_signature_bindings(self):
        c.signature_claims(json.dumps(self.claims()))
        for path, replacement in (
                (("critical", "image", "docker-manifest-digest"), "sha256:" + "0" * 64),
                (("critical", "identity", "docker-reference"), c.q.REPOSITORY),
                (("critical", "identity", "docker-reference"), "ghcr.io/other/tools@" + c.DIGEST),
                (("critical", "identity", "docker-reference"), c.q.REPOSITORY + "@sha256:" + "0" * 64),
                (("critical", "type"), "cosign container image signature"),
                (("critical", "type"), "unreviewed"),
                (("optional", "source_sha"), "0" * 40)):
            claims = self.claims()
            target = claims[0]
            for key in path[:-1]: target = target[key]
            target[path[-1]] = replacement
            with self.assertRaises(ValueError): c.signature_claims(json.dumps(claims))
        for raw in ('[]', '{}', '[{"optional":{"source_sha":"a","source_sha":"b"}}]'):
            with self.assertRaises(ValueError): c.signature_claims(raw)

    def test_invalid_run_refuses_before_signature(self):
        with tempfile.TemporaryDirectory() as root, patch.object(c.q, 'run') as run:
            for value in ('0', '-1', 'bad', '1' * 21):
                with self.assertRaises(ValueError): c.install(Path(root), 'b' * 40, value, '1')
            run.assert_not_called()

    def test_signature_failure_cannot_pull_extract_or_execute(self):
        with tempfile.TemporaryDirectory() as root, patch.object(c.q, 'run', side_effect=RuntimeError('refused')) as run, patch.object(c.q, 'extract_saved') as extract, patch.object(c.q, 'exercise') as execute:
            with self.assertRaises(RuntimeError): c.install(Path(root), 'b' * 40, '123', '2')
            self.assertEqual(run.call_count, 1)
            self.assertEqual(run.call_args.args[0], ['cosign', 'verify', '--certificate-oidc-issuer', c.q.ISSUER, '--certificate-identity', c.q.SIGNER, '-a', 'source_sha=' + c.PRODUCER_SOURCE, c.IMAGE])
            extract.assert_not_called(); execute.assert_not_called()

    def test_install_order_and_distinct_source(self):
        stages = []
        def run(args, **kwargs):
            stages.append(args[:2])
            if args[0] == 'cosign': kwargs['stdout'].write(json.dumps(self.claims()).encode())
            if args[:2] == ['docker', 'save']: Path(args[3]).write_bytes(b'archive')
        def extract(raw, config, directory, source):
            self.assertEqual((raw, config, source), (b'archive', c.CONFIG, c.PRODUCER_SOURCE))
            self.assertEqual(stages, [['cosign','verify'],['docker','pull'],['docker','tag'],['docker','save']])
            stages.append(['extract'])
            for name in c.q.TOOLS: (directory / name).write_bytes(b'public tool')
            return config, {'tools/cargo-audit': 'hash'}
        def exercise(directory):
            self.assertEqual(stages[-1], ['extract']); stages.append(['execute']); return ['a' * 40] * 2
        # Parser/control tests use immutable bytes. They do not emulate or waive
        # the Linux nofollow/nonblocking opener, separately tested by producer.
        with tempfile.TemporaryDirectory() as root, patch.object(c.q, 'run', side_effect=run), patch.object(c.q, 'read_bounded', side_effect=lambda p, limit: p.read_bytes()), patch.object(c.q, 'extract_saved', side_effect=extract), patch.object(c.q, 'exercise', side_effect=exercise):
            installed = c.install(Path(root), 'b' * 40, '123', '2')
            self.assertEqual({p.name for p in installed.iterdir()}, set(c.q.TOOLS))
            receipt = json.loads((Path(root) / 'rust-audit-tools-installed.json').read_text())
            self.assertEqual((receipt['run'], receipt['attempt']), ('123', '2'))
            self.assertEqual(receipt['consumer_source'], 'b' * 40)
            self.assertEqual(receipt['producer_source'], c.PRODUCER_SOURCE)
            self.assertNotEqual(receipt['consumer_source'], receipt['producer_source'])
            self.assertEqual(stages[-1], ['execute'])

    def test_config_failure_cannot_execute(self):
        with tempfile.TemporaryDirectory() as root, patch.object(c.q, 'run'), patch.object(c.q, 'read_bounded', side_effect=[json.dumps(self.claims()).encode(), b'bad archive']), patch.object(c.q, 'extract_saved', side_effect=ValueError('config mismatch')), patch.object(c.q, 'exercise') as execute:
            with self.assertRaises(ValueError): c.install(Path(root), 'b' * 40, '123', '2')
            execute.assert_not_called()
            self.assertFalse((Path(root) / 'rust-audit-tools-installed.json').exists())


if __name__ == '__main__': unittest.main()
