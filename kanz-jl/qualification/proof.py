"""Local OCI qualification only: no registry publication or production transport."""
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import threading
import tomllib
import uuid

import native_inventory
import native_mapping
import openssl_runtime
import curl_runtime

ROOT = Path(__file__).resolve().parents[2]
FLAGS = ['--startup-file=no', '--history-file=no', '--compiled-modules=existing',
         '--pkgimages=existing', '--project=/opt/kanz-jl']
CASES = {'deposit', 'forward', 'swap', 'negative', 'stub', 'missing', 'zero',
         'unsupported', 'invalid', 'numerical', 'empty', 'malformed', 'oversize'}
ARCHIVE_SHA256 = '5c61cb4b40332cb5a3504e7779e2f5dac093caa4a1116ded19f01413f85c63e4'


class CommandTimeout(RuntimeError):
    pass


ISOLATION_STAGES = {'imports', 'runtime', 'cgroup', 'process-security',
                    'write-refusal', 'installed-packages', 'native-runtime', 'native-version', 'openssl-version', 'libcurl-version', 'inventory'}


def isolation_stage(raw):
    stages = [line.removeprefix(b'KANZ_OCI_STAGE=').decode('ascii', errors='replace')
              for line in raw.splitlines() if line.startswith(b'KANZ_OCI_STAGE=')]
    return stages[-1] if stages and stages[-1] in ISOLATION_STAGES else 'unclassified'


def command(args, data=b'', timeout=120, limit=1 << 20, env=None, cwd=ROOT, diagnostic=None):
    process = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE, cwd=cwd, env=env)
    outputs = [bytearray(), bytearray()]
    overflow = threading.Event()

    def read(stream, index, maximum):
        try:
            while chunk := stream.read(8192):
                if len(outputs[index]) + len(chunk) > maximum:
                    overflow.set()
                    try:
                        process.kill()
                    except OSError:
                        pass
                    return
                outputs[index].extend(chunk)
        finally:
            stream.close()

    def write():
        try:
            process.stdin.write(data)
        except (BrokenPipeError, OSError):
            pass
        finally:
            try:
                process.stdin.close()
            except OSError:
                pass

    threads = [threading.Thread(target=read, args=(process.stdout, 0, limit), daemon=True),
               threading.Thread(target=read, args=(process.stderr, 1, 32768), daemon=True),
               threading.Thread(target=write, daemon=True)]
    for thread in threads:
        thread.start()
    try:
        code = process.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=5)
        raise CommandTimeout('bounded command timed out') from None
    finally:
        for thread in threads:
            thread.join(timeout=2)
    if overflow.is_set() or any(thread.is_alive() for thread in threads):
        raise RuntimeError('bounded command output exceeded')
    if diagnostic is not None:
        diagnostic.update(exitCode=code, isolationStage=isolation_stage(bytes(outputs[1])))
    return code, bytes(outputs[0])


def required(args, **kwargs):
    code, output = command(args, **kwargs)
    if code:
        raise RuntimeError('qualification command refused: ' + args[0])
    return output


def container_boundary(item, image, source):
    host, config = item['HostConfig'], item['Config']
    if (item['Image'] != image or config['User'] != '65532:65532'
            or config['Labels'].get('kanz.julia-proof') != source
            or host['NetworkMode'] != 'none' or not host['ReadonlyRootfs']
            or host['Privileged'] or host.get('Binds') or host.get('Devices')
            or host['Memory'] != 1 << 30 or host['MemorySwap'] != 1 << 30
            or host['NanoCpus'] != 1000000000 or host['PidsLimit'] != 64
            or host['CapDrop'] != ['ALL'] or host.get('CapAdd')
            or 'no-new-privileges' not in host['SecurityOpt']
            or host['Tmpfs'] != {'/tmp': 'rw,noexec,nosuid,size=16777216,mode=1777'}):
        raise ValueError('OCI isolation boundary differs')


