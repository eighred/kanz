"""Verify an immutable application candidate with the standard Go oracle and runtime limits."""
import argparse
import contextlib
import json
from pathlib import Path
import re
import sys
import tempfile
import uuid

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from linked_advisory import SYMBOLS
from proof import CASES, CommandTimeout, command, container_boundary, oracle, required


@contextlib.contextmanager
def worker(image, source, check_runtime=False, supervisor=None):
    name = 'kanz-julia-app-' + uuid.uuid4().hex
    label = 'kanz.julia-proof=' + source
    try:
        args = ['docker', 'create', '--pull=never', '--name', name, '--label', label,
                  '--platform', 'linux/amd64', '--network', 'none', '--read-only', '--user', '65532:65532',
                  '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges', '--cpus', '1',
                  '--memory', '1g', '--memory-swap', '1g', '--pids-limit', '64',
                  '--tmpfs', '/tmp:rw,noexec,nosuid,size=16777216,mode=1777', '-i']
        entrypoint = '/opt/kanz/bin/curve-worker'
        if supervisor:
            binary, digest = supervisor
            entrypoint = '/bounded-parent'
            args += ['--mount', 'type=bind,src=' + str(binary) + ',dst=' + entrypoint + ',readonly',
                     '--entrypoint', entrypoint, image, '-test.v', '-test.run=^TestCompiledApplicationSupervisor$',
                     '-application-launcher-sha256=' + digest]
        elif check_runtime:
            entrypoint = '/opt/kanz/bin/julia'
            controls = Path(__file__).with_name('runtime_controls.jl').read_text()
            args += ['--entrypoint', entrypoint, image, '--startup-file=no', '--history-file=no', '-e', controls]
        else:
            args += [image]
        required(args)
        info, = json.loads(required(['docker', 'inspect', name]))
        container_boundary(info, image, source)
        external = [m for m in info['Mounts'] if (m['Type'], m['Destination']) != ('tmpfs', '/tmp')]
        if supervisor:
            if len(external) != 1 or external[0]['Type'] != 'bind' or external[0]['Destination'] != entrypoint or external[0]['RW']:
                raise ValueError('supervisor must be the only external, read-only file')
        elif external:
            raise ValueError('unexpected external application mount')
        if info['Config']['Entrypoint'] != [entrypoint] or info['Config']['Labels'].get('org.opencontainers.image.revision') != source:
            raise ValueError('unexpected application entrypoint or external mount')
        yield name
    finally:
        selector = ['docker', 'ps', '-aq', '--filter', 'name=^/' + name + '$']
        cid = required(selector).decode().strip()
        if cid:
            info, = json.loads(required(['docker', 'inspect', cid]))
            if info['Config']['Labels'].get('kanz.julia-proof') != source or info['Name'] != '/' + name:
                raise ValueError('refusing to clean up an unowned container')
            required(['docker', 'rm', '--force', cid])
        if required(selector).strip():
            raise RuntimeError('owned application candidate survived cleanup')


def bounded_controls(image, source):
    with worker(image, source, check_runtime=True) as name:
        output = required(['docker', 'start', '--attach', name]).decode('ascii').splitlines()
        if not output or output[-1] != 'ENOSPC at 16777216 bytes':
            raise ValueError('tmpfs upper bound differed')
        symbols = {}
        for line in output[:-1]:
            prefix, path, symbol, version = line.split(' ')
            items = symbols.setdefault(path, {})
            if prefix != 'KANZ_NATIVE' or symbol in items:
                raise ValueError('ambiguous native runtime symbol')
            items[symbol] = version
        if symbols != SYMBOLS:
            raise ValueError('linked runtime owner or version differs')
    # Keep stdin open on the real entrypoint. The owning parent must be able to
    # time out and reap a worker that is still waiting for a complete request.
    with worker(image, source) as name:
        required(['docker', 'start', name])
        try:
            required(['docker', 'wait', name], timeout=2)
        except CommandTimeout:
            info, = json.loads(required(['docker', 'inspect', name]))
            container_boundary(info, image, source)
            if not info['State']['Running'] or info['State']['OOMKilled']:
                raise ValueError('timed-out application was not running')
        else:
            raise ValueError('open application request did not time out')
    return symbols


def main(image, source, receipt):
    if not re.fullmatch('sha256:[0-9a-f]{64}', image) or not re.fullmatch('[0-9a-f]{40}', source):
        raise ValueError('expected immutable image and Git identities')
    info, = json.loads(required(['docker', 'image', 'inspect', image]))
    if (info['Config'].get('Labels') or {}).get('org.opencontainers.image.revision') != source:
        raise ValueError('application source revision differs')
    symbols = bounded_controls(image, source)
    with tempfile.TemporaryDirectory() as directory:
        scratch = Path(directory)
        oracle(scratch, 'prepare')
        rows = json.loads((scratch / 'cases.json').read_bytes())
        if len(rows) != len(CASES) or {case['name'] for case in rows} != CASES:
            raise ValueError('independent wire corpus differs')
        for case in rows:
            key = case['name']
            with worker(image, source) as name:
                code, response = command(['docker', 'start', '--attach', '--interactive', name],
                                         data=(scratch / (key + '.input')).read_bytes())
            if (code == 0) != case['exitZero'] or (code and response):
                raise ValueError('application wire exit contract differs: ' + key)
            (scratch / (key + '.output')).write_bytes(response)
        oracle(scratch, 'verify')
    receipt.write_text(json.dumps({'image': image, 'source': source, 'symbols': symbols}, sort_keys=True) + '\n')
    print('Passed 13 real application wire cases, Go oracle, runtime isolation, tmpfs quota, timeout and cleanup.')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('image')
    parser.add_argument('source')
    parser.add_argument('receipt', type=Path)
    args = parser.parse_args()
    main(args.image, args.source, args.receipt)
