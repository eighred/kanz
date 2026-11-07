import copy
import errno
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import MagicMock, patch

import yaml

import check
import cleanup
import probe
import proof
from test_network_evidence import complete_controls


def objects():
    policies = [{'apiVersion': 'networking.k8s.io/v1', 'kind': 'NetworkPolicy',
                 'metadata': {'name': name, 'namespace': proof.SERVICES},
                 'spec': {'podSelector': {'matchLabels': {'app': target}}, 'policyTypes': ['Ingress'], 'ingress': []}}
                for target, name in proof.POLICY_NAMES.items()]
    deployments = [{'kind': 'Deployment', 'metadata': {'name': name, 'namespace': proof.SERVICES},
                    'spec': {'template': {'metadata': {'labels': {'app': name}}}}}
                   for name in [*proof.TARGETS, 'api-gateway', 'web-bff']]
    return policies + deployments


def complete():
    policies, _, rendered = proof.render(lambda _: yaml.safe_dump_all(objects()))
    return {'sourceSHA': 'a' * 40, 'sourceTree': 'b' * 40, 'runID': '123', 'runAttempt': '1',
            'developmentOnly': True, 'passed': True, 'cleanupPassed': True, 'restoredExact': True,
            'initialPolicyReadback': True, 'unionCleanupPassed': True, 'networkControls': complete_controls(), 'nodeImage': proof.feed.NODE, 'fixtureImage': proof.IMAGE,
            'cluster': 'cni-proof-' + 'c' * 12, 'renderSHA256': rendered,
            'policyRemovalSHA256': 'd' * 64, 'policyRestoreSHA256': 'd' * 64,
            'policies': [{'name': p['metadata']['name'], 'namespace': proof.SERVICES, 'sha256': proof.digest(p)} for p in policies],
            'observations': {name: {'result': [outcome] * 3, **({'before': ['allowed'] * 3, 'after': ['allowed'] * 3}
                                                                if outcome == 'blocked' else {})}
                             for name, outcome in proof.cases().items()}}


def validate(report):
    proof.validate_report(report, 'a' * 40, 'b' * 40, '123', '1')


