"""Disposable Tokyo messaging policy qualification; no existing-context input."""
import copy
from datetime import datetime, timezone
import hashlib
import importlib.util
import ipaddress
import json
import os
import re
import time
from pathlib import Path
import secrets
import subprocess
import tempfile
import uuid

import yaml

ROOT = Path(__file__).resolve().parents[2]
HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('feed_fixture', ROOT / 'tools/feed-identity/proof.py')
feed = importlib.util.module_from_spec(spec)
spec.loader.exec_module(feed)
PYTHON = 'python:3.13.18-alpine3.24@sha256:7712cfd23e0501f5cb58faa5e6e0d515c9939bdbce866921799b31e696a1d13c'
REDIS = 'redis@sha256:bb186d083732f669da90be8b0f975a37812b15e913465bb14d845db72a4e3e08'
BOX = 'natsio/nats-box@sha256:ffce8bd103383f179f8c7f11cf645726acf5d17280706c530c3b342dbe16334c'
NATS = 'nats@sha256:d4ac35882ac65aff236cd65b9d3fa4d24332c681e1a85f94eedccd3cdd65b1da'
PROMETHEUS = 'quay.io/prometheus/prometheus:v3.13.3@sha256:6976aa8a60fec930796ce5772b8d12da7a318a5daa8d40d69c5c7819a05eeed7'
SECURITY = {'allowPrivilegeEscalation': False, 'readOnlyRootFilesystem': True, 'capabilities': {'drop': ['ALL']}}
RESOURCES = {'requests': {'cpu': '25m', 'memory': '32Mi'}, 'limits': {'cpu': '500m', 'memory': '256Mi'}}
TCP = '''import socket,sys
for _ in range(3):
 try:
  with socket.create_connection((sys.argv[1],int(sys.argv[2])),timeout=2): pass
 except TimeoutError:
  assert sys.argv[3]=='blocked'
 else:
  assert sys.argv[3]=='allowed'
print('TCP_PASS')
'''
METRICS = '''from http.server import BaseHTTPRequestHandler,HTTPServer
class Handler(BaseHTTPRequestHandler):
 def log_message(self,*args):pass
 def do_GET(self):
  body=b'# TYPE fixture_ready gauge\\nfixture_ready 1\\n'
  self.send_response(200);self.end_headers();self.wfile.write(body)
HTTPServer(('0.0.0.0',8080),Handler).serve_forever()
'''
REDIS_CLAIM = '''import socket,pathlib
password=pathlib.Path('/fixture/password').read_bytes()
def resp(*parts):return b'*'+str(len(parts)).encode()+b'\\r\\n'+b''.join(b'$'+str(len(p)).encode()+b'\\r\\n'+p+b'\\r\\n' for p in parts)
with socket.create_connection(('redis.kanz-messaging.svc',6379),timeout=3) as s:
 f=s.makefile('rb')
 def ask(*parts):s.sendall(resp(*parts));return f.readline()
 assert ask(b'PING')==b'-NOAUTH Authentication required.\\r\\n'
 assert ask(b'AUTH',password)==b'+OK\\r\\n'
 assert ask(b'SET',b'fixture:claim',b'owned',b'NX')==b'+OK\\r\\n'
 assert ask(b'SET',b'fixture:claim',b'duplicate',b'NX')==b'$-1\\r\\n'
 assert ask(b'SAVE')==b'+OK\\r\\n'
print('REDIS_CLAIM_PASS')
'''


def docs(path):
    return [d for d in yaml.safe_load_all((ROOT / path).read_text()) if d]


def staged_policies():
    return docs('tools/tokyo-messaging-proof/policies.yaml')


def bootstrap_objects():
    result = docs('kanz/infra/nats/bootstrap-job.yaml')
    job = next(d for d in result if d['kind'] == 'Job')
    patch = docs('tools/tokyo-messaging-proof/bootstrap-labels.yaml')[0]
    job['metadata'].setdefault('labels', {}).update(patch['metadata']['labels'])
    pod = job['spec']['template']
    pod.setdefault('metadata', {}).setdefault('labels', {}).update(patch['spec']['template']['metadata']['labels'])
    job['spec']['activeDeadlineSeconds'] = 180
    for container in pod['spec']['containers']:
        container['image'] = BOX
        container['resources'] = copy.deepcopy(RESOURCES)
        for env in container['env']:
            if env['name'] == 'NATS_REPLICAS':
                env['value'] = '1'
    feed.pin_images(result)
    return result


