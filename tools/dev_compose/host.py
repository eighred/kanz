"""Run the fresh-checkout Make lifecycle in one leased VM with a host-observed network handoff."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import time
import uuid


ROOT = Path(__file__).resolve().parents[2]
HERE = Path(__file__).resolve().parent
UBUNTU = 'a80e9c9fd0d484dec024da3e9e264bbad4ce159b5312d6e149ef6ad532e18e43'
# These scripts execute during input acquisition while the generation user may
# reach only its loopback proxy. Require review whenever their bytes change.
ACQUISITION = {'kanz/Makefile': '7655a0999e399ed66d2acc1fc042e8b73bec32c3',
               'kanz/build/go-mod-download.sh': '8317486095205ed7f6ecad1eaf6235da542c2668',
               'schemas/bootstrap-go-sdk.sh': 'cce224c9eed3a986b01dca72abfa537486b527e6',
               'schemas/buf.gen.yaml': 'a1a098602d9820ec97d2d3937390dcde18e5da6e',
               'schemas/buf.yaml': 'd6b0f79c8f37f1ec09d24d1639cb38599fb8f529'}
DEV = ['env', '-i', 'HOME=/home/developer', 'USER=developer',
       'PATH=/usr/local/go/bin:/home/developer/go/bin:/usr/local/bin:/usr/bin:/bin',
       'GOPATH=/home/developer/go', 'GOTOOLCHAIN=local', 'GOMAXPROCS=2', 'COMPOSE_PARALLEL_LIMIT=1',
       'GOPROXY=https://proxy.golang.org', 'https_proxy=http://127.0.0.1:3128',
       'http_proxy=http://127.0.0.1:3128', 'no_proxy=localhost,127.0.0.1']


def run(*args, timeout=180, quiet=False):
    try:
        p = subprocess.run(args, capture_output=quiet, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        raise RuntimeError(f'{Path(args[0]).name} timed out') from None
    if p.returncode:
        raise RuntimeError(f'{Path(args[0]).name} exited {p.returncode}')
    return p.stdout.strip() if quiet else None


def unit(vm):
    return run('lxc', 'exec', vm, '--', 'systemctl', 'show', 'kanz-dev-up', '-p', 'ActiveState',
               '--value', quiet=True)


def inventory():
    return {row['name']: row for row in json.loads(run('lxc', 'list', '--format=json', quiet=True))}


def main(inputs, buf):
    if (os.environ.get('GITHUB_REPOSITORY') != 'eighred/kanz' or os.environ.get('RUNNER_ENVIRONMENT') != 'github-hosted'
            or not Path('/dev/kvm').is_char_device() or inventory()):
        raise ValueError('disposable hosted KVM and empty LXD inventory required')
    revision = run('git', 'rev-parse', 'HEAD', quiet=True)
    if revision != os.environ['GITHUB_SHA'] or any(run('git', 'rev-parse', 'HEAD:' + f, quiet=True) != oid
                                                   for f, oid in ACQUISITION.items()):
        raise ValueError('source revision or reviewed acquisition inputs changed')
    print('Qualifying source', revision, flush=True)
    source = inputs / 'source.tar'
    run('git', 'archive', '--output=' + str(source), 'HEAD')
    lease = uuid.uuid4().hex
    vm = 'kanz1323-' + lease[:12]
    project = 'kanzdev' + lease[:12]
    def guest(*args, timeout=180, quiet=False):
        return run('lxc', 'exec', vm, '--', *args, timeout=timeout, quiet=quiet)

    try:
        run('lxc', 'init', 'ubuntu:' + UBUNTU, vm, '--vm', '-c', 'user.kanz_lease=' + lease,
            '-c', 'limits.cpu=2', '-c', 'limits.memory=4GiB', '--device', 'root,size=24GiB', timeout=600)
        run('lxc', 'config', 'device', 'add', vm, 'acquisition', 'nic', 'network=lxdbr0', 'name=eth0')
        run('lxc', 'start', vm, timeout=240)
        deadline = time.monotonic() + 180
        while True:
            try:
                guest('true', timeout=10, quiet=True)
                break
            except RuntimeError:
                if time.monotonic() >= deadline:
                    raise RuntimeError('VM agent unavailable') from None
                time.sleep(2)
        guest('cloud-init', 'status', '--wait', timeout=240)
        for local, remote in [(HERE / 'prepare.sh', '/root/prepare.sh'), (inputs / 'images.tar', '/root/images.tar'),
                              (source, '/root/source.tar'), (buf, '/usr/local/bin/buf')]:
            run('lxc', 'file', 'push', str(local), vm + remote, timeout=480)
        guest('sh', '/root/prepare.sh', timeout=500)
        guest('docker', 'load', '-i', '/root/images.tar', timeout=450)
        pins = json.loads((HERE / 'images.json').read_bytes())
        for reference, index in pins.items():
            if guest('docker', 'image', 'inspect', '--format', '{{.Id}}', reference, quiet=True) != 'sha256:' + index:
                raise ValueError('guest image store changed an immutable input: ' + reference)
        go = 'ghcr.io/eighred/base/golang:1.27.4'
        cid = guest('docker', 'create', go, 'true', quiet=True)
        guest('docker', 'cp', cid + ':/usr/local/go', '/usr/local/go')
        guest('docker', 'rm', cid)
        guest('sh', '-ec', """
            chmod 755 /usr/local/bin/buf
            tar -xf /root/source.tar --no-same-owner -C /work/repo
            install -d -o developer -g developer /work/repo/schemas/gen /work/repo/kanz/.gotmp
            test -z "$(docker container ls -aq)"
            test -z "$(docker volume ls -q)"
            test "$(/usr/local/bin/buf --version)" = 1.69.0
            /usr/local/go/bin/go version
        """)
        guest('env', 'COMPOSE_PROJECT_NAME=' + project, 'python3', '-B',
              '/work/repo/tools/dev_compose/lifecycle.py', '--observe-baseline')
        env = [*DEV, 'COMPOSE_PROJECT_NAME=' + project]
        guest('systemd-run', '--unit=kanz-dev-up', '--remain-after-exit', '--uid=developer',
              '--property=RuntimeMaxSec=45min', *env, 'make', '-C', '/work/repo/kanz', '-f', 'Makefile',
              '-f', '/work/repo/tools/dev_compose/phase.mk', 'up')
        deadline = time.monotonic() + 600
        while guest('sh', '-c', 'test -f /home/developer/sdk-ready && echo yes || echo waiting', quiet=True) != 'yes':
            if time.monotonic() > deadline or unit(vm) == 'failed':
                guest('systemctl', 'show', 'kanz-dev-up', '-p', 'ExecMainStatus', '-p', 'Result')
                raise RuntimeError('canonical SDK generation did not reach the handoff')
            time.sleep(3)
        guest('runuser', '-u', 'developer', '--', *env, 'go', '-C', '/work/repo/kanz', 'mod', 'download', timeout=480)
        guest('runuser', '-u', 'developer', '--', *env, 'go', '-C', '/work/repo/kanz', 'mod', 'verify', timeout=240)
        guest('python3', '-B', '/work/repo/tools/dev_compose/dependencies.py', timeout=600)
        # The LXD host owns this NIC. Even a guest with Docker authority cannot reattach it.
        run('lxc', 'config', 'device', 'remove', vm, 'acquisition')
        devices = json.loads(run('lxc', 'query', '/1.0/instances/' + vm, quiet=True))['expanded_devices']
        if set(devices) != {'root'} or devices['root'].get('type') != 'disk' or devices['root'].get('path') != '/':
            raise ValueError('unexpected guest hardware after detachment')
        guest('sh', '-ec', """
            test -z "$(ip -4 route show default)"
            test -z "$(ip -6 route show default)"
            test ! -e /sys/class/net/eth0
            systemctl stop squid
            iptables -D OUTPUT -m owner --uid-owner developer -j KANZ_DEV_ACQUISITION
            ip6tables -D OUTPUT -m owner --uid-owner developer -j REJECT
            setfacl -m u:developer:rw /var/run/docker.sock
            touch /run/kanz-lifecycle-authorized
        """)
        deadline = time.monotonic() + 1250
        while True:
            status = guest('systemctl', 'show', 'kanz-dev-up', '-p', 'SubState', '--value', quiet=True)
            if status == 'exited':
                break
            if time.monotonic() > deadline or status in ('failed', 'dead'):
                guest('systemctl', 'show', 'kanz-dev-up', '-p', 'ExecMainStatus', '-p', 'Result')
                raise RuntimeError('normal make up failed on the disconnected VM')
            time.sleep(3)
        print('The normal Make startup and authenticated smoke completed.', flush=True)
        guest('env', 'COMPOSE_PROJECT_NAME=' + project, 'python3', '-B',
              '/work/repo/tools/dev_compose/lifecycle.py', timeout=720)
    finally:
        existing = inventory()
        if vm in existing:
            if existing[vm]['config'].get('user.kanz_lease') != lease:
                raise RuntimeError('refusing to remove an unowned VM')
            run('lxc', 'delete', '--force', vm)
        if vm in inventory():
            raise RuntimeError('owned VM survived teardown')
        print('Observed leased VM absent:', vm, flush=True)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('inputs', type=Path)
    parser.add_argument('buf', type=Path)
    args = parser.parse_args()
    main(args.inputs, args.buf)
