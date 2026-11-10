"""Exercise the tracked STEP=all script against real disposable services."""
import base64
from contextlib import contextmanager, ExitStack
import hashlib
import hmac
import json
import os
import secrets
import socket
import subprocess
import time
import urllib.error
import urllib.request

import onboarding_postgres as pg

TENANTS = {'acme': 'PFACME', 'bravo': 'PFBRAVO'}
INSTRUMENTS = ['AAPL', 'MSFT', 'NVDA', 'TSLA']
REQUIRED = ('riskProjection', 'postgresBefore', 'postgresAfterRestart',
            'signedSelf', 'signedCrossRejected')


def validate(evidence):
    if (not isinstance(evidence, dict)
            or set(evidence) != {'step', 'tenant', 'postgresRestartCount', *REQUIRED}
            or evidence['step'] != 'all' or evidence['tenant'] != 'acme'
            or type(evidence['postgresRestartCount']) is not int
            or evidence['postgresRestartCount'] != 1
            or any(evidence[key] is not True for key in REQUIRED)):
        raise ValueError('composed onboarding evidence incomplete')


def bind_address():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return '127.0.0.1:'+str(sock.getsockname()[1])


@contextmanager
def process(executable, env, scratch):
    child = subprocess.Popen([str(executable)], cwd=scratch, env=env, stdin=subprocess.DEVNULL,
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        yield child
    finally:
        if child.poll() is None:
            child.terminate()
            try:
                child.wait(timeout=7)
            except subprocess.TimeoutExpired:
                child.kill()
                child.wait(timeout=5)


def ready(url, child):
    deadline = time.monotonic()+25
    client = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    while child.poll() is None and time.monotonic() < deadline:
        try:
            with client.open(url, timeout=2) as response:
                if response.status == 200:
                    return
        except (urllib.error.URLError, TimeoutError):
            time.sleep(0.25)
    raise ValueError('owned risk or gateway process not ready')


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def exposure(gateway, portfolio, token, key):
    path = f'/v1/portfolios/{portfolio}/exposure'
    signature = hmac.digest(key.encode(), f'GET\n{path}\n'.encode(), hashlib.sha256)
    req = urllib.request.Request(gateway+path, headers={
        'Authorization': 'Bearer '+token,
        'X-Signature': base64.urlsafe_b64encode(signature).rstrip(b'=').decode('ascii'),
    })
    client = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    try:
        with client.open(req, timeout=3) as response:
            return response.status, json.loads(response.read(65536))
    except urllib.error.HTTPError as error:
        return error.code, None


def self_read(gateway, tenant, token, key):
    deadline = time.monotonic()+25
    while time.monotonic() < deadline:
        status, result = exposure(gateway, TENANTS[tenant], token, key)
        if status == 200:
            instruments = sorted(item['bucket'] for item in result['set']['exposures']
                                 if item['dimension'] == 'EXPOSURE_DIMENSION_INSTRUMENT')
            if (result['portfolio_id'] != TENANTS[tenant] or result['owner_tenant'] != tenant
                    or instruments != INSTRUMENTS):
                raise ValueError('signed self response differs from seed')
            return
        if status != 404:
            raise ValueError(f'signed self read refused with status {status}')
        time.sleep(0.5)
    raise TimeoutError('seed did not reach owned risk projection')


@contextmanager
def services(scratch, nats, dsn, paths, jwt, signing, env):
    gateways = {}
    with ExitStack() as stack:
        for tenant in TENANTS:
            grpc, health, http = bind_address(), bind_address(), bind_address()
            risk = stack.enter_context(process(paths['risk'], dict(env,
                RISK_ENGINE_NATS_URL=nats.url,
                RISK_ENGINE_NATS_CREDS_FILE=str(nats.scratch/(tenant+'.creds')),
                RISK_ENGINE_LISTEN=health, RISK_ENGINE_GRPC_LISTEN=grpc,
                RISK_ENGINE_DATABASE_URL=dsn, RISK_ENGINE_TENANT=tenant,
                RISK_ENGINE_SNAPSHOT_INTERVAL='1s'), scratch))
            ready('http://'+health+'/readyz', risk)
            gateway = stack.enter_context(process(paths['gateway'], dict(env,
                API_GATEWAY_LISTEN=http, API_GATEWAY_RISK_ENGINE_ADDR=grpc,
                API_GATEWAY_ALLOW_DEV_HS256='true', API_GATEWAY_JWT_SECRET=jwt,
                API_GATEWAY_SIGNING_SECRET=signing,
                API_GATEWAY_REQUIRED_ROLE='kanz-user',
                API_GATEWAY_TRADE_ROLE='kanz-trader'), scratch))
            gateways[tenant] = 'http://'+http
            ready(gateways[tenant]+'/readyz', gateway)
        yield gateways


def qualify(root, scratch, api, apply, cluster, nats, caller_env, source, run):
    fixture = scratch/'onboarding-all'
    fixture.mkdir(mode=0o700)
    # STEP=compute writes rendered manifests. Run the exact source in disposable
    # scratch and keep generated SDK input linked to its existing CI output.
    archive = fixture/'source.tar'
    checkout = fixture/'source'
    checkout.mkdir()
    run(['git', 'archive', '--format=tar', '-o', str(archive), source], cwd=root, env=os.environ, timeout=45)
    run(['tar', '-xf', str(archive), '-C', str(checkout)], cwd=root, env=os.environ, timeout=90)
    (checkout/'kanz-schemas/gen').symlink_to(root/'kanz-schemas/gen', target_is_directory=True)

    env = {key: os.environ[key] for key in ('PATH', 'HOME', 'GOCACHE', 'GOMODCACHE',
           'GOPATH', 'GOTOOLCHAIN', 'GOFLAGS', 'TMPDIR') if key in os.environ}
    paths = {}
    for name, package in (
        ('risk', './services/risk-engine/cmd/risk-engine'),
        ('gateway', './services/api-gateway/cmd/api-gateway'),
        ('token', './cmd/kanz-devtoken'),
        ('seed', './test/load/seed'),
    ):
        paths[name] = fixture/name
        run(['go', 'build', '-o', str(paths[name]), package], cwd=root/'kanz', env=env, timeout=180)
    jwt, signing = secrets.token_hex(32), secrets.token_hex(32)
    tokens = {tenant: run([str(paths['token']), '--tenant', tenant, '--subject', 'fixture-'+tenant],
                          cwd=fixture, env=dict(env, API_GATEWAY_JWT_SECRET=jwt), timeout=10)
              for tenant in TENANTS}
    seed_creds = {}
    for tenant in TENANTS:
        nats.observe(tenant, 'projection-prepare')
        seed_creds[tenant] = str(nats.publisher(tenant))
    pg.install(apply, api)
    kubeconfig = scratch/'kubeconfig'
    with pg.endpoint(kubeconfig, cluster, 'kanz-books-1') as books:
        pg.migrate(run, root, books, 'accounting', env)
    with pg.endpoint(kubeconfig, cluster, 'kanz-risk-1') as risk:
        pg.migrate(run, root, risk, 'risk-engine', env)
        with services(fixture, nats, risk, paths, jwt, signing, env) as gateways:
            # Establish the other tenant's positive control using the same
            # canonical publisher, then invoke the real composed command once.
            run([str(paths['seed'])], cwd=fixture, env=dict(env, SEED_TENANT='bravo',
                SEED_PORTFOLIO=TENANTS['bravo'], SEED_NATS_URL=nats.url,
                SEED_NATS_CREDS_FILE=seed_creds['bravo']), timeout=30)
            self_read(gateways['bravo'], 'bravo', tokens['bravo'], signing)
            output = run(['sh', './provision-tenant.sh'], cwd=checkout/'kanz/infra/onboarding',
                         env=dict(env, **{**caller_env, 'STEP': 'all', 'TENANT': 'acme',
                             'FIRST_PORTFOLIO': TENANTS['acme'], 'RISK_DB_NAME': 'risk', 'BOOKS_DB_NAME': 'books',
                             'SEED_NATS_URL': nats.url, 'SEED_NATS_CREDS_FILE': seed_creds['acme'],
                             'GW': gateways['acme'], 'VERIFY_TOKEN': tokens['acme'],
                             'VERIFY_TOKEN_OTHER': tokens['bravo'], 'VERIFY_SIGNING_SECRET': signing}),
                         timeout=360, stdout_limit=65536)
            if "tenant 'acme' provisioned (step(s) 'all')" not in output:
                raise ValueError('composed provisioning completion missing')
            assert_reads(gateways, tokens, signing)
            for tenant, portfolio in TENANTS.items():
                nats.observe(tenant, 'seed-read', portfolio)
            assert_storage(api, wait=True)
    # Risk processes and DB forwarding have stopped. Inspect the database before
    # risk reconnects so NATS redelivery cannot hide a missing persisted record.
    restarts = pg.restart(api, 'kanz-risk-1')
    assert_storage(api, wait=False)
    with pg.endpoint(kubeconfig, cluster, 'kanz-risk-1') as risk:
        with services(fixture, nats, risk, paths, jwt, signing, env) as gateways:
            assert_reads(gateways, tokens, signing)
    return {'step': 'all', 'tenant': 'acme', 'riskProjection': True,
            'postgresBefore': True, 'postgresAfterRestart': True,
            'postgresRestartCount': restarts, 'signedSelf': True, 'signedCrossRejected': True}


def assert_reads(gateways, tokens, key):
    for tenant, other in (('acme', 'bravo'), ('bravo', 'acme')):
        self_read(gateways[tenant], tenant, tokens[tenant], key)
        status, _ = exposure(gateways[tenant], TENANTS[tenant], tokens[other], key)
        if status not in (403, 404):
            raise ValueError(f'unexpected signed cross-tenant status {status}')


def assert_storage(api, *, wait):
    for tenant, other in (('acme', 'bravo'), ('bravo', 'acme')):
        deadline = time.monotonic()+(25 if wait else 0)
        while pg.positions(api, tenant, TENANTS[tenant]) != INSTRUMENTS:
            if time.monotonic() >= deadline:
                raise ValueError('owned PostgreSQL snapshot absent')
            time.sleep(0.5)
        if pg.positions(api, tenant, TENANTS[other]):
            raise ValueError('owned PostgreSQL returned another tenant snapshot')