def broker_objects():
    # Real canonical account ACLs and exact Tokyo singleton config, not a relaxed fixture broker.
    result = feed.broker_objects()
    result = [d for d in result if d['metadata']['name'] != 'nats-wrong']
    for i, obj in enumerate(result):
        if obj['kind'] == 'ConfigMap' and obj['metadata']['name'] == 'nats-config':
            result[i] = docs('kanz/infra/overlays/testnet-tokyo/data/nats-config-patch.yaml')[0]
        elif obj['kind'] == 'ConfigMap' and obj['metadata']['name'] == 'nats-tenants':
            result[i] = docs('kanz/infra/nats/tenancy.yaml')[0]
        elif obj['kind'] == 'StatefulSet':
            for c in obj['spec']['template']['spec']['containers']:
                if c['name'] == 'nats':
                    c['image'] = NATS
    return result


def python_pod(name, namespace, app=None):
    metadata = {'name': name, 'namespace': namespace}
    if app:
        metadata['labels'] = {'app': app}
    return {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': metadata, 'spec': {
        'automountServiceAccountToken': False, 'restartPolicy': 'Never',
        'securityContext': {'runAsNonRoot': True, 'runAsUser': 65532, 'runAsGroup': 65532, 'fsGroup': 65532,
                            'seccompProfile': {'type': 'RuntimeDefault'}},
        'containers': [{'name': 'probe', 'image': PYTHON, 'command': ['python', '-I', '-B', '-c', 'import time;time.sleep(1800)'],
                        'resources': copy.deepcopy(RESOURCES), 'securityContext': copy.deepcopy(SECURITY)}]}}


def runner_from_job(job):
    # Mirror the existing recovery clone's metadata/spec selection exactly.
    result = {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': copy.deepcopy(job['metadata']),
              'spec': copy.deepcopy(job['spec']['template']['spec'])}
    result['metadata']['name'] = 'nats-backup-runner'
    result['metadata'].pop('annotations', None)
    result['spec']['restartPolicy'] = 'Never'
    result['spec']['containers'][0]['command'] = ['sleep', '1800']
    result['spec']['containers'][0]['args'] = []
    return result


def nats_failure(stdout, stderr):
    # Closed categories only: never return raw diagnostics, upload subjects,
    # certificate paths or payloads. Unrecognized failures stay failures.
    message = (stdout[:32768] + '\n' + stderr[:32768]).lower()
    if 'permissions violation' in message and '$js.snapshot.restore.' in message:
        return 'restore_upload_permission_denied'
    if 'authorization violation' in message:
        return 'principal_authorization_denied'
    if 'permissions violation' in message:
        return 'other_subject_permission_denied'
    if 'already exist' in message or 'exists already' in message:
        return 'stream_already_exists'
    if 'expected backup.json' in message:
        return 'backup_manifest_missing'
    if 'timeout' in message or 'timed out' in message:
        return 'operation_timeout'
    return 'unclassified_cli_failure'


def tool_version(output):
    # The binaries print v-prefixed versions; a word boundary before the
    # number would incorrectly reject the normal "v2.x.y" form.
    matches = re.findall(r'(?<![0-9.])([0-9]+\.[0-9]+\.[0-9]+)(?![0-9.])', output)
    if len(matches) != 1 or len(output) > 128:
        raise ValueError('unrecognized pinned tool version')
    return matches[0]


def restore_publish_denied(logs, address):
    ipaddress.ip_address(address)
    if len(logs) > 65536:
        return False
    # Exact server2.14.5 client.go pubPermissionViolation log, with the
    # contemporaneous backup Pod address. Raw lines never leave this function.
    pattern = (r'(?<![0-9.])' + re.escape(address) + r':\d+ - cid:\d+ - '
               r'Publish Violation - Subject "\$JS\.SNAPSHOT\.RESTORE\.[A-Za-z0-9_-]+\.[A-Za-z0-9]+"')
    return any(re.search(pattern, line) for line in logs.splitlines())


