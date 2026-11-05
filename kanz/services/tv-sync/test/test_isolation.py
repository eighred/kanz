"""Offline trust-boundary tests; no Docker, registry, or cluster calls."""
import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import isolation

IMAGE = 'ghcr.io/eighred/tv-sync@sha256:' + 'a' * 64
MIGRATION_IMAGE = 'ghcr.io/eighred/kanz-migrate@sha256:' + 'c' * 64
SHA = 'b' * 40


class InputTests(unittest.TestCase):
    def test_digest_and_reference_validation(self):
        isolation.validate_inputs(IMAGE, 'v2026.11.1', SHA)
        isolation.validate_inputs(MIGRATION_IMAGE, 'v2026.11.1', SHA, 'kanz-migrate')
        with self.assertRaises(ValueError):
            isolation.validate_inputs(IMAGE, 'v1', SHA, 'kanz-migrate')
        for image, tag, sha in [
            ('ghcr.io/eighred/tv-sync:latest', 'v1', SHA),
            (IMAGE.replace('tv-sync', 'other'), 'v1', SHA),
            (IMAGE, 'v1; touch stolen', SHA),
            (IMAGE, '--help', SHA),
            (IMAGE, 'v1', SHA + '\n--help'),
        ]:
            with self.subTest(image=image, tag=tag, sha=sha):
                with self.assertRaises(ValueError):
                    isolation.validate_inputs(image, tag, sha)

    @patch('isolation.run')
    def test_tag_mismatch_stops_before_signature_or_execution(self, run):
        run.return_value = 'c' * 40
        with self.assertRaisesRegex(RuntimeError, 'tag does not resolve'):
            isolation.verify_release(IMAGE, 'v1', SHA)
        self.assertEqual(run.call_count, 1)

    @patch('isolation.run')
    def test_every_signature_and_attestation_binds_sha_and_tag(self, run):
        run.return_value = SHA
        isolation.verify_release(IMAGE, 'v1', SHA)
        isolation.verify_release(MIGRATION_IMAGE, 'v1', SHA, 'kanz-migrate')
        calls = [c.args for c in run.call_args_list if c.args[0] == 'cosign']
        self.assertEqual(len(calls), 6)
        for image in (IMAGE, MIGRATION_IMAGE):
            image_calls = [c for c in calls if image in c]
            self.assertEqual([c[1] for c in image_calls], ['verify', 'verify-attestation', 'verify-attestation'])
            self.assertEqual([c[c.index('--type') + 1] for c in image_calls[1:]], ['spdxjson', 'vuln'])
        for call in calls:
            self.assertTrue(IMAGE in call or MIGRATION_IMAGE in call)
            self.assertEqual(call[call.index('--certificate-github-workflow-sha') + 1], SHA)
            self.assertEqual(call[call.index('--certificate-identity') + 1],
                             'https://github.com/eighred/kanz/.github/workflows/release.yml@refs/tags/v1')