class EvidenceTests(unittest.TestCase):
    def test_collision_never_grants_cleanup_ownership(self):
        with tempfile.TemporaryDirectory() as temporary, patch.dict(cleanup.os.environ, {'RUNNER_TEMP': temporary}):
            scratch = Path(temporary)
            artifact = scratch / 'gateway-policy.json'
            name = 'cni-proof-' + 'a' * 12
            with self.assertRaises(ValueError):
                proof.claim_cluster(name, scratch, artifact, lambda _: name)
            self.assertFalse(artifact.with_suffix('.cluster').exists())
            with patch.object(cleanup.subprocess, 'run') as run:
                cleanup.main(); run.assert_not_called()
            (scratch / 'kubeconfig').write_text('existing')
            with self.assertRaises(ValueError): proof.claim_cluster(name, scratch, artifact, lambda _: '')
            self.assertFalse(artifact.with_suffix('.cluster').exists())

    def test_policy_api_serialization_defaults_only(self):
        policies = objects()[:3]
        policies[0]['spec']['ingress'] = [{'from': [{'podSelector': {}}], 'ports': [{'port': 8101}]}]
        actual = copy.deepcopy(policies)
        actual[0]['spec']['ingress'][0]['ports'][0]['protocol'] = 'TCP'
        for p in actual[1:]: del p['spec']['ingress']
        proof.same_policy_union(actual, policies)
        for change in ('selector', 'port', 'protocol', 'missing', 'extra', 'namespace'):
            mutated = copy.deepcopy(actual)
            if change == 'selector': del mutated[0]['spec']['ingress'][0]['from'][0]['podSelector']
            if change == 'port': mutated[0]['spec']['ingress'][0]['ports'][0]['port'] = 9999
            if change == 'protocol': mutated[0]['spec']['ingress'][0]['ports'][0]['protocol'] = 'UDP'
            if change == 'missing': mutated.pop()
            if change == 'extra': mutated.append(copy.deepcopy(mutated[0]))
            if change == 'namespace': mutated[0]['metadata']['namespace'] = 'other'
            with self.assertRaises(ValueError): proof.same_policy_union(mutated, policies)

    def test_complete(self):
        validate(complete())
        self.assertEqual(len(proof.cases()), 76)

    def test_binding_cleanup_mutations(self):
        for key, value in [('sourceSHA', 'c' * 40), ('sourceTree', 'c' * 40), ('runID', '124'),
                           ('runAttempt', '2'), ('passed', False), ('cleanupPassed', False),
                           ('restoredExact', False), ('initialPolicyReadback', False),
                           ('policyRestoreSHA256', 'e' * 64), ('fixtureImage', 'python:latest')]:
            with self.subTest(key=key):
                report = complete(); report[key] = value
                with self.assertRaises(ValueError): validate(report)

    def test_case_mutations(self):
        for name, outcome in proof.cases().items():
            report = complete(); del report['observations'][name]
            with self.assertRaises(ValueError): validate(report)
            report = complete(); report['observations'][name]['result'] = [outcome] * 2
            with self.assertRaises(ValueError): validate(report)
            if outcome == 'blocked':
                for control in ('before', 'after'):
                    report = complete(); report['observations'][name][control] = ['blocked'] * 3
                    with self.assertRaises(ValueError): validate(report)

    def test_policy_inventory_mutations(self):
        for mutation in ('missing', 'duplicate', 'hash', 'namespace'):
            report = complete()
            if mutation == 'missing': report['policies'].pop()
            if mutation == 'duplicate': report['policies'].append(report['policies'][0])
            if mutation == 'hash': report['policies'][0]['sha256'] = 'unverified'
            if mutation == 'namespace': report['policies'][0]['namespace'] = 'outside'
            with self.assertRaises(ValueError): validate(report)

    def test_render_retains_additive_union(self):
        docs = objects()
        extra = copy.deepcopy(docs[0]); extra['metadata']['name'] = 'extra-additive'; docs.append(extra)
        policies, _, _ = proof.render(lambda _: yaml.safe_dump_all(docs))
        self.assertEqual(len(policies), 4)
        for mutated in (objects()[1:], objects() + [objects()[0]]):
            with self.assertRaises(ValueError): proof.render(lambda _: yaml.safe_dump_all(mutated))
        docs = objects(); docs[-1]['spec']['template']['metadata']['labels']['app'] = 'wrong'
        with self.assertRaises(ValueError): proof.render(lambda _: yaml.safe_dump_all(docs))

    def test_checker_rejects_resealed_policy_hash(self):
        report = complete()
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / 'evidence.json'
            def output(args, **kwargs):
                if args[:2] == ['git', 'rev-parse']:
                    return 'a' * 40 if args[-1] == 'HEAD' else 'b' * 40
                return yaml.safe_dump_all(objects()).strip()
            with patch.object(check.subprocess, 'check_output', side_effect=output):
                path.write_text(json.dumps(report)); check.check(path, 'a' * 40, 'b' * 40, '123', '1')
                report['policies'][0]['sha256'] = 'f' * 64
                path.write_text(json.dumps(report))
                with self.assertRaises(ValueError): check.check(path, 'a' * 40, 'b' * 40, '123', '1')

    def test_untyped_connection_error_is_unknown(self):
        with patch.object(probe.socket, 'create_connection', side_effect=ConnectionRefusedError):
            with self.assertRaisesRegex(ValueError, "unknown connection observation"): probe.observe('192.0.2.1', 8101, 'blocked')

    def test_finite_connect_denials_but_not_protocol_errors(self):
        with patch.object(probe.socket, 'create_connection', side_effect=TimeoutError), patch.object(probe.time, 'sleep'):
            self.assertEqual(probe.observe('192.0.2.1', 8101, 'blocked'), ['blocked'] * 3)
        for code in (errno.ECONNREFUSED, errno.EHOSTUNREACH):
            with patch.object(probe.socket, 'create_connection', side_effect=OSError(code, 'private')), patch.object(probe.time, 'sleep'):
                self.assertEqual(probe.observe('192.0.2.1', 8101, 'blocked'), ['blocked'] * 3)
        connection = MagicMock(); connection.__enter__.return_value = connection; connection.recv.side_effect = TimeoutError
        with patch.object(probe.socket, 'create_connection', return_value=connection):
            with self.assertRaises(TimeoutError): probe.observe('192.0.2.1', 8101, 'blocked')

    def test_positive_requires_exact_http_body(self):
        connection = MagicMock(); connection.__enter__.return_value = connection
        connection.recv.side_effect = [b'HTTP/1.0 200 OK\r\n\r\nfixture-port=9999\n', b'']
        with patch.object(probe.socket, 'create_connection', return_value=connection):
            with self.assertRaises(ValueError): probe.observe('192.0.2.1', 8101, 'allowed')

    def test_cleanup_owns_exact_nonce(self):
        with tempfile.TemporaryDirectory() as temporary, patch.dict(cleanup.os.environ, {'RUNNER_TEMP': temporary}):
            marker = Path(temporary) / 'gateway-policy.cluster'
            with patch.object(cleanup.subprocess, 'run') as run:
                cleanup.main(); run.assert_not_called()
                marker.write_text('production')
                with self.assertRaises(ValueError): cleanup.main()
                run.assert_not_called()
                marker.write_text('cni-proof-' + 'c' * 12)
                run.side_effect = [MagicMock(stdout=marker.read_text() + '\n'), MagicMock(stdout=''), MagicMock(stdout='')]
                cleanup.main()
                self.assertEqual(run.call_args_list[1].args[0], ['kind', 'delete', 'cluster', '--name', marker.read_text()])