EXCLUDED_DEVELOPMENT_ENVIRONMENTS = ['base/JuliaSyntax/docs', 'stdlib/v1.13/TOML/docs',
    'stdlib/v1.13/TOML/benchmark', 'stdlib/v1.13/Tar/test', 'test']


def validate_inventory(inventory, source, runtime_sha256, native_expected):
    native_inventory.validate(inventory.get('nativeRuntime'), native_expected)
    native_mapping.validate_component(inventory, native_expected)
    openssl_runtime.validate_component(inventory)
    curl_runtime.validate_component(inventory)
    if inventory.get('excludedDevelopmentEnvironments') != EXCLUDED_DEVELOPMENT_ENVIRONMENTS:
        raise ValueError('packaged development environment boundary differs')
    if inventory['sourceSHA'] != source or inventory['runtime'] != '1.13.3':
        raise ValueError('image source/runtime differs')
    for name, key in [('Project.toml', 'projectSHA256'), ('Manifest.toml', 'manifestSHA256')]:
        if inventory[key] != hashlib.sha256((ROOT / 'kanz-jl' / name).read_bytes()).hexdigest():
            raise ValueError('image locked environment differs')
    manifest = tomllib.loads((ROOT / 'kanz-jl/Manifest.toml').read_text())
    expected = {name: {'version': entries[0]['version'], 'treeSHA1': entries[0]['git-tree-sha1']}
                for name, entries in manifest['deps'].items() if 'git-tree-sha1' in entries[0]}
    if inventory['packages'] != expected or not inventory['generated']:
        raise ValueError('installed Julia dependency inventory differs')
    # Compare actual image-generated bytes with the separately generated canonical
    # Julia contracts from the existing host CI lane, not an image self-assertion.
    generated = {str(p.relative_to(ROOT / 'kanz-jl/gen')).replace('\\', '/'): hashlib.sha256(p.read_bytes()).hexdigest()
                 for p in (ROOT / 'kanz-jl/gen').rglob('*') if p.is_file()}
    if inventory['generated'] != generated:
        raise ValueError('image canonical contract generation differs')
    sources = {}
    for directory in ('src', 'bin', 'test'):
        for path in (ROOT / 'kanz-jl' / directory).rglob('*'):
            if path.is_file():
                sources[path.relative_to(ROOT / 'kanz-jl').as_posix()] = hashlib.sha256(path.read_bytes()).hexdigest()
    sources['qualification/isolation.jl'] = hashlib.sha256(Path(__file__).with_name('isolation.jl').read_bytes()).hexdigest()
    if (inventory['sources'] != sources or not re.fullmatch('[0-9a-f]{64}', runtime_sha256)
            or inventory['runtimeExecutableSHA256'] != runtime_sha256):
        raise ValueError('image worker/source inventory differs')


def oracle(directory, phase):
    output = required(['go', 'test', '-p', '1', './tools/julia-conformance', '-run', '^TestJuliaOCIOracle$',
                       '-count=1', '-json', '-args', '-julia-oci-dir', str(directory), '-julia-oci-phase', phase], cwd=ROOT / 'kanz')
    events = [json.loads(line) for line in output.splitlines()]
    if any(e.get('Action') in ('skip', 'fail') for e in events):
        raise ValueError('OCI oracle skipped or failed')
    passed = {e.get('Test') for e in events if e.get('Action') == 'pass'}
    expected = {'TestJuliaOCIOracle/' + name for name in CASES - {'empty', 'malformed', 'oversize'}}
    if 'TestJuliaOCIOracle' not in passed or None not in passed or not expected.issubset(passed):
        raise ValueError('OCI oracle incomplete')


