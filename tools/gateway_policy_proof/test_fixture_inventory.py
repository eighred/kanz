import copy
import json
import unittest
from unittest.mock import Mock

import network_fixture as fixture


class FixtureInventoryTests(unittest.TestCase):
    def setUp(self):
        self.nonce = 'b' * 32
        self.expected = {('ns' + str(i % 3), 'pod' + str(i)) for i in range(8)}
        self.pods = [{'metadata': {'namespace': ns, 'name': name, 'uid': name,
                                   'labels': {fixture.OWNER: self.nonce}},
                      'spec': {'containers': [{'image': 'pinned'}]},
                      'status': {'phase': 'Running', 'conditions': [{'type': 'Ready', 'status': 'True'}],
                                 'podIP': '10.0.0.' + str(i + 1),
                                 'containerStatuses': [{'restartCount': 0, 'imageID': 'immutable'}]}}
                     for i, (ns, name) in enumerate(sorted(self.expected))]

    def observe(self, pods):
        query = Mock(return_value=json.dumps({'items': pods}))
        value = fixture.snapshot(query, self.expected, 'pinned', self.nonce)
        query.assert_called_once_with('get', 'pods', '-A', '-l', fixture.OWNER + '=' + self.nonce, '-o', 'json')
        return value

    def test_exact_inventory_and_identity(self):
        original = self.observe(self.pods)
        self.assertEqual(len(original), 8)
        self.assertEqual(original, self.observe(list(reversed(self.pods))))
        for field in ('uid', 'podIP', 'imageID'):
            pods = copy.deepcopy(self.pods)
            owner = pods[0]['metadata'] if field == 'uid' else pods[0]['status'] if field == 'podIP' else pods[0]['status']['containerStatuses'][0]
            owner[field] = 'replacement'
            self.assertNotEqual(original, self.observe(pods))

    def test_closed_inventory_refusals(self):
        mutations = {
            'missing': lambda p: p.pop(),
            'extra': lambda p: p.append(copy.deepcopy(p[0])),
            'duplicate': lambda p: p.__setitem__(1, copy.deepcopy(p[0])),
            'namespace': lambda p: p[0]['metadata'].__setitem__('namespace', 'other'),
            'name': lambda p: p[0]['metadata'].__setitem__('name', 'other'),
            'label': lambda p: p[0]['metadata']['labels'].__setitem__(fixture.OWNER, 'other'),
            'malformed': lambda p: p.__setitem__(0, None),
            'labels': lambda p: p[0]['metadata'].__setitem__('labels', None),
            'uid': lambda p: p[0]['metadata'].__setitem__('uid', ''),
            'ready': lambda p: p[0]['status']['conditions'][0].__setitem__('status', 'False'),
            'phase': lambda p: p[0]['status'].__setitem__('phase', 'Pending'),
            'image': lambda p: p[0]['spec']['containers'][0].__setitem__('image', 'other'),
            'sidecar': lambda p: p[0]['spec']['containers'].append({'image': 'pinned'}),
            'restart': lambda p: p[0]['status']['containerStatuses'][0].__setitem__('restartCount', 1),
            'imageID': lambda p: p[0]['status']['containerStatuses'][0].__setitem__('imageID', ''),
        }
        for name, mutate in mutations.items():
            pods = copy.deepcopy(self.pods); mutate(pods)
            with self.subTest(name=name), self.assertRaises(ValueError): self.observe(pods)
        for value in (None, [], {}, {'items': None}, {'items': {}}):
            with self.assertRaises(ValueError): fixture.snapshot(Mock(return_value=json.dumps(value)), self.expected, 'pinned', self.nonce)

    def test_selector_independence_before_any_effect(self):
        fixture.selector_independent([{'spec': {'podSelector': {'matchLabels': {'app': 'a'}}}}], {'a': {'app': 'a'}})
        for selector in ({'matchLabels': {fixture.OWNER: 'anything'}},
                         {'matchExpressions': [{'key': fixture.OWNER, 'operator': 'DoesNotExist'}]}):
            for direction in ('ingress', 'egress'):
                policies = [{'spec': {direction: [{'from' if direction == 'ingress' else 'to': [{'podSelector': selector}]}]}}]
                with self.assertRaises(ValueError): fixture.selector_independent(policies, {})
        with self.assertRaises(ValueError): fixture.selector_independent([], {'a': {fixture.OWNER: 'old'}})
        # The real executor checks the whole policy graph before namespace writes.
        from pathlib import Path
        source = (Path(__file__).parent / 'proof.py').read_text(encoding='utf-8')
        body = source[source.index('def qualify_union('):]
        self.assertLess(body.index('network.selector_independent(policies, labels)'), body.index("k('create'"))
        self.assertIn('if snapshot(probes.k, expected, image, nonce) != identities:', (Path(__file__).parent / 'network_fixture.py').read_text(encoding='utf-8'))


if __name__ == '__main__':
    unittest.main()
