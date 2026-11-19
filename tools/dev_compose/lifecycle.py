"""Observe the real services, identities, durable state and teardown inside the disconnected VM."""
from contextlib import ExitStack
import json
import os
from pathlib import Path
import re
import subprocess
import socket
import time
import urllib.parse
import urllib.request


REPO = Path('/work/repo')
KANZ = REPO / 'kanz'
PROJECT = os.environ['COMPOSE_PROJECT_NAME']
COMPOSE = ['docker', 'compose', '-f', 'dev/docker-compose.yml']
SERVICES = {'nats', 'nats-init', 'kafka', 'postgres', 'risk-migrate', 'risk-engine', 'api-gateway'}
VOLUMES = {'nats-data', 'postgres-data', 'redpanda-data'}
ENDPOINTS = {('nats', 4222), ('nats', 8222), ('postgres', 5432), ('kafka', 9092),
             ('risk-engine', 8082), ('api-gateway', 8080)}


def run(*args, timeout=60, env=None, data=None, as_developer=False):
    credentials = {'user': 'developer', 'group': 'developer', 'extra_groups': []} if as_developer else {}
    try:
        result = subprocess.run(args, capture_output=True, text=True, cwd=KANZ, env=env, input=data,
                                timeout=timeout, **credentials)
    except subprocess.TimeoutExpired:
        raise RuntimeError(f'{Path(args[0]).name} timed out') from None
    if result.returncode:
        raise RuntimeError(f'{Path(args[0]).name} exited {result.returncode}')
    return result.stdout.strip()


def networks():
    lines = run('docker', 'network', 'ls', '--no-trunc', '--format', '{{.Name}} {{.ID}}').splitlines()
    mapped = dict(line.split() for line in lines)
    if len(mapped) != len(lines) or any(not re.fullmatch(r'[a-f0-9]{64}', ident) for ident in mapped.values()):
        raise ValueError('invalid Docker network inventory')
    return mapped


def baseline():
    actual = networks()
    if set(actual) != {'bridge', 'host', 'none'}:
        raise ValueError('guest did not start with only built-in networks')
    Path('/run/kanz-dev-networks.json').write_text(json.dumps(actual))
    stopped(actual)


def stopped(original):
    if run('docker', 'container', 'ls', '-aq') or networks() != original:
        raise ValueError('unexpected guest containers or networks after shutdown')
    with ExitStack() as stack:
        for _, port in sorted(ENDPOINTS):
            listener = stack.enter_context(socket.socket())
            listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            listener.bind(('127.0.0.1', port))
            listener.listen(1)


def resources(kind):
    lines = run('docker', kind, 'ls', *(['-a'] if kind == 'container' else []), '--filter',
                'label=com.docker.compose.project=' + PROJECT, '--format', '{{.ID}}' if kind != 'volume' else '{{.Name}}')
    return lines.splitlines()


def volume_identity():
    names = resources('volume')
    if set(names) != {PROJECT + '_' + suffix for suffix in VOLUMES} or set(run('docker', 'volume', 'ls', '-q').splitlines()) != set(names):
        raise ValueError('owned volume inventory differs')
    result = {}
    for name in names:
        raw, = json.loads(run('docker', 'volume', 'inspect', name))
        node = Path(raw['Mountpoint']).stat()
        result[name] = (raw['CreatedAt'], node.st_dev, node.st_ino)
    return result


def inspect_services(original):
    ids = resources('container')
    if len(ids) != len(SERVICES) or len(resources('network')) != 1:
        raise ValueError('unexpected Compose container/network count')
    items = json.loads(run('docker', 'inspect', *ids))
    project_network = PROJECT + '_default'
    all_networks = networks()
    if (set(all_networks) != original.keys() | {project_network}
            or any(all_networks.get(k) != v for k, v in original.items())
            or set(run('docker', 'container', 'ls', '-aq').splitlines()) != set(ids)):
        raise ValueError('unexpected guest resource outside the owned project')
    services, ports = set(), set()
    app_dsn = None
    for item in items:
        service = item['Config']['Labels']['com.docker.compose.service']
        services.add(service)
        if set(item['NetworkSettings']['Networks']) != {project_network}:
            raise ValueError('unexpected network attachment')
        if service == 'risk-engine':
            values = [v.removeprefix('RISK_ENGINE_DATABASE_URL=') for v in item['Config']['Env']
                      if v.startswith('RISK_ENGINE_DATABASE_URL=')]
            if len(values) != 1:
                raise ValueError('owned risk service DSN is missing or ambiguous')
            app_dsn = urllib.parse.urlsplit(values[0])
        if service in {'risk-migrate', 'nats-init'}:
            if item['State']['Status'] != 'exited' or item['State']['ExitCode'] != 0:
                raise ValueError('initializer failed: ' + service)
        elif item['State']['Status'] != 'running':
            raise ValueError('service is not running: ' + service)
        for bindings in (item['HostConfig']['PortBindings'] or {}).values():
            for binding in bindings or []:
                if binding['HostIp'] != '127.0.0.1':
                    raise ValueError('non-loopback published service')
                ports.add((service, int(binding['HostPort'])))
    if services != SERVICES or ports != ENDPOINTS:
        raise ValueError('service or published port inventory differs')
    print('Owned containers', {i['Config']['Labels']['com.docker.compose.service']: i['Id'] for i in items},
          'network', all_networks[project_network], flush=True)
    if (app_dsn is None or app_dsn.scheme != 'postgres' or app_dsn.hostname != 'postgres'
            or app_dsn.port != 5432 or app_dsn.username != 'kanzapp' or not app_dsn.password
            or app_dsn.path != '/kanz_risk_dev'):
        raise ValueError('the observed service database binding is outside the developer estate')
    return urllib.parse.unquote(app_dsn.password)