def validate_report(report, source):
    required = {'baseline-unlabelled', 'baseline-redis', 'baseline-monitor', 'trap-control', 'bootstrap', 'nats-publish', 'ungranted-identity', 'backup-restore', 'redis-claim',
                'unlabelled-nats', 'unlabelled-redis', 'wrong-namespace-nats', 'wrong-namespace-redis',
                'prometheus-nats', 'prometheus-redis', 'prometheus-monitor', 'prometheus-scrape',
                'bootstrap-redis', 'bootstrap-monitor', 'bootstrap-egress', 'redis-restart'}
    policies = staged_policies()
    if (not re.fullmatch(r'[0-9a-f]{40}', source) or report.get('sourceSHA') != source
            or report.get('passed') is not True or report.get('developmentOnly') is not True
            or report.get('cleanupPassed') is not True or set(report.get('cases', [])) != required
            or report.get('policyNames') != sorted(p['metadata']['name'] for p in policies)
            or report.get('policyCount') != len(policies)
            or report.get('policySHA256') != hashlib.sha256((HERE / 'policies.yaml').read_bytes()).hexdigest()
            or len(report['cases']) != len(required)):
        raise ValueError('incomplete messaging qualification')


def main():
    name = 'messaging-proof-' + uuid.uuid4().hex[:12]
    report = {'passed': False, 'developmentOnly': True, 'cluster': name, 'cases': []}
    artifact = Path(os.environ['RUNNER_TEMP']) / 'tokyo-messaging.json'
    with tempfile.TemporaryDirectory(prefix='kanz-messaging-') as directory:
        scratch = Path(directory)
        kubeconfig = str(scratch / 'kubeconfig')

        def k(*args, data=None):
            return feed.command(['kubectl', '--kubeconfig', kubeconfig, '--context', 'kind-' + name, *args], data=data)

        def apply(objects):
            k('apply', '-f', '-', data=yaml.safe_dump_all(objects))

        def ready(namespace, resource):
            k('-n', namespace, 'wait', '--for=condition=Ready', resource, '--timeout=180s')

        def case(label, namespace, pod, code, *args, container='probe'):
            report['stage'] = label
            output = k('-n', namespace, 'exec', pod, '-c', container, '--', 'python', '-I', '-B', '-c', code, *args)
            if output not in ('TCP_PASS', 'REDIS_CLAIM_PASS', 'PROMETHEUS_PASS'):
                raise ValueError('missing fixture success marker')
            report['cases'].append(label)

        def tcp(label, namespace, pod, address, port, outcome):
            case(label, namespace, pod, TCP, address, str(port), outcome)

        try:
            report['sourceSHA'] = feed.command(['git', 'rev-parse', 'HEAD'])
            feed.command(['git', 'diff', '--quiet', 'HEAD', '--'])
            report['policySHA256'] = hashlib.sha256((HERE / 'policies.yaml').read_bytes()).hexdigest()
            report['stage'] = 'create enforcing cluster'
            feed.create_enforcing_cluster(name, scratch)
            crds = []
            for path in sorted((ROOT / 'kanz/infra/security/spire/crds').glob('*.yaml')):
                crds += docs(str(path.relative_to(ROOT)))
            apply(crds)
            for crd in crds:
                k('wait', '--for=condition=Established', 'crd/' + crd['metadata']['name'], '--timeout=90s')
            report['stage'] = 'SPIRE'
            apply(feed.spire_objects())
            k('-n', 'spire-system', 'rollout', 'status', 'statefulset/spire-server', '--timeout=240s')
            k('-n', 'spire-system', 'rollout', 'status', 'daemonset/spire-agent', '--timeout=240s')
            apply(docs('kanz/infra/security/spire/registration.yaml'))
            apply([{'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': ns}}
                   for ns in ('kanz-observability', 'fixture-wrong')])
            report['stage'] = 'broker and Redis'
            apply(broker_objects())
            k('-n', 'kanz-messaging', 'rollout', 'status', 'statefulset/nats', '--timeout=240s')
            password = secrets.token_hex(32)
            apply([{'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'redis-fixture', 'namespace': ns},
                    'stringData': {'password': password}} for ns in ('kanz-messaging', 'kanz-services')])
            del password
            redis = python_pod('redis', 'kanz-messaging', 'redis')
            redis['spec']['containers'] = [{'name': 'redis', 'image': REDIS, 'command': ['sh', '-ceu',
                'exec redis-server --appendonly yes --appendfsync always --dir /data --requirepass "$(cat /fixture/password)"'],
                'readinessProbe': {'tcpSocket': {'port': 6379}, 'periodSeconds': 1},
                'resources': copy.deepcopy(RESOURCES), 'securityContext': copy.deepcopy(SECURITY),
                'volumeMounts': [{'name': 'data', 'mountPath': '/data'}, {'name': 'auth', 'mountPath': '/fixture', 'readOnly': True}]}]
            redis['spec']['restartPolicy'] = 'Always'
            redis['spec']['volumes'] = [{'name': 'data', 'emptyDir': {'sizeLimit': '64Mi'}}, {'name': 'auth', 'secret': {'secretName': 'redis-fixture'}}]
            apply([redis, {'apiVersion': 'v1', 'kind': 'Service', 'metadata': {'name': 'redis', 'namespace': 'kanz-messaging'},
                           'spec': {'selector': {'app': 'redis'}, 'ports': [{'port': 6379}]}}])
            ready('kanz-messaging', 'pod/redis')
            gateway = python_pod('gateway-probe', 'kanz-services', 'api-gateway')
            gateway['spec']['volumes'] = [{'name': 'auth', 'secret': {'secretName': 'redis-fixture'}}]
            gateway['spec']['containers'][0]['volumeMounts'] = [{'name': 'auth', 'mountPath': '/fixture', 'readOnly': True}]
            pods = [gateway, python_pod('unlabelled', 'kanz-services'), python_pod('wrong', 'fixture-wrong', 'api-gateway'),
                    python_pod('egress-trap', 'kanz-services', 'fixture-trap')]
            pods[-1]['spec']['containers'][0]['command'] = ['python', '-I', '-B', '-c', METRICS]
            pods[-1]['spec']['containers'][0]['readinessProbe'] = {'tcpSocket': {'port': 8080}, 'periodSeconds': 1}
            apply(pods)
            for pod in pods:
                ready(pod['metadata']['namespace'], 'pod/' + pod['metadata']['name'])
            nats_ip = k('-n', 'kanz-messaging', 'get', 'service/nats', '-o', 'jsonpath={.spec.clusterIP}')
            monitor_ip = k('-n', 'kanz-messaging', 'get', 'pod/nats-0', '-o', 'jsonpath={.status.podIP}')
            redis_ip = k('-n', 'kanz-messaging', 'get', 'service/redis', '-o', 'jsonpath={.spec.clusterIP}')
            trap_ip = k('-n', 'kanz-services', 'get', 'pod/egress-trap', '-o', 'jsonpath={.status.podIP}')
            tcp('baseline-unlabelled', 'kanz-services', 'unlabelled', nats_ip, 4222, 'allowed')
            tcp('baseline-redis', 'kanz-services', 'unlabelled', redis_ip, 6379, 'allowed')
            tcp('baseline-monitor', 'kanz-services', 'unlabelled', monitor_ip, 8222, 'allowed')
            tcp('trap-control', 'kanz-services', 'unlabelled', trap_ip, 8080, 'allowed')
            apply(staged_policies())
            report['stage'] = 'canonical bootstrap'
            bootstrap = bootstrap_objects()
            apply(bootstrap)
            k('-n', 'kanz-messaging', 'wait', '--for=condition=complete', 'job/nats-bootstrap', '--timeout=200s')
            report['cases'].append('bootstrap')
            runner = runner_from_job(next(d for d in bootstrap if d['kind'] == 'Job'))
            runner['spec']['containers'].append(copy.deepcopy(gateway['spec']['containers'][0]))
            runner['spec']['containers'][-1].pop('volumeMounts')
            apply([runner]); ready('kanz-messaging', 'pod/nats-backup-runner')
            # A canonical admitted caller presents its own SPIFFE identity.
            client = copy.deepcopy(runner)
            client['metadata'] = {'name': 'nats-client', 'namespace': 'kanz-services', 'labels': {'app': 'web-bff'}}
            client['spec']['serviceAccountName'] = 'web-bff'
            apply([{'apiVersion': 'v1', 'kind': 'ServiceAccount', 'metadata': {'name': 'web-bff', 'namespace': 'kanz-services'}},
                   copy.deepcopy(next(d for d in bootstrap if d['kind'] == 'ConfigMap' and d['metadata']['name'] == 'nats-bootstrap-spiffe-helper')) | {'metadata': {'name': 'nats-bootstrap-spiffe-helper', 'namespace': 'kanz-services'}}])
            # The sleeping runner does not need the bootstrap script ConfigMap.
            client['spec']['volumes'] = [v for v in client['spec']['volumes'] if v['name'] != 'scripts']
            client['spec']['containers'][0]['volumeMounts'] = [v for v in client['spec']['containers'][0]['volumeMounts'] if v['name'] != 'scripts']
            apply([client]); ready('kanz-services', 'pod/nats-client')
            ungranted = copy.deepcopy(client)
            ungranted['metadata']['name'] = 'nats-ungranted'
            ungranted['spec']['serviceAccountName'] = 'fixture-ungranted'
            apply([{'apiVersion': 'v1', 'kind': 'ServiceAccount', 'metadata': {'name': 'fixture-ungranted', 'namespace': 'kanz-services'}}, ungranted])
            ready('kanz-services', 'pod/nats-ungranted')
            report['stage'] = 'ungranted-identity'
            # Same admitted app label, endpoint and issued trust chain, but no
            # canonical NATS principal. Only the broker's explicit auth error
            # qualifies; DNS/TLS/timeout/command errors cannot pass this control.
            refused = subprocess.run(['kubectl', '--kubeconfig', kubeconfig, '--context', 'kind-' + name,
                '-n', 'kanz-services', 'exec', 'nats-ungranted', '-c', 'bootstrap', '--', 'nats',
                'pub', '--jetstream', 'audit.authority.decision', '{"fixture":true}'],
                capture_output=True, text=True, timeout=30, cwd=ROOT)
            if refused.returncode == 0 or 'authorization violation' not in refused.stderr.lower():
                raise ValueError('explicit broker authorization refusal missing')
            report['cases'].append('ungranted-identity')
            report['stage'] = 'nats-publish'
            k('-n', 'kanz-services', 'exec', 'nats-client', '-c', 'bootstrap', '--',
              'nats', 'pub', '--jetstream', 'audit.authority.decision', '{"fixture":true}')
            report['cases'].append('nats-publish')
            # Restore into this disposable account after removing its streams.
            # This proves the backup runner's network/identity path, not a new
            # cluster, S3/KMS custody, or the production recovery orchestrator.
            report['stage'] = 'backup-restore'
            backup_ip = k('-n', 'kanz-messaging', 'get', 'pod/nats-backup-runner', '-o', 'jsonpath={.status.podIP}')
            ipaddress.ip_address(backup_ip)
            def nats(*args):
                started = datetime.now(timezone.utc).isoformat()
                result = subprocess.run(['kubectl', '--kubeconfig', kubeconfig, '--context', 'kind-' + name,
                    '-n', 'kanz-messaging', 'exec', 'nats-backup-runner', '-c', 'bootstrap', '--', 'nats', *args],
                    capture_output=True, text=True, timeout=300, cwd=ROOT)
                if result.returncode:
                    report['natsFailure'] = nats_failure(result.stdout, result.stderr)
                    if args[:2] == ('account', 'restore'):
                        logs = k('-n', 'kanz-messaging', 'logs', 'nats-0', '-c', 'nats',
                                 '--since-time=' + started, '--tail=100', '--limit-bytes=65536')
                        report['brokerRestoreUploadDenied'] = restore_publish_denied(logs, backup_ip)
                        if report['brokerRestoreUploadDenied']:
                            report['natsFailure'] = 'restore_upload_permission_denied'
                    raise RuntimeError('NATS CLI operation refused; sanitized category recorded')
                return result.stdout.strip()

            for tool, version in [('server', k('-n', 'kanz-messaging', 'exec', 'nats-0', '-c', 'nats', '--', 'nats-server', '--version')),
                                  ('cli', nats('--version'))]:
                report[tool + 'Version'] = tool_version(version)

            def stream_inventory():
                names = json.loads(nats('stream', 'ls', '--json', '--all'))
                if not isinstance(names, list) or not 1 <= len(names) <= 128 or len(set(names)) != len(names):
                    raise ValueError('invalid disposable stream inventory')
                result = {}
                for stream in names:
                    if not isinstance(stream, str) or not re.fullmatch(r'[A-Za-z0-9_-]{1,128}', stream):
                        raise ValueError('invalid disposable stream name')
                    info = json.loads(nats('stream', 'info', stream, '--json'))
                    state = info['state']
                    result[stream] = {'config': info['config'], 'state': {key: state[key] for key in
                        ('messages', 'bytes', 'first_seq', 'last_seq', 'consumer_count')}}
                return result

            before = stream_inventory()
            if before['AUTHORITY_AUDIT']['state']['messages'] != 1:
                raise ValueError('missing acknowledged synthetic record')
            nats('account', 'backup', '--check', '--critical-warnings', '--force', '/tmp/fixture-backup')
            for stream in before:
                nats('stream', 'rm', stream, '--force')
            if json.loads(nats('stream', 'ls', '--json', '--all')) not in (None, []):
                raise ValueError('disposable account not empty before restore')
            nats('account', 'restore', '/tmp/fixture-backup')
            if stream_inventory() != before:
                raise ValueError('restored account differs')
            if nats('stream', 'get', 'AUTHORITY_AUDIT', '1', '--raw') != '{"fixture":true}':
                raise ValueError('restored synthetic record differs')
            report['cases'].append('backup-restore')
            case('redis-claim', 'kanz-services', 'gateway-probe', REDIS_CLAIM)
            # Verify persisted claim after restarting Redis within the same owned Pod/volume.
            before_redis = json.loads(k('-n', 'kanz-messaging', 'get', 'pod/redis', '-o', 'json'))
            before_restarts = before_redis['status']['containerStatuses'][0]['restartCount']
            k('-n', 'kanz-messaging', 'exec', 'redis', '-c', 'redis', '--', 'sh', '-ceu',
              'REDISCLI_AUTH="$(cat /fixture/password)" redis-cli SHUTDOWN NOSAVE')
            for _ in range(60):
                current = json.loads(k('-n', 'kanz-messaging', 'get', 'pod/redis', '-o', 'json'))
                status = current['status']['containerStatuses'][0]
                if current['metadata']['uid'] != before_redis['metadata']['uid']:
                    raise ValueError('Redis pod replaced')
                if status['restartCount'] > before_restarts and status.get('ready'):
                    break
                time.sleep(1)
            else:
                raise ValueError('Redis did not restart')
            k('-n', 'kanz-messaging', 'exec', 'redis', '-c', 'redis', '--', 'sh', '-ceu',
              'REDISCLI_AUTH="$(cat /fixture/password)" redis-cli --raw GET fixture:claim | grep -qx owned')
            report['cases'].append('redis-restart')
            for label, ns, pod in [('unlabelled', 'kanz-services', 'unlabelled'), ('wrong-namespace', 'fixture-wrong', 'wrong')]:
                tcp(label + '-nats', ns, pod, nats_ip, 4222, 'blocked')
                tcp(label + '-redis', ns, pod, redis_ip, 6379, 'blocked')
            tcp('bootstrap-redis', 'kanz-messaging', 'nats-backup-runner', redis_ip, 6379, 'blocked')
            tcp('bootstrap-monitor', 'kanz-messaging', 'nats-backup-runner', monitor_ip, 8222, 'blocked')
            tcp('bootstrap-egress', 'kanz-messaging', 'nats-backup-runner', trap_ip, 8080, 'blocked')
            prom = python_pod('prometheus', 'kanz-observability', 'prometheus')
            prom['spec']['volumes'] = [{'name': 'config', 'configMap': {'name': 'prometheus'}}, {'name': 'data', 'emptyDir': {'sizeLimit': '64Mi'}}]
            prom['spec']['containers'].append({'name': 'prometheus', 'image': PROMETHEUS,
                'args': ['--config.file=/config/prometheus.yml', '--storage.tsdb.path=/data'],
                'resources': copy.deepcopy(RESOURCES), 'securityContext': copy.deepcopy(SECURITY),
                'volumeMounts': [{'name': 'config', 'mountPath': '/config', 'readOnly': True}, {'name': 'data', 'mountPath': '/data'}]})
            apply([{'apiVersion': 'v1', 'kind': 'ConfigMap', 'metadata': {'name': 'prometheus', 'namespace': 'kanz-observability'},
                    'data': {'prometheus.yml': yaml.safe_dump({'global': {'scrape_interval': '1s'}, 'scrape_configs': [
                        {'job_name': 'fixture', 'static_configs': [{'targets': [trap_ip + ':8080']}]}]})}}, prom])
            ready('kanz-observability', 'pod/prometheus')
            case('prometheus-scrape', 'kanz-observability', 'prometheus', '''import json,time,urllib.request
for _ in range(30):
 try:
  r=json.load(urllib.request.urlopen('http://127.0.0.1:9090/api/v1/query?query=fixture_ready',timeout=2))
  if r['status']=='success' and any(v['value'][1]=='1' for v in r['data']['result']):break
 except OSError:pass
 time.sleep(1)
else:raise RuntimeError('missing scrape')
print('PROMETHEUS_PASS')
''')
            for label, address, port in [('nats', nats_ip, 4222), ('redis', redis_ip, 6379), ('monitor', monitor_ip, 8222)]:
                tcp('prometheus-' + label, 'kanz-observability', 'prometheus', address, port, 'blocked')
            # Positive control after every denial, including kubelet health through default-deny.
            k('-n', 'kanz-messaging', 'rollout', 'status', 'statefulset/nats', '--timeout=120s')
            k('-n', 'kanz-messaging', 'exec', 'nats-backup-runner', '-c', 'bootstrap', '--', 'nats', 'stream', 'ls', '--json')
            k('-n', 'kanz-messaging', 'exec', 'redis', '-c', 'redis', '--', 'sh', '-ceu',
              'REDISCLI_AUTH="$(cat /fixture/password)" redis-cli --raw PING | grep -qx PONG')
            # Same gateway network/credential path remains healthy after the
            # restart and all denied probes; localhost alone cannot prove this.
            k('-n', 'kanz-services', 'exec', 'gateway-probe', '-c', 'probe', '--', 'python', '-I', '-B', '-c',
              REDIS_CLAIM.replace("assert ask(b'SET',b'fixture:claim',b'owned',b'NX')==b'+OK\\r\\n'",
                                  "assert ask(b'SET',b'fixture:claim',b'owned',b'NX')==b'$-1\\r\\n'"))
            inventory = json.loads(k('-n', 'kanz-messaging', 'get', 'networkpolicies', '-o', 'json'))['items']
            expected = {p['metadata']['name']: p['spec'] for p in staged_policies()}
            observed = {p['metadata']['name']: p['spec'] for p in inventory}
            # Kubernetes defaults empty ingress/egress to omitted, which is
            # already the staged representation. No extra additive policy may pass.
            if observed != expected:
                raise ValueError('additive policy inventory differs')
            report['policyNames'] = sorted(observed)
            report['policyCount'] = len(observed)
            report['passed'] = True
        except Exception as error:
            report['failureStage'] = report.get('stage', 'initialization')
            report['errorType'] = type(error).__name__
            raise
        finally:
            try:
                feed.command(['kind', 'delete', 'cluster', '--name', name, '--kubeconfig', kubeconfig], timeout=180)
                report['cleanupPassed'] = name not in feed.command(['kind', 'get', 'clusters']).splitlines()
            except Exception:
                report['cleanupPassed'] = False
            report['passed'] &= report['cleanupPassed']
            artifact.write_text(json.dumps(report, indent=2) + '\n')
    validate_report(report, report['sourceSHA'])


if __name__ == '__main__':
    main()
