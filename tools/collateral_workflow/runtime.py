"""Exercise one released collateral workflow on an isolated, disposable Docker estate."""
from datetime import datetime, timezone, timedelta
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import sys
import tempfile
import time
import uuid

from release import reference


ROOT = Path(__file__).resolve().parents[2]
PINS = json.loads((ROOT / 'tools/dev_compose/images.json').read_bytes())
DATABASE = 'docker.io/library/postgres:16-alpine'
BROKER = 'docker.io/library/nats:2.10-alpine'
BOX = 'docker.io/natsio/nats-box:0.14.5'


def public(ref):
    return ref.rsplit(':', 1)[0] + '@sha256:' + PINS[ref]


def docker(stage, *args, env=None, data=None, timeout=90):
    # A subprocess error must never echo its arguments, environment or raw service logs.
    try:
        result = subprocess.run(['docker', *args], env={**os.environ, **(env or {})}, input=data,
                                capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        raise RuntimeError(stage + ' timed out') from None
    if result.returncode:
        detail = result.stderr.strip()
        if re.fullmatch(r'gateway (?:propose|self-approve|approve|instructed|settled) returned [1-5][0-9]{2}, expected [1-5][0-9]{2}', detail):
            raise RuntimeError(stage + ': ' + detail)
        raise RuntimeError(f'{stage} exited {result.returncode}')
    return result.stdout.strip()


def wait_for(stage, check, timeout=55):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            if check():
                return
        except RuntimeError as exc:
            last = exc
        time.sleep(1)
    raise RuntimeError(stage + ' did not converge') from last


def nats_configuration():
    # Use the deployed Accounting principal's actual permissions. The fixture's
    # two external sources may publish only their respective collateral FACT.
    tenancy = (ROOT / 'kanz/infra/nats/tenancy.yaml').read_text()
    owner = '{ user: "spiffe://kanz.internal/ns/kanz-services/sa/accounting"'
    if tenancy.count(owner) != 1:
        raise ValueError('Accounting NATS principal is missing or ambiguous')
    body = '\n'.join(line.split('#', 1)[0] for line in tenancy.split(owner)[1].split('\n          }', 1)[0].splitlines())
    grants = {}
    for direction in ('publish', 'subscribe'):
        found = re.findall(direction + r':\s*\{\s*allow:\s*(\[[^\]]*\])', body)
        if len(found) != 1:
            raise ValueError('Accounting permissions changed')
        grants[direction] = {'allow': json.loads(re.sub(r',\s*\]', ']', found[0]))}
    lines = ['port: 4222', 'jetstream { store_dir: "/data" }', 'authorization { users: [',
             '{user: admin, password: $ADMIN_KEY},',
             '{user: accounting, password: $ACCOUNTING_KEY, permissions: ' + json.dumps(grants) + '},']
    for user, variable, route in [('feed', 'FEED_KEY', 'accounting.collateral.snapshot'),
                                  ('custody', 'CUSTODY_KEY', 'accounting.collateral.confirmation.custody')]:
        grants = {'publish': {'allow': [route]}, 'subscribe': {'allow': ['_INBOX.>']}}
        lines.append('{user: ' + user + ', password: $' + variable + ', permissions: ' + json.dumps(grants) + '},')
    observer = {'publish': {'allow': ['$JS.API.STREAM.INFO.ACCOUNTING', '$JS.API.STREAM.MSG.GET.ACCOUNTING',
                                      '$JS.API.CONSUMER.LIST.ACCOUNTING']}, 'subscribe': {'allow': ['_INBOX.>']}}
    lines.append('{user: observer, password: $OBSERVER_KEY, permissions: ' + json.dumps(observer) + '}')
    return '\n'.join(lines + [']}'])


class Estate:
    def __init__(self, scratch, programs):
        self.scratch = Path(scratch)
        self.programs = programs
        self.lease = uuid.uuid4().hex
        self.label = 'kanz.collateral.lease=' + self.lease
        self.prefix = 'collateral-' + self.lease[:12]
        self.secrets = {name: secrets.token_hex(28) for name in
                        ('admin', 'accounting', 'feed', 'custody', 'observer', 'postgres', 'owner', 'runtime', 'issuer', 'signing')}
        self.tokens = {}

    def name(self, suffix):
        return self.prefix + '-' + suffix

    def clean(self):
        for kind, listing in [('container', ['ps', '-aq', '--no-trunc']), ('network', ['network', 'ls', '-q', '--no-trunc']),
                              ('volume', ['volume', 'ls', '-q'])]:
            selection = [*listing, '--filter', 'label=' + self.label]
            for ident in docker('list owned ' + kind, *selection).splitlines():
                cmd = [kind, 'inspect', ident]
                info, = json.loads(docker('verify owned ' + kind, *cmd))
                labels = info['Config']['Labels'] if kind == 'container' else info['Labels']
                if labels.get('kanz.collateral.lease') != self.lease:
                    raise RuntimeError('cleanup lease mismatch')
                flags = ['-f', '-v'] if kind == 'container' else []
                docker('remove owned ' + kind, kind, 'rm', *flags, ident)
            if docker('observe cleanup ' + kind, *selection):
                raise RuntimeError('owned ' + kind + ' survived cleanup')

    def create(self, name, image, net, extra=(), command=(), env=None, connections=()):
        values = env or {}
        ident = docker('create ' + name, 'create', '--pull=never', '--label', self.label, '--name', self.name(name),
                       '--network', self.name(net), '--network-alias', name, '--memory=384m', '--pids-limit=160',
                       *extra, *(item for k in values for item in ('-e', k)), image, *command, env=values)
        for network, alias in connections:
            docker('connect ' + name, 'network', 'connect', '--alias', alias, self.name(network), ident)
        docker('start ' + name, 'start', ident)
        return ident

    def invoke(self, stage, net, image, command, *, env=None, extra=(), data=None):
        values = env or {}
        return docker(stage, 'run', '--rm', '-i', '--pull=never', '--label', self.label, '--network', self.name(net),
                      '--cap-drop=ALL', '--security-opt=no-new-privileges', '--read-only', '--tmpfs', '/tmp:size=16m',
                      '--pids-limit=64', '--memory=180m', *extra, *(item for k in values for item in ('-e', k)),
                      image, *command, env=values, data=data)

    def psql(self, role, query):
        user = 'postgres' if role == 'postgres' else 'workflow_' + role
        return docker('PostgreSQL ' + role + ' query', 'exec', '-i', '-e', 'PGPASSWORD', self.name('postgres'),
                      'psql', '-XqAt', '-v', 'ON_ERROR_STOP=1', '-h', '127.0.0.1', '-U', user, '-d', 'workflow',
                      env={'PGPASSWORD': self.secrets[role]}, data=query)

    def observed(self, sql):
        return self.psql('runtime', "SET app.tenant_id='rehearsal'; " + sql)

    def migrate(self):
        self.invoke('released migration', 'data', reference('kanz-migrate'), ['--dir', '/migrations/accounting'],
                    env={'KANZ_MIGRATE_DATABASE_URL': self.dsn('owner')})

    def dsn(self, role):
        return f"postgres://workflow_{role}:{self.secrets[role]}@postgres:5432/workflow?sslmode=disable"

    def broker(self, role):
        return f"nats://{role}:{self.secrets[role]}@nats:4222"

    def probe(self, operation, *arguments, role=None, event_time=None):
        net = 'client'
        env = {}
        if operation == 'mint':
            env = {'FIXTURE_ISSUER_KEY': self.secrets['issuer']}
        elif operation in ('snapshot', 'confirm'):
            net = 'data'
            env = {'NATS_URL': self.broker('feed' if operation == 'snapshot' else 'custody'), 'EVENT_TIME': event_time}
        elif operation == 'receipt':
            net, env = 'data', {'NATS_URL': self.broker('observer')}
        else:
            env = {'ACTOR_TOKEN': self.tokens[role], 'REQUEST_SIGNING_KEY': self.secrets['signing']}
        return self.invoke(operation, net, reference('accounting'), [operation, *arguments], env=env,
                           extra=['--mount', f'type=bind,src={self.programs}/probe,dst=/probe,readonly', '--entrypoint', '/probe'])

    def setup(self):
        for name in ('data', 'control', 'client'):
            ident = docker('create isolated network', 'network', 'create', '--internal', '--label', self.label, self.name(name))
            if docker('verify internal network', 'network', 'inspect', '-f', '{{.Internal}}', ident) != 'true':
                raise RuntimeError('network permits external routing')
        for name in ('pgdata', 'natsdata'):
            docker('create disposable volume', 'volume', 'create', '--label', self.label, self.name(name))
        self.create('postgres', public(DATABASE), 'data', env={'POSTGRES_PASSWORD': self.secrets['postgres'], 'POSTGRES_DB': 'workflow'},
                    extra=['--mount', 'type=volume,src=' + self.name('pgdata') + ',dst=/var/lib/postgresql/data'])
        wait_for('PostgreSQL readiness', lambda: self.psql('postgres', 'SELECT 1;') == '1')
        # Generated hex literals contain no SQL metacharacters; all SQL goes to stdin.
        self.psql('postgres', f"""
            CREATE ROLE workflow_owner LOGIN PASSWORD '{self.secrets['owner']}' NOSUPERUSER NOBYPASSRLS;
            CREATE ROLE workflow_runtime LOGIN PASSWORD '{self.secrets['runtime']}' NOSUPERUSER NOBYPASSRLS;
            ALTER DATABASE workflow OWNER TO workflow_owner;
            GRANT USAGE ON SCHEMA public TO workflow_runtime;
            ALTER DEFAULT PRIVILEGES FOR ROLE workflow_owner IN SCHEMA public
                GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO workflow_runtime;
            ALTER DEFAULT PRIVILEGES FOR ROLE workflow_owner IN SCHEMA public
                GRANT USAGE, SELECT ON SEQUENCES TO workflow_runtime;
        """)
        if self.psql('runtime', "SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user") != 'f':
            raise RuntimeError('financial runtime role is privileged')
        conf = self.scratch / 'nats.conf'
        conf.write_text(nats_configuration())
        conf.chmod(0o644)
        self.create('nats', public(BROKER), 'data', command=['-c','/fixture.conf'],
                    extra=['--mount', f'type=bind,src={conf},dst=/fixture.conf,readonly',
                           '--mount', 'type=volume,src=' + self.name('natsdata') + ',dst=/data'],
                    env={k.upper() + '_KEY':self.secrets[k] for k in ('admin','accounting','feed','custody','observer')})
        bootstrap = (ROOT / 'kanz/infra/nats/bootstrap-job.yaml').read_text().split('  bootstrap.sh: |\n', 1)[1].split('\n---', 1)[0]
        script = '\n'.join(row[4:] for row in bootstrap.splitlines()) + '\n'
        self.invoke('canonical development stream bootstrap', 'data', public(BOX), ['-se'], extra=['--entrypoint', '/bin/sh'],
                    env={'NATS_URL': 'nats://nats:4222', 'NATS_USER': 'admin',
                         'NATS_PASSWORD': self.secrets['admin'], 'NATS_REPLICAS': '1'}, data=script)
        self.migrate()
        self.accounting()
        self.create('gateway', reference('accounting'), 'client', connections=[('control','gateway-control')],
                    extra=['--cap-drop=ALL','--security-opt=no-new-privileges','--read-only',
                           '--mount',f'type=bind,src={self.programs}/gateway,dst=/gateway,readonly','--entrypoint','/gateway'],
                    env={'API_GATEWAY_LISTEN':'gateway:8080','API_GATEWAY_RISK_ENGINE_ADDR':'127.0.0.1:9090',
                         'API_GATEWAY_ACCOUNTING_ADDR':'http://accounting-api:8101','API_GATEWAY_REQUIRED_ROLE':'kanz-user',
                         'API_GATEWAY_FUND_ROLE':'kanz-fund','API_GATEWAY_TRADE_ROLE':'kanz-trader',
                         'API_GATEWAY_ALLOW_DEV_HS256':'true','API_GATEWAY_JWT_SECRET':self.secrets['issuer'],
                         'API_GATEWAY_SIGNING_SECRET':self.secrets['signing']})
        for role in ('maker','checker'):
            self.tokens[role] = self.probe('mint', role)

    def accounting(self):
        return self.create('accounting', reference('accounting'), 'data', connections=[('control', 'accounting-api')],
                           extra=['--cap-drop=ALL','--security-opt=no-new-privileges','--read-only'],
                           env={'ACCOUNTING_LISTEN':'accounting-api:8101','ACCOUNTING_METRICS_LISTEN':'127.0.0.1:8080',
                                'ACCOUNTING_DATABASE_URL':self.dsn('runtime'),'ACCOUNTING_NATS_URL':self.broker('accounting'),
                                'ACCOUNTING_TENANT':'rehearsal','ACCOUNTING_COLLATERAL_INPUT_SOURCE':'feed',
                                'ACCOUNTING_COLLATERAL_CUSTODY_SOURCES':json.dumps([{'Source':'custody','Custodian':'custodian','Account':'account'}])})

    def receipt(self):
        return self.observed("""SELECT json_build_array((SELECT count(*) FROM collateral_snapshots),
            (SELECT count(*) FROM collateral_requests),(SELECT count(*) FROM collateral_confirmations),
            (SELECT count(*) FROM collateral_reservations),(SELECT count(*) FROM collateral_workflows),
            (SELECT count(*) FROM outbox WHERE subject='accounting.collateral.recorded'))""")

    def recover(self):
        print('Publishing the authoritative snapshot and checking the Accounting acknowledgment.', flush=True)
        stamp = (datetime.now(timezone.utc) - timedelta(minutes=1)).isoformat()
        self.probe('snapshot', event_time=stamp)
        wait_for('first acknowledged snapshot', lambda: bool(self.probe('receipt','accounting.collateral.snapshot','1')))
        wait_for('committed authoritative snapshot', lambda: self.observed('SELECT count(*) FROM collateral_snapshots') == '1')
        proposal = self.probe('propose', role='maker')
        self.probe('self-approve', role='maker')
        original = self.probe('approve', role='checker')
        if proposal == original or not re.fullmatch(r'[0-9a-f]{64}', original):
            raise ValueError('proposal and instruction must have distinct evidence')
        wait_for('publication of both workflow revisions', lambda: self.published() == '2')
        before = json.loads(self.receipt())
        if before != [1,2,0,1,1,2]:
            raise ValueError('pending approval ledger differs: ' + str(before))
        journal = self.psql('owner', 'SELECT row_to_json(s) FROM schema_migrations s ORDER BY set_name,version')
        if len(journal.splitlines()) != 20:
            raise RuntimeError('released migration did not record its complete inventory')
        print('Restarting Accounting, PostgreSQL and JetStream with an outstanding instruction.', flush=True)
        app = self.name('accounting')
        old_id = docker('observe original process identity','inspect','-f','{{.Id}}',app)
        docker('terminate Accounting at the unconfirmed instruction','kill',app)
        # Remove only our leased container before replacing it against the same volumes.
        if docker('verify released worker ownership','inspect','-f','{{index .Config.Labels "kanz.collateral.lease"}}',app) != self.lease:
            raise RuntimeError('unexpected Accounting lease')
        docker('remove terminated Accounting process','rm',app)
        for service in ('postgres','nats'):
            ident = self.name(service)
            old = docker('observe server start','inspect','-f','{{.State.StartedAt}}',ident)
            docker('restart ' + service,'restart',ident)
            if docker('verify new server start','inspect','-f','{{.State.StartedAt}}',ident) == old:
                raise RuntimeError('server did not restart')
        wait_for('database recovery', lambda: self.psql('runtime','SELECT 1;') == '1')
        self.migrate()
        if journal != self.psql('owner','SELECT row_to_json(s) FROM schema_migrations s ORDER BY set_name,version'):
            raise RuntimeError('migration journal changed on replay')
        replacement = self.accounting()
        if replacement == old_id:
            raise RuntimeError('worker identity did not change')
        wait_for('original pending instruction after restart', lambda: self.probe('instructed',role='checker') == original)
        self.probe('snapshot',event_time=stamp)
        wait_for('replayed snapshot acknowledged', lambda: bool(self.probe('receipt','accounting.collateral.snapshot','2')))
        if self.probe('approve',role='checker') != original or json.loads(self.receipt()) != before:
            raise ValueError('ambiguous instruction retry changed durable evidence or created settlement')
        print('Checking a custody confirmation and its independent duplicate delivery.', flush=True)
        effective = datetime.now(timezone.utc).isoformat()
        self.probe('confirm',event_time=effective)
        wait_for('first confirmation acknowledged',
                 lambda: bool(self.probe('receipt','accounting.collateral.confirmation.custody','1')))
        wait_for('authoritative settlement', lambda: bool(self.probe('settled',role='checker')))
        settled = self.probe('settled',role='checker')
        self.probe('confirm',event_time=effective)
        wait_for('duplicate confirmation acknowledged',
                 lambda: bool(self.probe('receipt','accounting.collateral.confirmation.custody','2')))
        wait_for('publication of the settlement', lambda: self.published() == '3')
        if self.probe('settled',role='checker') != settled or json.loads(self.receipt()) != [1,2,1,1,1,3]:
            raise ValueError('duplicate custody confirmation changed the settled workflow')
        print(f'Released Accounting worker {old_id[:12]} replaced by {replacement[:12]}. Both servers restarted. '
              '20 migrations retained their recorded identity. One approved instruction remained pending across retries. '
              'One custodian confirmation settled the 60 USD leg.', flush=True)

    def published(self):
        return self.observed("SELECT count(published_at) FROM outbox WHERE subject='accounting.collateral.recorded'")


def main(programs):
    if os.environ.get('RUNNER_ENVIRONMENT') != 'github-hosted' or os.environ.get('GITHUB_REPOSITORY') != 'eighred/kanz':
        raise ValueError('this release rehearsal requires an owned disposable GitHub runner')
    for ref in (DATABASE, BROKER, BOX):
        docker('acquire pinned public backing image','pull','--platform=linux/amd64',public(ref),timeout=180)
    with tempfile.TemporaryDirectory() as scratch:
        estate = Estate(scratch,programs)
        try:
            estate.setup()
            estate.recover()
        finally:
            estate.clean()
        print('All owned containers, networks and persistent backing volumes were removed.', flush=True)


if __name__ == '__main__':
    main(Path(sys.argv[1]).resolve(strict=True))