def developer(*args, timeout=180, extra=None):
    env = {'PATH': '/usr/local/go/bin:/home/developer/go/bin:/usr/bin:/bin', 'HOME': '/home/developer',
           'GOPATH': '/home/developer/go', 'GOPROXY': 'off', 'GOTOOLCHAIN': 'local',
           'COMPOSE_PROJECT_NAME': PROJECT, 'GOMAXPROCS': '2', 'GOTMPDIR': str(KANZ / '.gotmp')}
    return run(*args, env={**env, **(extra or {})}, as_developer=True, timeout=timeout)


def integration(package, name, variables):
    events = [json.loads(line) for line in developer('go', 'test', '-p', '1', '-count=1', '-json',
              '-run', '^' + name + '$', package, extra=variables, timeout=300).splitlines()]
    if (any(e['Action'] in ('skip', 'fail') for e in events) or
            sum(e['Action'] == 'pass' and e.get('Test') == name for e in events) != 1):
        raise ValueError('missing real integration pass: ' + name)
    print('Observed real integration', name, flush=True)


def psql(sql, password):
    return run(*COMPOSE, 'exec', '-T', '-e', 'PGPASSWORD', 'postgres', 'psql', '-XqwAt', '-v', 'ON_ERROR_STOP=1',
               '-h', '127.0.0.1', '-U', 'kanzapp', '-d', 'kanz_risk_dev', data=sql,
               env={'PATH': '/usr/bin:/bin', 'HOME': '/root', 'COMPOSE_PROJECT_NAME': PROJECT, 'PGPASSWORD': password})


def durable(password):
    sql = """SET app.tenant_id='dev'; SELECT json_build_array(p.tenant_id,p.portfolio_id,p.position_count,
        p.log_topic,p.log_partition,p.log_offset,(SELECT array_agg(instrument_id ORDER BY instrument_id)
        FROM positions WHERE portfolio_id=p.portfolio_id)) FROM portfolios p WHERE p.portfolio_id='PF1';"""
    deadline = time.monotonic() + 75
    while time.monotonic() < deadline:
        raw = psql(sql, password)
        if raw and json.loads(raw) == ['dev', 'PF1', 4, 'risk.portfolio', 0, 0, ['AAPL', 'MSFT', 'NVDA', 'TSLA']]:
            return json.loads(raw)
        time.sleep(2)
    raise ValueError('durable portfolio, positions or checkpoint mismatch')


def broker():
    expected = set(re.findall(r'^\s+ensure_stream ([A-Z_]+) ', (KANZ / 'infra/nats/bootstrap-job.yaml').read_text(), re.M))
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open('http://127.0.0.1:8222/jsz?streams=true&accounts=true&config=true', timeout=5) as result:
        document = json.load(result)
    streams = {s['name']: s for a in document['account_details'] for s in a.get('stream_detail', [])}
    if len(expected) != 18 or streams.keys() != expected:
        raise ValueError('canonical JetStream inventory differs')
    metadata = json.loads(run(*COMPOSE, 'exec', '-T', 'kafka', 'rpk', 'cluster', 'info', '--format', 'json',
                              '-X', 'brokers=kafka:9092'))
    if metadata.get('brokers') != [{'id': 0, 'host': 'kafka', 'port': 9092}]:
        raise ValueError('internal Kafka listener is not advertised')
    print('Observed 18 JetStream streams and internal Kafka listener', flush=True)


def main():
    if (not re.fullmatch(r'kanzdev[a-f0-9]{12}', PROJECT) or not Path('/run/kanz-lifecycle-authorized').is_file()
            or run('ip', '-4', 'route', 'show', 'default') or run('ip', '-6', 'route', 'show', 'default')):
        raise ValueError('lifecycle requires the disconnected leased guest')
    original = json.loads(Path('/run/kanz-dev-networks.json').read_bytes())
    try:
        password = inspect_services(original)
        volumes = volume_identity()
        broker()
        if psql('SELECT current_user,rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user', password) != 'kanzapp|f|f':
            raise ValueError('runtime database role is not restricted')
        integration('./internal/risk/state/persist', 'TestPostgres_RLSTenantIsolation',
                    {'TEST_POSTGRES_URL': 'postgres://kanzapp:' + urllib.parse.quote(password, safe='') +
                                          '@127.0.0.1:5432/kanzapp?sslmode=disable'})
        integration('./pkg/bus', 'TestKafkaPublishSubscribe', {'TEST_KAFKA_BROKERS': '127.0.0.1:9092'})
        before = durable(password)
        developer('make', 'down')
        stopped(original)
        if volume_identity() != volumes:
            raise ValueError('ordinary shutdown did not preserve only the same three volumes')
        run(*COMPOSE, 'up', '-d', '--no-build', timeout=160)
        developer('python3', 'dev/wait-ready.py')
        print(developer('python3', 'dev/smoke.py'), flush=True)
        password = inspect_services(original)
        broker()
        if volume_identity() != volumes or durable(password) != before:
            raise ValueError('unseeded restart lost durable state')
        print('Observed volume creation times and inode identities', volumes, 'checkpoint', before, flush=True)
    finally:
        developer('make', 'reset', 'CONFIRM=kanz-dev')
        stopped(original)
        if run('docker', 'volume', 'ls', '-q'):
            raise ValueError('owned Compose resources survived explicit reset')
        print('No labeled project containers, network or volumes remain.', flush=True)


if __name__ == '__main__':
    import sys
    if sys.argv[1:] == ['--observe-baseline']:
        baseline()
    else:
        main()
