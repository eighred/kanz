"""Cold backup and recovery of the disposable broker's data PVC."""
import json
import re

import yaml


NAMESPACE = 'kanz-messaging'
SOURCE = 'redpanda-source'
BACKUP = 'redpanda-backup'
RESTORED = 'redpanda-restored'
UUID = r'[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}'


def claim(name):
    return {'apiVersion': 'v1', 'kind': 'PersistentVolumeClaim',
            'metadata': {'name': name, 'namespace': NAMESPACE},
            'spec': {'accessModes': ['ReadWriteOnce'],
                     'resources': {'requests': {'storage': '8Gi'}}}}


def copy_job(name, image, source, destination, script):
    volumes = [{'name': key, 'persistentVolumeClaim': {'claimName': value, 'readOnly': readonly}}
               for key, value, readonly in [('input', source, True), ('output', destination, False)]]
    container = {'name': 'copy', 'image': image, 'command': ['sh', '-ec', script],
                 'volumeMounts': [{'name': 'input', 'mountPath': '/input', 'readOnly': True},
                                  {'name': 'output', 'mountPath': '/output'}],
                 'securityContext': {'allowPrivilegeEscalation': False, 'readOnlyRootFilesystem': True,
                                     'capabilities': {'drop': ['ALL']}},
                 'resources': {'requests': {'cpu': '50m', 'memory': '32Mi'},
                               'limits': {'cpu': '2', 'memory': '256Mi'}}}
    return {'apiVersion': 'batch/v1', 'kind': 'Job', 'metadata': {'name': name, 'namespace': NAMESPACE},
            'spec': {'backoffLimit': 0, 'activeDeadlineSeconds': 300, 'template': {
                'metadata': {'labels': {'redpanda-copy': 'true'}},
                'spec': {'restartPolicy': 'Never', 'automountServiceAccountToken': False,
                         'securityContext': {'runAsNonRoot': True, 'runAsUser': 101, 'runAsGroup': 101,
                                             'fsGroup': 101, 'seccompProfile': {'type': 'RuntimeDefault'}},
                         'containers': [container], 'volumes': volumes}}}}


def evidence(result):
    if not isinstance(result, dict) or set(result) != {'claims', 'sourcePod', 'restoredPod', 'backupSHA256', 'restoreSHA256'}:
        raise ValueError('incomplete cold restore evidence')
    claims = result['claims']
    if not isinstance(claims, dict) or set(claims) != {SOURCE, BACKUP, RESTORED}:
        raise ValueError('missing recovery claims')
    for entry in claims.values():
        if (not isinstance(entry, dict) or set(entry) != {'uid', 'volume'}
                or not re.fullmatch(UUID, str(entry['uid']))
                or not re.fullmatch(r'pvc-' + UUID, str(entry['volume']))):
            raise ValueError('invalid bound fixture claim')
    if (len({entry['uid'] for entry in claims.values()}) != 3
            or len({entry['volume'] for entry in claims.values()}) != 3
            or not all(re.fullmatch(UUID, str(result[field])) for field in ('sourcePod', 'restoredPod'))
            or result['sourcePod'] == result['restoredPod']
            or not re.fullmatch(r'[0-9a-f]{64}', str(result['backupSHA256']))
            or result['backupSHA256'] != result['restoreSHA256']):
        raise ValueError('recovery storage, pod or archive identity mismatch')


def recover(kube, image):
    def k(*args, **kwargs):
        return kube('-n', NAMESPACE, *args, **kwargs)

    def broker(expected_claim):
        pods = json.loads(k('get', 'pods', '-l', 'app=redpanda', '-o', 'json'))['items']
        pod, = pods
        data, = (v for v in pod['spec']['volumes'] if v['name'] == 'data')
        if (data.get('persistentVolumeClaim', {}).get('claimName') != expected_claim
                or pod['metadata'].get('deletionTimestamp')):
            raise ValueError('broker did not mount the intended claim')
        return pod['metadata']

    source = broker(SOURCE)
    k('scale', 'deployment/redpanda', '--replicas=0')
    k('wait', '--for=delete', 'pod/' + source['name'], '--timeout=120s')
    for name in (BACKUP, RESTORED):
        k('create', '-f', '-', data=yaml.safe_dump(claim(name)))
    k('create', '-f', '-', data=yaml.safe_dump({
        'apiVersion': 'networking.k8s.io/v1', 'kind': 'NetworkPolicy',
        'metadata': {'namespace': NAMESPACE, 'name': 'offline-redpanda-copy'},
        'spec': {'podSelector': {'matchLabels': {'redpanda-copy': 'true'}}, 'policyTypes': ['Ingress', 'Egress']}}))

    copies = [
        copy_job('backup-broker', image, SOURCE, BACKUP,
                 'test -d /input/state\ntest -z "$(ls -A /output)"\n'
                 'tar -czf /output/state.tar.gz -C /input state\n'
                 'sha256sum /output/state.tar.gz'),
        copy_job('restore-broker', image, BACKUP, RESTORED,
                 'test -s /input/state.tar.gz\ntest -z "$(ls -A /output)"\n'
                 'tar -xzf /input/state.tar.gz -C /output\n'
                 'sha256sum /input/state.tar.gz')]
    checksums = []
    for job in copies:
        name = 'job/' + job['metadata']['name']
        k('create', '-f', '-', data=yaml.safe_dump(job))
        k('wait', '--for=condition=complete', name, '--timeout=280s')
        checksum = re.fullmatch(r'([0-9a-f]{64})  /(?:input|output)/state\.tar\.gz', k('logs', name))
        if not checksum:
            raise ValueError('missing offline archive checksum')
        checksums.append(checksum[1])
    if checksums[0] != checksums[1]:
        raise ValueError('backup changed before restore')
    claims = {}
    for name in (SOURCE, BACKUP, RESTORED):
        pvc = json.loads(k('get', 'pvc', name, '-o', 'json'))
        if pvc['status']['phase'] != 'Bound':
            raise ValueError('expected a bound fixture claim')
        claims[name] = {'uid': pvc['metadata']['uid'], 'volume': pvc['spec']['volumeName']}
    k('patch', 'deployment/redpanda', '--type=strategic', '-p', json.dumps({
        'spec': {'replicas': 1, 'template': {'spec': {'volumes': [
            {'name': 'data', 'persistentVolumeClaim': {'claimName': RESTORED}}]}}}}))
    k('rollout', 'status', 'deployment/redpanda', '--timeout=240s')
    result = {'claims': claims, 'sourcePod': source['uid'], 'restoredPod': broker(RESTORED)['uid'],
              'backupSHA256': checksums[0], 'restoreSHA256': checksums[1]}
    evidence(result)
    return result