class CandidateTests(unittest.TestCase):
    def setUp(self):
        self.deployment = {'metadata':{'name':'tv-sync','namespace':'kanz-services'}, 'spec': {'replicas':1, 'strategy':{'type':'Recreate'}, 'template': {'spec': {
            'containers': [{'name': 'tv-sync', 'image': IMAGE, 'ports':[{'containerPort':8091},{'containerPort':8113}], 'env': [
                {'name': 'TV_SYNC_LISTEN', 'value': ':8091'},
                {'name': 'TV_SYNC_API_LISTEN', 'value': ':8113'},
            ]}], 'initContainers': [{'name': 'migrate', 'image': MIGRATION_IMAGE}],
        }}}}
        self.service = {'metadata':{'name':'tv-sync','namespace':'kanz-services'}, 'spec': {'ports': [{'port': 8113, 'targetPort': 8113}]}}

    def test_valid_split(self):
        isolation.candidate_listener(self.deployment, self.service, IMAGE, MIGRATION_IMAGE)

    def test_changed_namespace_is_not_silently_rewritten(self):
        self.deployment['metadata']['namespace']='different-estate'
        with self.assertRaisesRegex(RuntimeError,'resource identity'):
            isolation.candidate_listener(self.deployment,self.service,IMAGE,MIGRATION_IMAGE)

    def test_replica_and_rollout_shape_cannot_be_silently_rewritten(self):
        for change in ({'replicas':2}, {'replicas':0}, {'strategy':{'type':'RollingUpdate'}}, {'strategy':{}}):
            with self.subTest(change=change):
                doc=copy.deepcopy(self.deployment)
                doc['spec'].update(change)
                with self.assertRaisesRegex(RuntimeError,'singleton'):
                    isolation.candidate_listener(doc,self.service,IMAGE,MIGRATION_IMAGE)

    def test_different_immutable_migration_digest_is_refused(self):
        self.deployment['spec']['template']['spec']['initContainers'][0]['image'] = (
            'ghcr.io/eighred/kanz-migrate@sha256:' + 'd' * 64)
        with self.assertRaisesRegex(RuntimeError, 'migration image differs'):
            isolation.candidate_listener(self.deployment, self.service, IMAGE, MIGRATION_IMAGE)

    def test_dangerous_candidate_shapes_refused(self):
        mutations = {
            'host port': lambda spec: spec['containers'][0]['ports'][0].update(hostPort=8091),
            'host namespace': lambda spec: spec.update(hostNetwork=True),
            'host mount': lambda spec: spec.update(volumes=[{'name': 'host', 'hostPath': {'path': '/'}}]),
            'sidecar': lambda spec: spec['containers'].append({'name': 'unreviewed'}),
            'extra init': lambda spec: spec['initContainers'].append({'name': 'unreviewed'}),
            'privileged': lambda spec: spec['containers'][0].update(securityContext={'privileged': True}),
            'migration substitution': lambda spec: spec['initContainers'][0].update(image='ghcr.io/eighred/kanz-migrate:latest'),
            'image substitution': lambda spec: spec['containers'][0].update(image='attacker:latest'),
            'blank listener': lambda spec: spec['containers'][0]['env'][1].update(value=''),
        }
        for name, mutate in mutations.items():
            with self.subTest(name=name):
                doc = copy.deepcopy(self.deployment)
                mutate(doc['spec']['template']['spec'])
                with self.assertRaises(RuntimeError):
                    isolation.candidate_listener(doc, self.service, IMAGE, MIGRATION_IMAGE)


class InventoryTests(unittest.TestCase):
    def make_inventory(self, root):
        infra = root / 'infra'
        infra.mkdir()
        policy = {'apiVersion': 'networking.k8s.io/v1', 'kind': 'NetworkPolicy',
                  'metadata': {'name': 'same-overlay-name', 'namespace': 'kanz-services'},
                  'spec': {'podSelector': {}, 'policyTypes': ['Ingress'], 'ingress': []}}
        for i in range(20):
            (infra / f'{i}.yaml').write_text(json.dumps(policy), encoding='utf-8')
        return infra

    def test_additive_collisions_remain_distinct_and_nonpolicies_are_excluded(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            infra = self.make_inventory(root)
            (infra / 'ignored.yaml').write_text('kind: Pod\nmetadata: {name: never-applied}\n', encoding='utf-8')
            original=json.loads((infra/'0.yaml').read_text(encoding='utf-8'))
            (infra/'nested.yml').write_text(json.dumps({'kind':'List','items':[{'kind':'List','items':[original]}]}),encoding='utf-8')
            (infra/'extra.json').write_text(json.dumps(original),encoding='utf-8')
            policies, sources = isolation.policy_inventory(root)
            self.assertEqual(len(policies), 22)
            self.assertEqual(len({p['metadata']['name'] for p in policies}), 22)
            self.assertEqual({s['name'] for s in sources}, {'same-overlay-name'})
            self.assertTrue(all(s['spec_sha256'] for s in sources))

    def test_unsupported_relevant_yaml_fails_closed(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            infra = self.make_inventory(root)
            (infra / 'bad.yaml').write_text('kind: NetworkPolicy\nspec: [broken\n', encoding='utf-8')
            with self.assertRaisesRegex(RuntimeError, 'unparseable'):
                isolation.policy_inventory(root)

    def test_symlink_and_escape_refused(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp)
            inside=root/'checkout'
            inside.mkdir()
            outside=root/'private'
            outside.write_text('private',encoding='utf-8')
            with self.assertRaisesRegex(RuntimeError,'escapes'):
                isolation.safe_read(outside,inside)
            link=inside/'linked.yaml'
            try:
                link.symlink_to(outside)
            except OSError:
                self.skipTest('host does not permit symlink creation; Linux CI exercises this arm')
            with self.assertRaises(RuntimeError):
                isolation.safe_read(link,inside)

    def test_empty_inventory_refused(self):
        with tempfile.TemporaryDirectory() as temp:
            with self.assertRaisesRegex(RuntimeError, 'empty or truncated'):
                isolation.policy_inventory(Path(temp))


if __name__ == '__main__':
    unittest.main()
