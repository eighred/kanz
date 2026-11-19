"""Observe the real services, identities, durable state and teardown inside the disconnected VM."""
import json
import os
from pathlib import Path
import re
import subprocess
import time
import urllib.request


REPO = Path('/work/repo')
KANZ = REPO / 'kanz'
PROJECT = os.environ['COMPOSE_PROJECT_NAME']
COMPOSE = ['docker', 'compose', '-f', 'dev/docker-compose.yml']
SERVICES = {'nats', 'nats-init', 'kafka', 'postgres', 'risk-migrate', 'risk-engine', 'api-gateway'}
VOLUMES = {'nats-data', 'postgres-data', 'redpanda-data'}
ENDPOINTS = {('nats', 4222), ('nats', 8222), ('postgres', 5432), ('kafka', 9092),
             ('risk-engine', 8082), ('api-gateway', 8080)}


def run(*args, timeout=60, env=None):
    return subprocess.run(args, capture_output=True, text=True, cwd=KANZ,
                          env=env, timeout=timeout, check=True).stdout.strip()


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


def inspect_services():
    ids = resources('container')
    if len(ids) != len(SERVICES) or len(resources('network')) != 1:
        raise ValueError('unexpected Compose container/network count')
    items = json.loads(run('docker', 'inspect', *ids))
    services, ports = set(), set()
    for item in items:
        service = item['Config']['Labels']['com.docker.compose.service']
        services.add(service)
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
          'network', resources('network'), flush=True)


def developer(*args, timeout=180, extra=None):
    env = {'PATH': '/usr/local/go/bin:/home/developer/go/bin:/usr/bin:/bin', 'HOME': '/home/developer',
           'GOPATH': '/home/developer/go', 'GOPROXY': 'off', 'GOTOOLCHAIN': 'local',
           'COMPOSE_PROJECT_NAME': PROJECT, 'GOMAXPROCS': '2', 'GOTMPDIR': str(KANZ / '.gotmp')}
    return run('runuser', '-u', 'developer', '--', 'env', '-i', *(k + '=' + v for k, v in {**env, **(extra or {})}.items()),
               *args, timeout=timeout)


def integration(package, name, variables):
    events = [json.loads(line) for line in developer('go', 'test', '-p', '1', '-count=1', '-json',
              '-run', '^' + name + '$', package, extra=variables, timeout=300).splitlines()]
    if (any(e['Action'] in ('skip', 'fail') for e in events) or
            sum(e['Action'] == 'pass' and e.get('Test') == name for e in events) != 1):
        raise ValueError('missing real integration pass: ' + name)
    print('Observed real integration', name, flush=True)


def psql(sql):
    return run(*COMPOSE, 'exec', '-T', '-e', 'PGPASSWORD=kanz', 'postgres', 'psql', '-XqwAt', '-v', 'ON_ERROR_STOP=1',
               '-h', '127.0.0.1', '-U', 'kanzapp', '-d', 'kanz_risk_dev', '-c', sql)


def durable():
    sql = """SET app.tenant_id='dev'; SELECT json_build_array(p.tenant_id,p.portfolio_id,p.position_count,
        p.log_topic,p.log_partition,p.log_offset,(SELECT array_agg(instrument_id ORDER BY instrument_id)
        FROM positions WHERE portfolio_id=p.portfolio_id)) FROM portfolios p WHERE p.portfolio_id='PF1';"""
    deadline = time.monotonic() + 75
    while time.monotonic() < deadline:
        raw = psql(sql)
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
    try:
        inspect_services()
        volumes = volume_identity()
        broker()
        if psql('SELECT current_user,rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user') != 'kanzapp|f|f':
            raise ValueError('runtime database role is not restricted')
        integration('./internal/risk/state/persist', 'TestPostgres_RLSTenantIsolation',
                    {'TEST_POSTGRES_URL': 'postgres://kanzapp:kanz@127.0.0.1:5432/kanzapp?sslmode=disable'})
        integration('./pkg/bus', 'TestKafkaPublishSubscribe', {'TEST_KAFKA_BROKERS': '127.0.0.1:9092'})
        before = durable()
        developer('make', 'down')
        if resources('container') or resources('network') or volume_identity() != volumes:
            raise ValueError('ordinary shutdown did not preserve only the same three volumes')
        run(*COMPOSE, 'up', '-d', '--no-build', timeout=160)
        developer('python3', 'dev/wait-ready.py')
        print(developer('python3', 'dev/smoke.py'), flush=True)
        inspect_services()
        broker()
        if volume_identity() != volumes or durable() != before:
            raise ValueError('unseeded restart lost durable state')
        print('Observed volume creation times and inode identities', volumes, 'checkpoint', before, flush=True)
    finally:
        developer('make', 'reset', 'CONFIRM=kanz-dev')
        if (any(resources(kind) for kind in ('volume', 'container', 'network'))
                or run('docker', 'container', 'ls', '-aq') or run('docker', 'volume', 'ls', '-q')):
            raise ValueError('owned Compose resources survived explicit reset')
        print('No labeled project containers, network or volumes remain.', flush=True)


if __name__ == '__main__':
    main()
