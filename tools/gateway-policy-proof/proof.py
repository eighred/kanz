"""Disposable Cilium qualification of the actual rendered Tokyo ingress union."""
import hashlib
import copy
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import time
import uuid

import yaml

ROOT = Path(__file__).resolve().parents[2]
HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('feed_fixture', ROOT / 'tools/feed-identity/proof.py')
feed = importlib.util.module_from_spec(spec)
spec.loader.exec_module(feed)
IMAGE = 'python:3.13.18-alpine3.24@sha256:7712cfd23e0501f5cb58faa5e6e0d515c9939bdbce866921799b31e696a1d13c'
SERVICES = 'kanz-services'
OBS = 'kanz-observability'
OUTSIDE = 'gateway-proof-outsider'
TARGETS = {'accounting': (8101, 8080), 'audit': (8102, 8083), 'compliance': (8095, 8091)}
POLICY_NAMES = {'accounting': 'allow-gateway-to-accounting', 'audit': 'allow-gateway-to-audit',
                'compliance': 'allow-gateway-to-compliance-mandate'}


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def claim_cluster(name, scratch, artifact, run):
    # Do not leave a deletion marker for a refused existing cluster. Record the
    # fresh nonce only immediately before its potentially partial creation.
    if name in run(['kind', 'get', 'clusters']).splitlines() or (scratch / 'kubeconfig').exists():
        raise ValueError('existing_cluster_refusal')
    with artifact.with_suffix('.cluster').open('x') as marker:
        marker.write(name)


def canonical_spec(spec):
    value = copy.deepcopy(spec)
    for direction in ('ingress', 'egress'):
        # Kubernetes omitempty drops empty rule slices. With explicit
        # policyTypes their absence and [] both admit no traffic.
        if value.get(direction) == []:
            del value[direction]
        for rule in value.get(direction, []):
            for port in rule.get('ports', []):
                port.setdefault('protocol', 'TCP')
    return value


def same_policy_union(actual, expected):
    def inventory(items):
        result = {}
        for policy in items:
            key = (policy['metadata']['namespace'], policy['metadata']['name'])
            if key in result:
                raise ValueError('duplicate policy readback')
            result[key] = canonical_spec(policy['spec'])
        return result
    if inventory(actual) != inventory(expected):
        raise ValueError('policy_union_readback_mismatch')


def render(run):
    raw = run(['kubectl', 'kustomize', '--load-restrictor=LoadRestrictionsNone',
               'kanz/infra/overlays/testnet-tokyo']).strip()
    if len(raw.encode()) > 4 << 20:
        raise ValueError('render exceeded bound')
    docs = [d for d in yaml.safe_load_all(raw) if d]
    policies = [d for d in docs if d.get('kind') == 'NetworkPolicy']
    if not 3 <= len(policies) <= 64:
        raise ValueError('missing policy inventory')
    names = [p['metadata']['name'] for p in policies]
    if len(set(names)) != len(names) or any(p['metadata'].get('namespace') != SERVICES for p in policies):
        raise ValueError('unexpected policy ownership')
    if not set(POLICY_NAMES.values()).issubset(names):
        raise ValueError('missing shared API policies')
    labels = {}
    for name in [*TARGETS, 'api-gateway', 'web-bff']:
        matches = [d for d in docs if d.get('kind') == 'Deployment' and d['metadata']['name'] == name]
        if len(matches) != 1 or matches[0]['metadata'].get('namespace') != SERVICES:
            raise ValueError('missing target deployment')
        labels[name] = matches[0]['spec']['template']['metadata']['labels']
        if labels[name].get('app') != name:
            raise ValueError('target label mismatch')
    return policies, labels, hashlib.sha256(raw.encode()).hexdigest()


