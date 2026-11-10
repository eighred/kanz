"""Owned loopback NATS resolver for the golden-path infrastructure caller."""
import base64
from contextlib import contextmanager
import json
import os
from pathlib import Path
import shutil
import subprocess
import time

MODULES = {
    'nsc': ('github.com/nats-io/nsc/v2', 'v2.15.0'),
    'nats-server': ('github.com/nats-io/nats-server/v2', 'v2.14.5'),
}


def run(argv, env, cwd, timeout=30):
    result = subprocess.run(argv, cwd=cwd, env=env, capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        raise ValueError(f'NATS fixture command failed: {Path(argv[0]).name} {argv[1]}')
    return result.stdout.strip()


class Resolver:
    def __init__(self, scratch, env, url, probe, versions):
        self.scratch = scratch
        self.env = env
        self.url = url
        self.probe = probe
        self.versions = versions

    def account(self, tenant, *, create_probe=False):
        raw = run(['nsc', 'describe', 'account', '-n', tenant, '--raw'], self.env, self.scratch)
        payload = raw.split('.')[1]
        public_key = json.loads(base64.urlsafe_b64decode(payload + '=' * (-len(payload) % 4)))['sub']
        credentials = self.scratch / (tenant + '.creds')
        if create_probe:
            run(['nsc', 'add', 'user', '-a', tenant, '-n', 'fixture-probe'], self.env, self.scratch)
            run(['nsc', 'generate', 'creds', '-a', tenant, '-n', 'fixture-probe',
                 '-o', str(credentials)], self.env, self.scratch)
        # Only the tracked caller can publish the account claim. Creating a probe
        # user here exercises that claim through a new authenticated connection.
        self.observe(tenant, 'create' if create_probe else 'read')
        return public_key

    def observe(self, tenant, mode):
        result = run([str(self.probe), '-test.v', '-test.run=^TestOnboardingProbe$'],
                     dict(self.env, ONBOARDING_PROBE_URL=self.url, ONBOARDING_PROBE_TENANT=tenant,
                          ONBOARDING_PROBE_CREDS=str(self.scratch / (tenant + '.creds')),
                          ONBOARDING_PROBE_MODE=mode), self.scratch)
        if not any(line.startswith('--- PASS: TestOnboardingProbe (') for line in result.splitlines()):
            raise ValueError('NATS onboarding probe did not run')

    def seed_portfolios(self, root):
        tenants = ('acme', 'bravo')
        go_env = {key: os.environ[key] for key in ('GOTOOLCHAIN', 'GOTMPDIR', 'GOFLAGS') if key in os.environ}
        go_env.update(json.loads(run(['go', 'env', '-json', 'GOCACHE', 'GOMODCACHE'],
                                     os.environ, root / 'kanz')))
        for tenant in tenants:
            self.observe(tenant, 'seed-prepare')
            seed_creds = self.scratch / (tenant + '-seed.creds')
            run(['nsc', 'add', 'user', '-a', tenant, '-n', 'fixture-seed',
                 '--allow-pub', 'risk.portfolio.snapshot', '--allow-sub', '_INBOX.>'],
                self.env, self.scratch)
            run(['nsc', 'generate', 'creds', '-a', tenant, '-n', 'fixture-seed',
                 '-o', str(seed_creds)], self.env, self.scratch)
            run(['sh', './provision-tenant.sh'], dict(self.env, **go_env, TENANT=tenant, STEP='seed',
                SEED_NATS_URL=self.url, SEED_NATS_CREDS_FILE=str(seed_creds)),
                root / 'kanz/infra/onboarding', timeout=180)
        for tenant in tenants:
            self.observe(tenant, 'seed-read')
        return list(tenants)


@contextmanager
def resolver(scratch, root):
    scratch.mkdir(mode=0o700)
    env = {key: os.environ[key] for key in ('PATH', 'SYSTEMROOT', 'USERPROFILE', 'TEMP', 'TMP') if key in os.environ}
    env.update(HOME=str(scratch), NSC_HOME=str(scratch / 'nsc'),
               NKEYS_PATH=str(scratch / 'keys'), NATS_OPERATOR='fixture')
    versions = {}
    for tool, (module, expected) in MODULES.items():
        binary = shutil.which(tool)
        if binary is None:
            raise ValueError(f'NATS fixture requires {tool}')
        metadata = run(['go', 'version', '-m', binary], env, scratch)
        if f'\tmod\t{module}\t{expected}\t' not in metadata:
            raise ValueError(f'NATS fixture module version differs for {tool}')
        versions[tool] = expected
    probe = scratch / ('probe.exe' if os.name == 'nt' else 'probe')
    run(['go', 'test', '-c', '-o', str(probe), './test/load/onboardingprobe'],
        os.environ, root / 'kanz', timeout=180)
    run(['nsc', 'add', 'operator', '-n', env['NATS_OPERATOR'], '--sys'], env, scratch)
    config = run(['nsc', 'generate', 'config', '--nats-resolver'], env, scratch)
    path = scratch / 'nats.conf'
    path.write_text(config + '\njetstream { store_dir: "./jetstream" }\n', encoding='utf-8')
    argv = ['nats-server', '-c', str(path), '-a', '127.0.0.1', '-p', '-1', '--ports_file_dir', str(scratch)]
    with subprocess.Popen(argv, cwd=scratch, env=env, stdout=subprocess.DEVNULL,
                          stderr=subprocess.DEVNULL) as server:
        try:
            deadline = time.monotonic() + 15
            ports = scratch / f'nats-server_{server.pid}.ports'
            while not ports.exists():
                if server.poll() is not None or time.monotonic() >= deadline:
                    raise ValueError('owned NATS resolver did not start')
                time.sleep(0.1)
            [url] = json.loads(ports.read_text(encoding='utf-8'))['nats']
            if not url.startswith('nats://127.0.0.1:'):
                raise ValueError('NATS resolver is outside fixture loopback')
            run(['nsc', 'edit', 'operator', '--account-jwt-server-url', url], env, scratch)
            yield Resolver(scratch, env, url, probe, versions)
            if server.poll() is not None:
                raise ValueError('NATS resolver stopped during lifecycle qualification')
        finally:
            if server.poll() is None:
                server.terminate()
                try:
                    server.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    server.kill()
                    server.wait(timeout=5)
