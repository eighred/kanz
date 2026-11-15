"""PostgreSQL storage for the composed onboarding proof in the owned kind cluster."""
from contextlib import contextmanager
import json
import os
import re
import subprocess
import tempfile
import time

POSTGRES = 'postgres:16-alpine@sha256:1fb72075a1de85a1d39de23c2da81baef1b71bf5c0b61a14cbbfdb072a6246d9'
NAMESPACE = 'kanz-data'
DATABASES = {'kanz-risk-1': 'risk', 'kanz-books-1': 'books'}
ROLE = 'onboarding_probe'
PASSWORD = 'disposable-onboarding'


def objects():
    """Fixture-only pods and named exec access for the existing caller identity."""
    yield {'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': NAMESPACE}}
    for pod, db in DATABASES.items():
        yield {
            'apiVersion': 'v1', 'kind': 'Pod',
            'metadata': {'name': pod, 'namespace': NAMESPACE},
            'spec': {
                'automountServiceAccountToken': False,
                'containers': [{
                    'name': 'postgres', 'image': POSTGRES,
                    'env': [{'name': key, 'value': value} for key, value in (
                        ('POSTGRES_DB', db), ('POSTGRES_PASSWORD', PASSWORD), ('PGUSER', 'postgres'),
                        ('PGDATA', '/var/lib/postgresql/data/fixture'))],
                    'readinessProbe': {'tcpSocket': {'port': 5432}, 'periodSeconds': 2},
                    'volumeMounts': [{'name': 'data', 'mountPath': '/var/lib/postgresql/data'}],
                }],
                'volumes': [{'name': 'data', 'emptyDir': {}}],
            },
        }
    role = 'onboarding-storage-probe'
    yield {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'Role',
           'metadata': {'name': role, 'namespace': NAMESPACE},
           'rules': [{'apiGroups': [''], 'resources': ['pods'], 'resourceNames': list(DATABASES), 'verbs': ['get']},
                     {'apiGroups': [''], 'resources': ['pods/exec'], 'resourceNames': list(DATABASES),
                      'verbs': ['create']}]}
    yield {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'RoleBinding',
           'metadata': {'name': role, 'namespace': NAMESPACE},
           'roleRef': {'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'Role', 'name': role},
           'subjects': [{'kind': 'ServiceAccount', 'name': 'tenant-provisioner', 'namespace': 'kanz-tenancy'}]}


def install(apply, api):
    if api('get', 'namespace', NAMESPACE, '--ignore-not-found', '-o', 'name', timeout=10):
        raise ValueError('PostgreSQL fixture namespace already exists')
    apply(list(objects()))
    for pod, db in DATABASES.items():
        api('-n', NAMESPACE, 'wait', '--for=condition=Ready', 'pod/'+pod, '--timeout=100s', timeout=110)
        sql = (f"CREATE ROLE {ROLE} LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '{PASSWORD}'; "
               f"ALTER DATABASE {db} OWNER TO {ROLE}")
        api('-n', NAMESPACE, 'exec', pod, '--', 'psql', '-Xw', '-d', db,
            '-v', 'ON_ERROR_STOP=1', '-c', sql, timeout=20)


@contextmanager
def endpoint(kubeconfig, cluster, pod):
    """Use a kubectl-allocated port on loopback. Re-open after server restart."""
    argv = ['kubectl', '--kubeconfig', str(kubeconfig), '--context', 'kind-'+cluster,
            '-n', NAMESPACE, 'port-forward', '--address', '127.0.0.1', 'pod/'+pod, ':5432']
    with tempfile.TemporaryDirectory() as logs:
        output_path = os.path.join(logs, 'forward.log')
        with open(output_path, 'wb') as output:
            process = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=output, stderr=output)
            try:
                deadline = time.monotonic()+15
                while process.poll() is None and time.monotonic() < deadline:
                    with open(output_path, 'rb') as log:
                        match = re.search(rb'Forwarding from 127\.0\.0\.1:(\d+) -> 5432', log.read(8192))
                    if match:
                        port = int(match[1])
                        yield f'postgres://{ROLE}:{PASSWORD}@127.0.0.1:{port}/{DATABASES[pod]}?sslmode=disable'
                        return
                    time.sleep(0.1)
                raise ValueError('owned PostgreSQL forwarding unavailable')
            finally:
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=5)


def migrate(command, root, dsn, service, env):
    command(['go', 'run', './cmd/kanz-migrate', '--dir', f'services/{service}/migrations'],
            cwd=root/'kanz', env=dict(env, KANZ_MIGRATE_DATABASE_URL=dsn), timeout=120)


def identity(api, pod):
    status = json.loads(api('-n', NAMESPACE, 'get', 'pod', pod, '-o', 'json', timeout=10))
    [container] = status['status']['containerStatuses']
    return status['metadata']['uid'], container['restartCount'], container['ready']


def restart(api, pod):
    uid, count, _ = identity(api, pod)
    # The official postgres entrypoint execs the server as PID 1. SIGINT gives
    # it a fast shutdown. Kubernetes restarts the container on the same pod and
    # therefore retains its emptyDir. Verify identity and count, not just health.
    api('-n', NAMESPACE, 'exec', pod, '--', 'sh', '-c',
        '(sleep 1; kill -INT 1) </dev/null >/dev/null 2>&1 &', timeout=10)
    deadline = time.monotonic()+100
    while time.monotonic() < deadline:
        current_uid, current_count, ready = identity(api, pod)
        if current_uid != uid or current_count > count+1:
            raise ValueError('unexpected PostgreSQL pod replacement or repeated restart')
        if current_count == count+1 and ready:
            return current_count
        time.sleep(1)
    raise TimeoutError('PostgreSQL restart did not become ready')


def positions(api, tenant, portfolio):
    # Both values are internal fixture constants. Do not accept arbitrary SQL
    # fragments or propagate the admin connection into application probes.
    if not re.fullmatch('[a-z]+', tenant) or not re.fullmatch('[A-Z0-9]+', portfolio):
        raise ValueError('unexpected fixture tenant or portfolio')
    sql = (f"SET ROLE {ROLE}; SET app.tenant_id = '{tenant}'; "
           f"SELECT instrument_id FROM positions WHERE portfolio_id = '{portfolio}' ORDER BY instrument_id")
    return api('-n', NAMESPACE, 'exec', 'kanz-risk-1', '--', 'psql', '-XwqtA', '-d', 'risk',
               '-v', 'ON_ERROR_STOP=1', '-c', sql, timeout=10).splitlines()