def cases():
    result = {}
    for target in TARGETS:
        for route in ('pod', 'service'):
            for phase, peer, port, outcome in (
                    ('baseline', 'untrusted', 'api', 'allowed'), ('baseline', 'observer', 'ops', 'allowed'),
                    ('baseline', 'web-bff', 'api', 'allowed'), ('baseline', 'observer', 'api', 'allowed'),
                    ('baseline', 'spoof-gateway', 'api', 'allowed'),
                    ('enforced', 'api-gateway', 'api', 'allowed'), ('enforced', 'observer', 'ops', 'allowed'),
                    ('enforced', 'untrusted', 'api', 'blocked'), ('enforced', 'web-bff', 'api', 'blocked'),
                    ('enforced', 'observer', 'api', 'blocked'), ('enforced', 'spoof-gateway', 'api', 'blocked')):
                result['/'.join((phase, target, route, peer, port))] = outcome
    for route in ('pod', 'service'):
        for phase, peer, outcome in (('baseline', 'untrusted', 'allowed'),
                                      ('enforced', 'web-bff', 'allowed'), ('enforced', 'untrusted', 'blocked')):
            result['/'.join((phase, 'api-gateway', route, peer, 'api'))] = outcome
        result['removed/accounting/' + route + '/untrusted/api'] = 'allowed'
        result['restored/accounting/' + route + '/untrusted/api'] = 'blocked'
    return result


def validate_report(report, source, tree, run_id, attempt):
    if (report.get('sourceSHA') != source or report.get('sourceTree') != tree
            or not re.fullmatch('[0-9a-f]{40}', source) or not re.fullmatch('[0-9a-f]{40}', tree)
            or report.get('runID') != run_id or report.get('runAttempt') != attempt
            or not re.fullmatch('[0-9]+', run_id) or not re.fullmatch('[1-9][0-9]*', attempt)
            or report.get('developmentOnly') is not True or report.get('passed') is not True
            or report.get('cleanupPassed') is not True or report.get('restoredExact') is not True
            or report.get('initialPolicyReadback') is not True
            or report.get('policyRestoreSHA256') != report.get('policyRemovalSHA256')
            or not re.fullmatch('[0-9a-f]{64}', report.get('policyRestoreSHA256', ''))
            or report.get('nodeImage') != feed.NODE or report.get('fixtureImage') != IMAGE
            or not re.fullmatch(r'cni-proof-[0-9a-f]{12}', report.get('cluster', ''))):
        raise ValueError('missing source/cleanup/boundary evidence')
    validate_union(report)