if __name__ == '__main__':
    unittest.main()


class NamespaceCustodyTests(unittest.TestCase):
    def test_collision_lost_create_and_replacement_custody(self):
        for mode in ("collision", "lost_create", "setup_failure", "replacement"):
            objects, deleted = {}, []
            failed = False
            def k(*args, data=None, **kwargs):
                nonlocal failed
                if args[0] == "get":
                    name = args[2]
                    if mode == "collision" and args[-1] == "name": return name
                    obj = objects.get(name)
                    if obj is not None and mode == "replacement" and failed:
                        obj = copy.deepcopy(obj); obj["metadata"]["uid"] = "foreign"
                    return json.dumps(obj) if args[-1] == "json" and obj else ""
                if args[0] == "create":
                    obj = json.loads(data); name = obj["metadata"]["name"]
                    obj["metadata"].update(uid="owned-" + name, resourceVersion="1")
                    objects[name] = obj
                    if mode == "lost_create":
                        failed = True; raise RuntimeError("synthetic lost create response")
                    return ""
                if args[0] == "apply":
                    failed = True; raise RuntimeError("synthetic setup failure")
                if args[0] == "delete":
                    name = args[2].rsplit("/", 1)[1]
                    control = json.loads(data)["preconditions"]
                    self.assertEqual(control, {"uid": objects[name]["metadata"]["uid"], "resourceVersion": "1"})
                    deleted.append(name); del objects[name]; return ""
                if args[0] == "wait": return ""
                raise AssertionError("unexpected fixture operation")
            labels = {name: {"app": name} for name in (*proof.TARGETS, "api-gateway", "web-bff")}
            with self.subTest(mode=mode), self.assertRaises((RuntimeError, ValueError)):
                proof.qualify_union(k, [], labels, {})
            if mode in ("collision", "replacement"):
                self.assertEqual(deleted, [])
            else:
                self.assertTrue(deleted); self.assertEqual(objects, {})
