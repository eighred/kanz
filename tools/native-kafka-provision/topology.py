"""Owned disposable three-broker topology; never an active deployment manifest."""
import copy
import hashlib
import importlib.util
from pathlib import Path
import sys

import yaml

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'tools/feed-identity'))
_spec = importlib.util.spec_from_file_location('retained_redpanda_identity', ROOT / 'tools/redpanda-identity/qualify.py')
retained = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(retained)

BROKER_ID = 'spiffe://kanz.internal/ns/kanz-messaging/sa/redpanda'
ADMIN_ID = 'spiffe://kanz.internal/ns/kanz-messaging/sa/kafka-provisioner'
CLIENTS = {
    'tenant-a': ('tenant-acme', 'risk-engine'),
    'tenant-b': ('tenant-bravo', 'risk-engine'),
    'compute-a': ('kanz-services', 'archiver-acme'),
    'ungranted': ('tenant-acme', 'ungranted'),
}
IDENTITIES = (ADMIN_ID,) + tuple('spiffe://kanz.internal/ns/' + ns + '/sa/' + sa for ns, sa in CLIENTS.values())
BROKERS = tuple('native-redpanda-' + str(i) for i in range(3))
BROKER_MEMORY_BYTES = 2 << 30
DATA_VOLUME_BYTES = 2 << 30
FALLOC_BYTES = 1 << 20


def broker_principal(identity):
    if identity not in IDENTITIES:
        raise ValueError('unowned fixture principal')
    # The rule parser retains replacement escapes, and std::regex_replace
    # preserves them. This is a wire encoding, not a different SPIFFE identity.
    return identity.replace('/', r'\/')


def mapping(identity):
    replacement=broker_principal(identity)
    # SPIRE CredentialComposer uniqueid binds the complete URI, not a caller CN.
    unique = hashlib.sha256(identity.encode()).digest()[:16].hex()
    dn = 'C=US,O=SPIRE,CN=[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?,x500UniqueIdentifier=' + unique
    return 'RULE:^' + dn + '$/' + replacement + '/'


def broker_objects():
    original = retained.broker_objects()
    template = next(d for d in original if d['kind'] == 'Deployment')
    base = next(d for d in original if d['kind'] == 'ConfigMap' and d['metadata']['name'] == 'redpanda-config')
    helper = next(d for d in original if d['kind'] == 'ConfigMap' and d['metadata']['name'] == 'redpanda-helper')
    docs = [copy.deepcopy(d) for d in original if d['kind'] in ('Namespace', 'ServiceAccount')]
    docs.append(copy.deepcopy(helper))
    docs.append({'apiVersion': 'v1', 'kind': 'ServiceAccount', 'metadata': {'namespace': 'kanz-messaging', 'name': 'kafka-provisioner'}})
    for ns in ('tenant-acme', 'tenant-bravo'):
        docs.append({'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': ns, 'labels': {'kanz.internal/spiffe': 'enabled'}}})
    for ns, sa in CLIENTS.values():
        docs.append({'apiVersion': 'v1', 'kind': 'ServiceAccount', 'metadata': {'namespace': ns, 'name': sa}})
    for index, name in enumerate(BROKERS):
        config = yaml.safe_load(base['data']['redpanda.yaml'])
        rp = config['redpanda']
        rp['node_id'] = index
        rp['seed_servers'] = [{'host': {'address': n + '.kanz-messaging.svc', 'port': 33145}} for n in BROKERS]
        rp['rpc_server'] = {'address': '0.0.0.0', 'port': 33145}
        rp['advertised_rpc_api'] = {'address': name + '.kanz-messaging.svc', 'port': 33145}
        rp['advertised_kafka_api'][0]['address'] = name + '.kanz-messaging.svc'
        bootstrap = {'kafka_enable_authorization': True, 'superusers': [broker_principal(ADMIN_ID)],
                     'auto_create_topics_enabled': False, 'default_topic_replications': 3,
                     'write_caching_default': 'false',
                     'segment_fallocation_step': FALLOC_BYTES,
                     'kafka_mtls_principal_mapping_rules': [mapping(x) for x in IDENTITIES]}
        cm = copy.deepcopy(base)
        cm['metadata']['name'] = name
        cm['data'] = {'redpanda.yaml': yaml.safe_dump(config), '.bootstrap.yaml': yaml.safe_dump(bootstrap)}
        pod = copy.deepcopy(template)
        pod['metadata']['name'] = name
        pod['spec']['selector']['matchLabels'] = {'native-broker': name}
        pod['spec']['template']['metadata']['labels'] = {'native-broker': name, 'app': name}
        spec = pod['spec']['template']['spec']
        broker=next(c for c in spec['containers'] if c['name']=='redpanda')
        broker['args'].append('--unsafe-bypass-fsync=false')
        broker['args'][broker['args'].index('--memory')+1]='2G'
        broker['resources']['requests']['memory']='2Gi'
        broker['resources']['limits']['memory']='3Gi'
        next(v for v in spec['volumes'] if v['name'] == 'config')['configMap']['name'] = name
        # The full table creates 693 replicas per broker across three tenants.
        # Reserve memory for them and use small allocation steps for the sparse
        # functional fixture; retain the finite volume and all durability gates.
        svc = {'apiVersion': 'v1', 'kind': 'Service', 'metadata': {'name': name, 'namespace': 'kanz-messaging'},
               'spec': {'selector': {'native-broker': name}, 'publishNotReadyAddresses': True,
                        'ports': [{'name': 'kafka', 'port': 9094}, {'name': 'rpc', 'port': 33145}]}}
        docs.extend((cm, svc, pod))
    return docs


def policies(connected=BROKERS):
    # Auth remains actual mTLS/ACL. Cilium permits only the fixture namespaces
    # plus broker-to-broker replication; no production NetworkPolicy claim.
    if not isinstance(connected,tuple) or len(set(connected))!=len(connected) or any(b not in BROKERS for b in connected):
        raise ValueError('unowned replication membership')
    peers = [{'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': ns}}}
             for ns in ('kanz-messaging', 'kanz-services', 'tenant-acme', 'tenant-bravo')]
    return [{'apiVersion': 'networking.k8s.io/v1', 'kind': 'NetworkPolicy',
             'metadata': {'name': 'native-broker-ingress', 'namespace': 'kanz-messaging'},
             'spec': {'podSelector': {}, 'policyTypes': ['Ingress'], 'ingress': [
                 {'from': peers, 'ports': [{'protocol': 'TCP', 'port': 9094}]}]}},
            {'apiVersion':'networking.k8s.io/v1','kind':'NetworkPolicy',
             'metadata':{'name':'native-replication','namespace':'kanz-messaging'},
             'spec':{'podSelector':{'matchExpressions':[{'key':'native-broker','operator':'In','values':list(connected)}]} if connected else {},
                     'policyTypes':['Ingress'],
                     'ingress':[{'from':[{'podSelector':{'matchExpressions':[{'key':'native-broker','operator':'In','values':list(connected)}]}}],
                                 'ports':[{'protocol':'TCP','port':33145}]}] if connected else []}}]