def network_fixture():
    path = HERE / "network_fixture.py"
    spec = importlib.util.spec_from_file_location("gateway_network_fixture", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def validate_union(report):
    if (report.get('unionCleanupPassed') is not True or report.get('restoredExact') is not True
            or report.get('initialPolicyReadback') is not True
            or report.get('policyRemovalSHA256') != report.get('policyRestoreSHA256')):
        raise ValueError('missing rendered union lifecycle')
    network_fixture().validate(report.get('networkControls'))
    inventory = report.get('policies')
    if not isinstance(inventory, list) or not 3 <= len(inventory) <= 64:
        raise ValueError('missing full policy inventory')
    if (len({p['name'] for p in inventory}) != len(inventory)
            or not set(POLICY_NAMES.values()).issubset(p['name'] for p in inventory)
            or any(set(p) != {'name', 'namespace', 'sha256'} or p['namespace'] != SERVICES
                   or not re.fullmatch('[0-9a-f]{64}', p['sha256']) for p in inventory)
            or not re.fullmatch('[0-9a-f]{64}', report.get('renderSHA256', ''))):
        raise ValueError('invalid policy inventory')
    observations = report.get('observations')
    expected = cases()
    if not isinstance(observations, dict) or set(observations) != set(expected):
        raise ValueError('incomplete case inventory')
    for name, outcome in expected.items():
        item = observations[name]
        keys = {'result', 'before', 'after'} if outcome == 'blocked' else {'result'}
        if set(item) != keys or item['result'] != [outcome] * 3:
            raise ValueError('missing independent observations')
        if outcome == 'blocked' and (item['before'] != ['allowed'] * 3 or item['after'] != ['allowed'] * 3):
            raise ValueError('negative lacks live positive controls')


def pod(name, namespace, labels, ports=()):
    container = {'name': 'fixture', 'image': IMAGE,
                 'command': ['python3', '-B', '-u', '/fixture/listener.py', *map(str, ports)] if ports
                 else ['python3', '-B', '-c', 'import time;time.sleep(3600)'],
                 'securityContext': {'allowPrivilegeEscalation': False, 'readOnlyRootFilesystem': True,
                                     'capabilities': {'drop': ['ALL']}},
                 'resources': {'requests': {'cpu': '25m', 'memory': '32Mi'},
                               'limits': {'cpu': '500m', 'memory': '128Mi'}},
                 'volumeMounts': [{'name': 'scripts', 'mountPath': '/fixture', 'readOnly': True}]}
    if ports:
        container['readinessProbe'] = {'httpGet': {'path': '/fixture', 'port': ports[0]}, 'periodSeconds': 1}
    return {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': name, 'namespace': namespace, 'labels': labels},
            'spec': {'automountServiceAccountToken': False, 'restartPolicy': 'Never',
                     'securityContext': {'runAsNonRoot': True, 'runAsUser': 65532, 'runAsGroup': 65532,
                                         'seccompProfile': {'type': 'RuntimeDefault'}},
                     'containers': [container], 'volumes': [{'name': 'scripts', 'configMap': {'name': 'scripts'}}]}}


def qualify_union(k, policies, labels, report):
    """Same actual rendered objects on a caller-owned disposable cluster.

    Namespace absence precedes ownership. Cleanup reconciles response loss only
    against the fresh nonce, then deletes by observed UID/resourceVersion.
    """
    nonce = uuid.uuid4().hex
    namespaces = (SERVICES, OBS, OUTSIDE)
    for namespace in namespaces:
        if k('get', 'namespace', namespace, '--ignore-not-found', '-o', 'name').strip():
            raise ValueError('rendered_namespace_collision')
    def apply(items):
        k('apply', '-f', '-', data=yaml.safe_dump_all(items))
    def policy_readback(policies):
        actual = json.loads(k('-n', SERVICES, 'get', 'networkpolicy', '-o', 'json'))['items']
        same_policy_union(actual, policies)
    created = {}
    try:
        for namespace in namespaces:
            desired = {'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace, 'labels': {'kanz.io/gateway-proof': nonce}}}
            try:
                k('create', '-f', '-', data=json.dumps(desired))
            finally:
                actual = json.loads(k('get', 'namespace', namespace, '--ignore-not-found', '-o', 'json') or 'null')
                if actual is not None and actual['metadata'].get('labels', {}).get('kanz.io/gateway-proof') == nonce:
                    created[namespace] = actual['metadata']['uid']
            if namespace not in created:
                raise ValueError('rendered_namespace_create_unknown')
        report['stage'] = 'synthetic_listeners'
        peers = {'api-gateway': (SERVICES, 'api-gateway'), 'web-bff': (SERVICES, 'web-bff'),
                 'untrusted': (SERVICES, 'untrusted'), 'observer': (OBS, 'observer'),
                 'spoof-gateway': (OUTSIDE, 'spoof-gateway')}
        objects = []
        for namespace in (SERVICES, OBS, OUTSIDE):
            objects.extend([{'apiVersion': 'v1', 'kind': 'ConfigMap', 'metadata': {'name': 'scripts', 'namespace': namespace},
                             'data': {p: (HERE / p).read_text(encoding='utf-8') for p in ('listener.py', 'probe.py', 'network_observation.py')}}])
        for target, ports in {**TARGETS, 'api-gateway': (8080,)}.items():
            objects.append(pod(target, SERVICES, labels[target], ports))
            objects.append({'apiVersion': 'v1', 'kind': 'Service', 'metadata': {'name': target, 'namespace': SERVICES},
                            'spec': {'selector': {'app': target}, 'ports': [
                                {'name': 'p' + str(port), 'port': port, 'targetPort': port} for port in ports]}})
        for peer, (namespace, podname) in peers.items():
            if peer == 'api-gateway':
                continue
            objects.append(pod(podname, namespace, labels.get(peer, {'app': 'api-gateway' if peer == 'spoof-gateway' else peer})))
        apply(objects)
        for namespace in (SERVICES, OBS, OUTSIDE):
            k('-n', namespace, 'wait', '--for=condition=Ready', 'pod', '--all', '--timeout=120s')
        endpoints = {}
        for target in [*TARGETS, 'api-gateway']:
            endpoints[target] = {'pod': json.loads(k('-n', SERVICES, 'get', 'pod', target, '-o', 'json'))['status']['podIP'],
                                 'service': json.loads(k('-n', SERVICES, 'get', 'service', target, '-o', 'json'))['spec']['clusterIP']}
        def sample(peer, target, route, kind, outcome):
            namespace, podname = peers[peer]
            port = 8080 if target == 'api-gateway' else TARGETS[target][kind == 'ops']
            raw = k('-n', namespace, 'exec', podname, '--', 'python3', '-B', '/fixture/probe.py',
                    endpoints[target][route], str(port), outcome)
            value = json.loads(raw)
            if not isinstance(value, list) or len(value) != 3 or any(v not in ('allowed', 'blocked') for v in value):
                raise ValueError('unknown_probe')
            return value
        def probe(peer, target, route, kind, outcome):
            deadline = time.monotonic() + 20
            consecutive = 0
            trail = []
            for _ in range(6):
                values = sample(peer, target, route, kind, outcome)
                if time.monotonic() >= deadline:
                    break
                trail = (trail + values)[-3:]
                for value in values:
                    consecutive = consecutive + 1 if value == outcome else 0
                if consecutive >= 3:
                    return trail
            raise ValueError('network_not_converged')
        def phase(selected):
            for case, outcome in cases().items():
                prefix, target, route, peer, kind = case.split('/')
                if prefix != selected:
                    continue
                item = {}
                report['observations'][case] = item
                control = 'web-bff' if target == 'api-gateway' else 'api-gateway'
                if outcome == 'blocked':
                    item['before'] = probe(control, target, route, kind, 'allowed')
                try:
                    item['result'] = probe(peer, target, route, kind, outcome)
                finally:
                    if outcome == 'blocked':
                        item['after'] = probe(control, target, route, kind, 'allowed')
                report['observations'][case] = item
        report['stage'] = 'baseline'
        phase('baseline')
        network = network_fixture()
        report['networkControls'] = {}
        probe('api-gateway', 'accounting', 'pod', 'api', 'allowed')
        try:
            report['networkControls']['closed'] = network.qualify(k, peers, endpoints, IMAGE, 'closed')
        finally:
            probe('api-gateway', 'accounting', 'pod', 'api', 'allowed')
        report['networkControls']['no_policy'] = network.qualify(k, peers, endpoints, IMAGE, 'no_policy')
        report['stage'] = 'enforced'
        apply(policies)
        policy_readback(policies)
        report['initialPolicyReadback'] = True
        phase('enforced')
        report['networkControls']['enforced'] = network.qualify(k, peers, endpoints, IMAGE, 'enforced')
        report['stage'] = 'removal_control'
        removed = next(p for p in policies if p['metadata']['name'] == POLICY_NAMES['accounting'])
        report['policyRemovalSHA256'] = digest(removed)
        k('-n', SERVICES, 'delete', 'networkpolicy', removed['metadata']['name'], '--wait=true')
        policy_readback([p for p in policies if p is not removed])
        phase('removed')
        report['stage'] = 'exact_restore'
        apply([removed])
        policy_readback(policies)
        report['policyRestoreSHA256'] = digest(removed)
        phase('restored')
        report['restoredExact'] = True
    finally:
        for namespace in reversed(list(created)):
            actual = json.loads(k('get', 'namespace', namespace, '--ignore-not-found', '-o', 'json') or 'null')
            if actual is None:
                continue
            if (actual['metadata'].get('labels', {}).get('kanz.io/gateway-proof') != nonce
                    or actual['metadata']['uid'] != created[namespace]):
                raise ValueError('rendered_namespace_ownership')
            meta = actual['metadata']
            k('delete', '--raw', '/api/v1/namespaces/' + namespace, '-f', '-', data=json.dumps({
                'apiVersion': 'v1', 'kind': 'DeleteOptions',
                'preconditions': {'uid': meta['uid'], 'resourceVersion': meta['resourceVersion']}}))
            k('wait', '--for=delete', 'namespace/' + namespace, '--timeout=60s')
            if k('get', 'namespace', namespace, '--ignore-not-found', '-o', 'name').strip():
                raise ValueError('rendered_namespace_not_absent')
        report['unionCleanupPassed'] = True


def main():
    artifact = Path(os.environ['RUNNER_TEMP']) / 'gateway-policy.json'
    name = 'cni-proof-' + uuid.uuid4().hex[:12]
    report = {'developmentOnly': True, 'passed': False, 'cleanupPassed': False, 'cluster': name,
              'nodeImage': feed.NODE, 'fixtureImage': IMAGE, 'observations': {},
              'runID': os.environ.get('GITHUB_RUN_ID', ''), 'runAttempt': os.environ.get('GITHUB_RUN_ATTEMPT', '')}
    owned = False
    with tempfile.TemporaryDirectory(prefix='kanz-gateway-policy-') as temporary:
        scratch = Path(temporary)
        environment = {k: os.environ[k] for k in ('PATH', 'SYSTEMROOT') if k in os.environ}
        environment.update(HOME=str(scratch), TMPDIR=str(scratch), LANG='C.UTF-8')
        def run(args, data=None, timeout=300):
            result = subprocess.run(args, input=data, text=True, capture_output=True, timeout=timeout,
                                    cwd=ROOT, env=environment)
            if result.returncode:
                raise RuntimeError('command_failed')
            if len(result.stdout.encode()) > 4 << 20:
                raise RuntimeError('command_output_bound')
            return result.stdout.strip()
        feed.command = run
        kubeconfig = str(scratch / 'kubeconfig')
        def k(*args, data=None, timeout=90):
            return run(['kubectl', '--kubeconfig', kubeconfig, '--context', 'kind-' + name, *args], data=data, timeout=min(timeout, 90))
        def apply(items):
            k('apply', '-f', '-', data=yaml.safe_dump_all(items))
        def policy_readback(policies):
            actual = json.loads(k('-n', SERVICES, 'get', 'networkpolicy', '-o', 'json'))['items']
            same_policy_union(actual, policies)
        try:
            report['stage'] = 'render'
            report['sourceSHA'] = run(['git', 'rev-parse', 'HEAD'])
            report['sourceTree'] = run(['git', 'rev-parse', 'HEAD^{tree}'])
            policies, labels, report['renderSHA256'] = render(run)
            report['policies'] = [{'name': p['metadata']['name'], 'namespace': p['metadata']['namespace'],
                                   'sha256': digest(p)} for p in policies]
            report['stage'] = 'create_cilium'
            claim_cluster(name, scratch, artifact, run)
            owned = True
            feed.create_enforcing_cluster(name, scratch)
            qualify_union(k, policies, labels, report)
            report['passed'] = True
        except Exception:
            report['failure'] = 'qualification_failed'
            raise
        finally:
            try:
                if owned:
                    run(['kind', 'delete', 'cluster', '--name', name], timeout=180)
                    if name in run(['kind', 'get', 'clusters']).splitlines():
                        raise RuntimeError('cleanup_not_absent')
                report['cleanupPassed'] = owned
            except Exception:
                report['passed'] = False
                report['cleanupPassed'] = False
                raise
            finally:
                if report['passed']:
                    try:
                        validate_report(report, report['sourceSHA'], report['sourceTree'], report['runID'], report['runAttempt'])
                    except Exception:
                        report['passed'] = False
                        artifact.write_text(json.dumps(report, sort_keys=True))
                        raise
                artifact.write_text(json.dumps(report, sort_keys=True))
        validate_report(report, report['sourceSHA'], report['sourceTree'], report['runID'], report['runAttempt'])
        print('Rendered Tokyo policy isolation passed; disposable cluster absent')


if __name__ == '__main__':
    main()