def validate_report(report, source, runtime_sha256, native_expected):
    if (not re.fullmatch('[0-9a-f]{40}', source) or report.get('sourceSHA') != source
            or report.get('localOnly') is not True or report.get('passed') is not True
            or report.get('cleanupPassed') is not True or report.get('isolationPassed') is not True
            or report.get('goldensPassed') is not True
            or report.get('timeoutCleanupPassed') is not True
            or report.get('juliaAdvisoryCoverage') != 'unqualified-release-blocker'
            or report.get('archiveSHA256') != ARCHIVE_SHA256
            or not re.fullmatch('sha256:[0-9a-f]{64}', report.get('imageID', ''))
            or set(report.get('cases', [])) != CASES or len(report['cases']) != len(CASES)):
        raise ValueError('incomplete Julia OCI qualification')
    if not isinstance(report.get('inventory'), dict):
        raise ValueError('missing image inventory')
    try:
        validate_inventory(report['inventory'], source, runtime_sha256, native_expected)
    except (KeyError, TypeError, AttributeError):
        raise ValueError('malformed image inventory') from None


def main():
    output = Path(os.environ['JULIA_OCI_EVIDENCE'])
    output.mkdir(parents=True, exist_ok=False)
    source = required(['git', 'rev-parse', 'HEAD']).decode().strip()
    if not re.fullmatch('[0-9a-f]{40}', source):
        raise ValueError('invalid source revision')
    required(['git', 'diff', '--quiet', 'HEAD', '--'])
    # The existing CI lane installed this executable from the same verified
    # archive before packaging. Never trust a hash asserted only by the image.
    runtime = shutil.which('julia')
    if not runtime:
        raise ValueError('verified host Julia executable missing')
    runtime_sha256 = hashlib.sha256(Path(runtime).read_bytes()).hexdigest()
    native_expected = openssl_runtime.overlay(
        native_inventory.expected_archive(Path(os.environ['RUNNER_TEMP']) / 'julia.tar.gz'),
        Path(os.environ['RUNNER_TEMP']) / 'openssl.tar.gz')
    native_expected = curl_runtime.overlay(native_expected, Path(os.environ['RUNNER_TEMP']) / 'libcurl.tar.gz')
    report = {'sourceSHA': source, 'localOnly': True, 'passed': False, 'cleanupPassed': False,
              'juliaAdvisoryCoverage': 'unqualified-release-blocker', 'archiveSHA256': ARCHIVE_SHA256, 'cases': []}
    names = []
    image = None
    stage = 'build'
    try:
        with tempfile.TemporaryDirectory(prefix='kanz-julia-oci-') as directory:
            scratch = Path(directory)
            required(['docker', 'build', '--platform', 'linux/amd64', '--build-arg', 'VERSION=' + source,
                      '--iidfile', str(scratch / 'image-id'), '-f', 'kanz-jl/Dockerfile', '.'], timeout=900, limit=4 << 20)
            image = (scratch / 'image-id').read_text().strip()
            if not re.fullmatch('sha256:[0-9a-f]{64}', image):
                raise ValueError('invalid image identity')
            report['imageID'] = image
            metadata = json.loads(required(['docker', 'image', 'inspect', image]))[0]
            if metadata['Config']['Labels'].get('org.opencontainers.image.revision') != source:
                raise ValueError('image revision stamp differs')

            def run(data=b'', script=None, timeout_control=False):
                name = 'kanz-julia-proof-' + uuid.uuid4().hex
                names.append(name) # cleanup also covers a lost create response
                args = ['docker', 'create', '--name', name, '--label', 'kanz.julia-proof=' + source,
                        '--platform', 'linux/amd64', '--network', 'none', '--read-only', '--user', '65532:65532',
                        '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges', '--cpus', '1',
                        '--memory', '1g', '--memory-swap', '1g', '--pids-limit', '64',
                        '--tmpfs', '/tmp:rw,noexec,nosuid,size=16777216,mode=1777', '-i']
                if script or timeout_control:
                    args += ['--entrypoint', '/opt/julia/bin/julia']
                args += [image]
                if script:
                    args += FLAGS + [script]
                elif timeout_control:
                    args += FLAGS + ['-e', 'sleep(120)']
                required(args)
                info = json.loads(required(['docker', 'inspect', name]))[0]
                container_boundary(info, image, source)
                code, wire = command(['docker', 'start', '--attach', '--interactive', name], data=data,
                                     timeout=2 if timeout_control else 90,
                                     diagnostic=report.setdefault('isolationDiagnostic', {})
                                     if script == '/opt/kanz-jl/qualification/isolation.jl' else None)
                info = json.loads(required(['docker', 'inspect', name]))[0]
                container_boundary(info, image, source)
                if info['State']['Running'] or info['State']['OOMKilled'] or info['State']['ExitCode'] != code:
                    raise ValueError('container execution did not complete consistently')
                return code, wire

            stage = 'isolation-and-installed-dependencies'
            code, wire = run(script='/opt/kanz-jl/qualification/isolation.jl')
            if code:
                raise ValueError('actual isolation test refused')
            inventory = tomllib.loads(wire.decode())
            validate_inventory(inventory, source, runtime_sha256, native_expected)
            report['inventory'] = inventory
            report['isolationPassed'] = True
            stage = 'existing-independent-goldens'
            code, wire = run(script='/opt/kanz-jl/test/runtests.jl')
            if code or b'Generated wire boundary' not in wire or b'Independent rational discount factors' not in wire:
                raise ValueError('existing Julia goldens did not complete')
            report['goldensPassed'] = True
            stage = 'actual-timeout-cleanup'
            try:
                run(timeout_control=True)
            except CommandTimeout:
                name = names[-1]
                info = json.loads(required(['docker', 'inspect', name]))[0]
                container_boundary(info, image, source)
                if not info['State']['Running']:
                    raise ValueError('timeout control never reached running container')
                required(['docker', 'rm', '--force', name], timeout=30)
                if required(['docker', 'ps', '-a', '--filter', 'name=^/' + name + '$', '--format', '{{.Names}}']).strip():
                    raise ValueError('timed-out owned container survived cleanup')
                report['timeoutCleanupPassed'] = True
            else:
                raise ValueError('timeout control did not time out')
            stage = 'canonical-protobuf-entrypoint'
            oracle(scratch, 'prepare')
            cases = json.loads((scratch / 'cases.json').read_text())
            if len(cases) != len(CASES) or {c['name'] for c in cases} != CASES:
                raise ValueError('wire corpus differs')
            for case in cases:
                name = case['name']
                code, wire = run(data=(scratch / (name + '.input')).read_bytes())
                if (code == 0) is not case['exitZero'] or (code != 0 and wire):
                    raise ValueError('wire refusal/success contract differs')
                (scratch / (name + '.output')).write_bytes(wire)
                report['cases'].append(name)
            oracle(scratch, 'verify')
            report['passed'] = True
    except Exception as error:
        report['failureStage'] = stage
        report['errorType'] = type(error).__name__
        raise
    finally:
        clean = True
        for name in names:
            try:
                code, raw = command(['docker', 'inspect', name], timeout=15)
                if code == 0:
                    items = json.loads(raw)
                    if len(items) != 1 or items[0]['Config']['Labels'].get('kanz.julia-proof') != source:
                        clean = False
                        continue
                    code, _ = command(['docker', 'rm', '--force', name], timeout=30)
                    clean &= code == 0
                remaining = required(['docker', 'ps', '-a', '--filter', 'name=^/' + name + '$', '--format', '{{.Names}}'], timeout=15)
                clean &= not remaining.strip()
            except Exception:
                clean = False
        report['cleanupPassed'] = clean
        report['passed'] &= clean
        (output / 'julia-oci.json').write_text(json.dumps(report, indent=2) + '\n')
        if not clean:
            raise RuntimeError('owned OCI cleanup incomplete')
    validate_report(report, source, runtime_sha256, native_expected)
    print('Julia OCI qualification PASS: 13 wire cases, existing goldens, isolation, installed dependency identity, cleanup')


if __name__ == '__main__':
    main()
