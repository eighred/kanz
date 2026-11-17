"""Prepare a reproducible native input. This never installs a Julia consumer."""
import argparse
import datetime as dt
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import signal
import stat
import struct
import subprocess
import sys
import tarfile
import tempfile
import urllib.request
import uuid

HERE = Path(__file__).resolve().parent
PINS = json.loads((HERE / 'pins.json').read_text())
FILES = {'native.tar.gz': 16 << 20, 'libcrypto.so.3': 16 << 20,
         'libssl.so.3': 4 << 20, 'LICENSE.txt': 32 << 10,
         'build.log': 32 << 20, 'full_tests.log': 32 << 20,
         'regression_tests.log': 32 << 20, 'version.txt': 64 << 10,
         'toolchain.txt': 1 << 20, 'configure.txt': 1 << 20}
SAFE_ENV = {'PATH': '/usr/bin:/bin', 'LANG': 'C', 'LC_ALL': 'C'}
SOURCE_MEMBER_MAX = 32 << 20  # The pinned source includes two 20 MiB SLH-DSA vector files.


def digest(data):
    return hashlib.sha256(data).hexdigest()


def checked(args, timeout=60):
    # Target execution is redirected into disk-bounded scratch logs. Only
    # bounded trusted Docker/mount/git diagnostics reach this pipe.
    result = subprocess.run(args, env=SAFE_ENV, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, timeout=timeout, check=False)
    if len(result.stdout) + len(result.stderr) > 2 << 20:
        raise ValueError('oversized orchestration response')
    if result.returncode:
        raise RuntimeError('producer command refused: ' + args[0])
    return result.stdout


def fetch(url, maximum):
    request = urllib.request.Request(url, headers={'User-Agent': 'kanz-source-input'})
    with urllib.request.urlopen(request, timeout=60) as response:
        data = response.read(maximum + 1)
    if not 0 < len(data) <= maximum:
        raise ValueError('source response size differs')
    return data


def source_archive(data):
    if len(data) != PINS['size'] or digest(data) != PINS['sha256']:
        raise ValueError('source archive identity differs')
    prefix = 'openssl-' + PINS['source']
    found, total = set(), 0
    with tarfile.open(fileobj=io.BytesIO(data), mode='r:gz') as archive:
        for member in archive:
            name = PurePosixPath(member.name)
            if (member.name in found or name.is_absolute() or '..' in name.parts
                    or not name.parts or name.parts[0] != prefix
                    or not (member.isdir() or member.isfile())
                    or member.size > SOURCE_MEMBER_MAX or len(found) >= 10000):
                raise ValueError('unsafe source archive member')
            found.add(member.name)
            total += member.size
            if total > 192 << 20:
                raise ValueError('oversized unpacked source')
            if member.name == prefix + '/VERSION.dat':
                if archive.extractfile(member).read() != (HERE / 'VERSION.dat').read_bytes():
                    raise ValueError('source development version changed')
    required = {prefix + '/' + path for path in ('VERSION.dat', 'Configure', 'LICENSE.txt')}
    if not required <= found:
        raise ValueError('incomplete source archive')


def upstream_commit():
    url = 'https://api.github.com/repos/openssl/openssl/git/commits/' + PINS['source']
    raw = fetch(url, 256 << 10)
    commit = json.loads(raw)
    expected_date = dt.datetime.fromtimestamp(PINS['epoch'], dt.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
    if (commit.get('sha') != PINS['source'] or commit.get('tree', {}).get('sha') != PINS['tree']
            or commit.get('committer', {}).get('date') != expected_date):
        raise ValueError('upstream commit, tree or timestamp differs')
    return {'url': url, 'sha256': digest(raw)}


def provenance():
    observations = {}
    for ident, fix in PINS['fixes'].items():
        # The second page omits the unrelated file diffs while retaining the merge base.
        url = ('https://api.github.com/repos/openssl/openssl/compare/' + fix + '...' + PINS['source']
               + '?per_page=1&page=2')
        raw = fetch(url, 8 << 20)
        value = json.loads(raw)
        if (value.get('status') not in ('ahead', 'identical')
                or value.get('base_commit', {}).get('sha') != fix
                or value.get('merge_base_commit', {}).get('sha') != fix):
            raise ValueError('vendor fix is not an ancestor: ' + ident)
        cna_url = 'https://openssl-library.org/news/secjson/' + ident.lower() + '.json'
        cna_raw = fetch(cna_url, 64 << 10)
        cna = json.loads(cna_raw)
        container = cna.get('containers', {}).get('cna', {})
        # This is the CNA's label for the 3.5 maintenance patch. The source
        # under test still identifies itself as 3.5.10-dev.
        refs = [item.get('url') for item in container.get('references', [])
                if item.get('name') == '3.5.10 git commit' and 'patch' in item.get('tags', [])]
        if (cna.get('cveMetadata', {}).get('cveId') != ident
                or cna.get('cveMetadata', {}).get('state') != 'PUBLISHED'
                or container.get('providerMetadata', {}).get('shortName') != 'openssl'
                or refs != ['https://github.com/openssl/openssl/commit/' + fix]):
            raise ValueError('vendor CNA fix reference differs: ' + ident)
        observations[ident] = {'fix': fix, 'compareURL': url, 'compareSHA256': digest(raw),
                               'cnaURL': cna_url, 'cnaSHA256': digest(cna_raw)}
    return observations


def read_regular(directory_fd, name, maximum, allow_empty=False):
    fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory_fd)
    try:
        before = os.fstat(fd)
        if (not stat.S_ISREG(before.st_mode) or before.st_nlink != 1
                or not (0 if allow_empty else 1) <= before.st_size <= maximum):
            raise ValueError('unsafe producer output: ' + name)
        data = bytearray()
        while block := os.read(fd, min(65536, maximum + 1 - len(data))):
            data.extend(block)
            if len(data) > maximum:
                raise ValueError('oversized producer output')
        after = os.fstat(fd)
        if ((before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns, before.st_nlink)
                != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns, after.st_nlink)
                or len(data) != before.st_size):
            raise ValueError('producer output changed during read')
        return bytes(data)
    finally:
        os.close(fd)


def directory(path):
    """Walk from a retained root descriptor, refusing every linked component."""
    path = Path(path)
    if not path.is_absolute() or '..' in path.parts:
        raise ValueError('absolute evidence path required')
    fd = os.open('/', os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        for component in path.parts[1:]:
            child = os.open(component, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = child
        return fd
    except BaseException:
        os.close(fd)
        raise


def read_path(path, maximum):
    path = Path(path).absolute()
    fd = directory(path.parent)
    try:
        return read_regular(fd, path.name, maximum)
    finally:
        os.close(fd)


def new_directory(path):
    path = Path(path).absolute()
    parent = directory(path.parent)
    try:
        os.mkdir(path.name, 0o755, dir_fd=parent)
        fd = os.open(path.name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent)
        actual = os.fstat(fd)
        named = os.stat(path.name, dir_fd=parent, follow_symlinks=False)
        if (actual.st_dev, actual.st_ino) != (named.st_dev, named.st_ino):
            os.close(fd)
            raise ValueError('new evidence directory identity changed')
        return fd
    finally:
        os.close(parent)


def write_new(directory_fd, name, data):
    if not re.fullmatch(r'[A-Za-z0-9_.-]+', name):
        raise ValueError('invalid evidence name')
    fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                 0o644, dir_fd=directory_fd)
    try:
        before = os.fstat(fd)
        if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1 or before.st_size:
            raise ValueError('new evidence leaf is unsafe')
        with os.fdopen(fd, 'wb', closefd=False) as stream:
            stream.write(data)
            stream.flush()
            os.fsync(fd)
        after = os.fstat(fd)
        if ((before.st_dev, before.st_ino) != (after.st_dev, after.st_ino)
                or after.st_nlink != 1 or after.st_size != len(data)):
            raise ValueError('evidence leaf changed during write')
    finally:
        os.close(fd)


def stage_tests(path):
    """Trusted parent copy; the unprivileged sandbox never traverses host home."""
    source_fd = directory(HERE)
    destination_fd = None
    try:
        destination_fd = new_directory(path)
        os.fchmod(destination_fd, 0o755)
        for name, maximum in {'produce.py': 64 << 10, 'test_produce.py': 64 << 10,
                              'pins.json': 8 << 10, 'VERSION.dat': 1 << 10}.items():
            data = read_regular(source_fd, name, maximum)
            write_new(destination_fd, name, data)
            fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW, dir_fd=destination_fd)
            try:
                os.fchmod(fd, 0o644)
            finally:
                os.close(fd)
    finally:
        os.close(source_fd)
        if destination_fd is not None:
            os.close(destination_fd)


def elf(data, soname):
    """Independent bounded ELF64 inspection; never load a candidate library."""
    if len(data) < 64 or data[:7] != b'\x7fELF\x02\x01\x01':
        raise ValueError('not little-endian ELF64')
    header = struct.unpack_from('<HHIQQQIHHHHHH', data, 16)
    if header[0] != 3 or header[1] != 62 or header[10] != 64 or not 0 < header[11] <= 4096:
        raise ValueError('unexpected native ELF target')
    offset, count, names_index = header[5], header[11], header[12]
    if offset + count * 64 > len(data) or names_index >= count:
        raise ValueError('ELF section table exceeds library')
    sections = [struct.unpack_from('<IIQQQQIIQQ', data, offset + i * 64) for i in range(count)]

    def section(item):
        start, size = item[4], item[5]
        if start + size > len(data):
            raise ValueError('ELF section exceeds library')
        return data[start:start + size]

    def string(table, start):
        if not 0 <= start < len(table):
            raise ValueError('ELF string outside table')
        end = table.find(b'\0', start, min(len(table), start + 4096))
        if end < 0:
            raise ValueError('unterminated ELF string')
        return table[start:end].decode('ascii')

    names = section(sections[names_index])
    by_name = {}
    for item in sections:
        name = string(names, item[0])
        if name in by_name and name:
            raise ValueError('duplicate ELF section')
        by_name[name] = item
    strings = section(by_name['.dynstr'])
    dynamic = section(by_name['.dynamic'])
    if len(dynamic) % 16:
        raise ValueError('incomplete ELF dynamic table')
    needed, actual_soname = [], []
    for position in range(0, len(dynamic), 16):
        tag, value = struct.unpack_from('<qQ', dynamic, position)
        if tag == 1:
            needed.append(string(strings, value))
        elif tag == 14:
            actual_soname.append(string(strings, value))
        elif tag in (15, 29):
            raise ValueError('candidate embeds a runtime search path')
    allowed = {'libc.so.6', 'libdl.so.2', 'libpthread.so.0'}
    if soname == 'libssl.so.3':
        allowed.add('libcrypto.so.3')
        if 'libcrypto.so.3' not in needed:
            raise ValueError('SSL library does not use the candidate crypto ABI')
    if actual_soname != [soname] or 'libc.so.6' not in needed or not set(needed) <= allowed:
        raise ValueError('native SONAME/dependencies differ')
    symbols = section(by_name['.dynsym'])
    if len(symbols) % 24:
        raise ValueError('incomplete ELF dynamic symbols')
    exports = []
    for position in range(0, len(symbols), 24):
        name, info, other, index, value, size = struct.unpack_from('<IBBHQQ', symbols, position)
        if index and info >> 4 in (1, 2) and other & 3 in (0, 3):
            exports.append(string(strings, name))
    required = {'OpenSSL_version'} if soname == 'libcrypto.so.3' else {'SSL_CTX_new', 'SSL_CTX_free', 'TLS_method'}
    if not required <= set(exports):
        raise ValueError('required native ABI symbols absent')
    return {'soname': soname, 'needed': sorted(needed), 'exports': sorted(exports),
            'versionDefinitionsSHA256': digest(section(by_name['.gnu.version_d']))}


def tests(data, required, cases=()):
    text = data.decode('utf-8')
    if 'All tests successful.' not in text or 'Result: PASS' not in text:
        raise ValueError('upstream test summary did not pass')
    for name in required:
        matches = re.findall(r'(?m)^.*\d+-' + re.escape(name) + r'\.t\s+\.+([^\n]*)$', text)
        if len(matches) != 1 or matches[0].strip() not in ('', 'ok'):
            raise ValueError('required upstream recipe missing/skipped: ' + name)
    for case in cases:
        matches = re.findall(r'(?m)^\s*ok [0-9]+ - ' + re.escape(case) + r'\s*$', text)
        if not matches:
            raise ValueError('required upstream regression missing/skipped: ' + case)
    skips = [line for line in text.splitlines() if re.search(r'(?i)#\s*skip|skipped:', line)]
    if len(skips) > 4096:
        raise ValueError('oversized skip inventory')
    return {'requiredRecipes': required, 'requiredCases': list(cases), 'skips': skips,
            'summaries': re.findall(r'(?m)^Files=\d+, Tests=\d+.*$', text)}


def validate_outputs(outputs):
    for name in ('libcrypto.so.3', 'libssl.so.3'):
        elf(outputs[name], name)
    version = outputs['version.txt'].decode('ascii')
    if not version.startswith('OpenSSL ' + PINS['version'] + ' '):
        raise ValueError('actual development version differs')
    with tarfile.open(fileobj=io.BytesIO(outputs['native.tar.gz']), mode='r:gz') as archive:
        members = list(archive)
        if {m.name for m in members} != {'libcrypto.so.3', 'libssl.so.3', 'LICENSE.txt'} or len(members) != 3:
            raise ValueError('native archive member inventory differs')
        for member in members:
            if (not member.isfile() or member.uid or member.gid or member.mtime != PINS['epoch']
                    or member.size != len(outputs[member.name])
                    or archive.extractfile(member).read() != outputs[member.name]):
                raise ValueError('native archive member identity differs')
    return {'fullTests': tests(outputs['full_tests.log'], PINS['regressions'], PINS['regressionCases']),
            'regressionTests': tests(outputs['regression_tests.log'], PINS['regressions'], PINS['regressionCases']),
            'elf': {name: elf(outputs[name], name) for name in ('libcrypto.so.3', 'libssl.so.3')},
            'members': {name: {'size': len(data), 'sha256': digest(data)} for name, data in outputs.items()}}


def build_command():
    return ['-i', 'PATH=/usr/bin:/bin', 'LANG=C', 'LC_ALL=C', 'TZ=UTC',
            'HOME=/work/home', 'TMPDIR=/work/tmp', 'SOURCE_DATE_EPOCH=' + str(PINS['epoch']),
            '/bin/bash', '/input/build.sh']


def scratch_boundary(work_fd, result_fd):
    for fd in (work_fd, result_fd):
        info = os.fstat(fd)
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != 65532 or info.st_gid != 65532:
            raise ValueError('scratch directory ownership differs')
    filesystem = os.fstatvfs(work_fd)
    if (filesystem.f_blocks * filesystem.f_frsize != 2 << 30
            or filesystem.f_flag & (os.ST_NOSUID | os.ST_NODEV) != (os.ST_NOSUID | os.ST_NODEV)):
        raise ValueError('scratch disk bound/mount flags differ')


def inspect_boundary(value, image, work, inputs):
    host = value['HostConfig']
    mounts = {(m['Source'], m['Destination'], m['RW']) for m in value['Mounts']}
    limits = {v['Name']: (v['Soft'], v['Hard']) for v in host.get('Ulimits', [])}
    if (value['Image'] != image['Id'] or value['Config']['User'] != '65532:65532'
            or value['Config']['Entrypoint'] != ['/usr/bin/env']
            or value['Config']['Cmd'] != build_command()
            or value['Config']['Env'] != image['Config']['Env']
            or host['NetworkMode'] != 'none' or host['IpcMode'] != 'none'
            or not host['ReadonlyRootfs'] or host['Privileged'] or host.get('Devices')
            or host['Memory'] != 2 << 30 or host['MemorySwap'] != 2 << 30
            or host['NanoCpus'] != 2000000000 or host['PidsLimit'] != 128
            or host['CapDrop'] != ['ALL'] or host.get('CapAdd')
            or 'no-new-privileges' not in host['SecurityOpt']
            or limits != {'nproc': (128, 128), 'nofile': (512, 512),
                          'fsize': (33554432, 33554432), 'cpu': (1200, 1200)}
            or mounts != {(str(work), '/work', True), (str(inputs), '/input', False)}):
        raise ValueError('native build isolation differs')


def release_scratch(source_fd, work_fd, work, mounted, output_fd, lane, failure, cleanup):
    """Called only after owned container removal; refusal cannot skip cleanup."""
    try:
        if failure is not None and source_fd is not None:
            for file in ('build.log', 'full_tests.log', 'regression_tests.log', 'version.txt'):
                try:
                    data = read_regular(source_fd, file, FILES[file], allow_empty=True)
                except FileNotFoundError:
                    continue
                write_new(output_fd, lane + '_failure_' + file, data)
    finally:
        try:
            try:
                if source_fd is not None:
                    os.close(source_fd)
            finally:
                if work_fd is not None:
                    os.close(work_fd)
        finally:
            if mounted:
                checked(['sudo', 'umount', str(work)])
                cleanup['scratchUnmounted'] = True
            os.rmdir(work)


def build(inputs, output_fd, lane, source_sha, image):
    name = 'kanz-openssl-source-' + uuid.uuid4().hex
    temporary = Path(tempfile.mkdtemp(prefix='kanz-openssl-source-'))
    cleanup = {}
    try:
        work = temporary / 'scratch'
        work.mkdir()
        mounted, source_fd, work_fd = False, None, None
        try:
            checked(['sudo', 'mount', '-t', 'tmpfs', '-o', 'size=2147483648,mode=0755,nosuid,nodev', 'tmpfs', str(work)])
            mounted = True
            checked(['sudo', 'chown', str(os.getuid()) + ':' + str(os.getgid()), str(work)])
            (work / 'result').mkdir()
            work_fd = os.open(work, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            source_fd = os.open(work / 'result', os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            checked(['sudo', 'chown', '65532:65532', str(work), str(work / 'result')])
            scratch_boundary(work_fd, source_fd)
            checked(['docker', 'create', '--name', name, '--label', 'kanz.openssl-source=' + source_sha,
                     '--platform', 'linux/amd64', '--network', 'none', '--ipc', 'none', '--read-only',
                     '--user', '65532:65532', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges',
                     '--memory', '2g', '--memory-swap', '2g', '--cpus', '2', '--pids-limit', '128',
                     '--ulimit', 'nproc=128:128', '--ulimit', 'nofile=512:512',
                     '--ulimit', 'fsize=33554432:33554432', '--ulimit', 'cpu=1200:1200',
                     '--mount', 'type=bind,src=' + str(work) + ',dst=/work',
                     '--mount', 'type=bind,src=' + str(inputs) + ',dst=/input,readonly',
                     '--entrypoint', '/usr/bin/env', PINS['builder'], *build_command()])
            info = json.loads(checked(['docker', 'inspect', name]))[0]
            inspect_boundary(info, image, work, inputs)
            checked(['docker', 'start', '--attach', name], timeout=1800)
            info = json.loads(checked(['docker', 'inspect', name]))[0]
            inspect_boundary(info, image, work, inputs)
            if info['State']['Running'] or info['State']['OOMKilled'] or info['State']['ExitCode']:
                raise ValueError('native build did not finish cleanly')
            outputs = {file: read_regular(source_fd, file, maximum) for file, maximum in FILES.items()}
            if sum(map(len, outputs.values())) > 96 << 20:
                raise ValueError('cumulative producer output bound exceeded')
            report = validate_outputs(outputs)
            report['process'] = {key: info['State'][key] for key in ('Running', 'OOMKilled', 'ExitCode')}
            report['cleanup'] = cleanup
            for file, data in outputs.items():
                write_new(output_fd, lane + '_' + file, data)
            return report, outputs
        finally:
            failure = sys.exc_info()[1]
            # The generated name and label establish exclusive cleanup ownership.
            listing = ['docker', 'ps', '-a', '--filter', 'name=^/' + name + '$', '--format', '{{.Names}}']
            existing = checked(listing).decode().strip()
            if existing:
                if existing != name:
                    raise ValueError('ambiguous native cleanup inventory')
                info = json.loads(checked(['docker', 'inspect', name]))[0]
                if info['Config'].get('Labels', {}).get('kanz.openssl-source') != source_sha:
                    raise ValueError('native cleanup ownership differs')
                checked(['docker', 'rm', '--force', name])
            if checked(listing).strip():
                raise ValueError('owned native container remains')
            cleanup.update(container=name, label=source_sha, removed=bool(existing), remaining=0)
            try:
                release_scratch(source_fd, work_fd, work, mounted, output_fd, lane, failure, cleanup)
            except BaseException as cleanup_error:
                if failure is None:
                    raise
                cleanup['collectionOrScratchError'] = type(cleanup_error).__name__
                failure.add_note('Protected collection/scratch cleanup refused: ' + type(cleanup_error).__name__)
            if failure is not None:
                try:
                    write_new(output_fd, lane + '_failure.json', json.dumps({
                        'sourceSHA': source_sha, 'builderImageID': image['Id'],
                        'errorType': type(failure).__name__, 'consumerAccepted': False,
                        'cleanup': cleanup}, sort_keys=True, indent=2).encode())
                except BaseException as evidence_error:
                    failure.add_note('Failure evidence write refused: ' + type(evidence_error).__name__)
    finally:
        # Never recursively delete a possibly mounted scratch tree after an
        # ownership/daemon failure. Its unique path remains available to repair.
        if not any(temporary.iterdir()):
            os.rmdir(temporary)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--archive', type=Path)
    parser.add_argument('--run-id', required=True)
    parser.add_argument('--attempt', required=True)
    args = parser.parse_args()
    if sys.platform != 'linux' or os.uname().machine != 'x86_64':
        raise ValueError('Linux amd64 with Docker and bounded tmpfs mounting is required')
    if not all(re.fullmatch(r'[1-9][0-9]{0,19}', x) for x in (args.run_id, args.attempt)):
        raise ValueError('invalid run identity')
    source_sha, source_tree = checked(['git', '-C', str(HERE), 'rev-parse', 'HEAD', 'HEAD^{tree}']).decode().splitlines()
    checked(['git', '-C', str(HERE), 'diff', '--quiet', 'HEAD', '--'])
    # Pull/authentication occur outside this program; no token or Docker config
    # is inherited by the native build. Refuse a locally substituted image.
    image = json.loads(checked(['docker', 'image', 'inspect', PINS['builder']]))[0]
    if (image['Os'] != 'linux' or image['Architecture'] != 'amd64'
            or not any(x.endswith('@' + PINS['builder'].split('@')[1]) for x in image['RepoDigests'])):
        raise ValueError('immutable builder identity differs')
    raw = read_path(args.archive, 64 << 20) if args.archive else fetch(PINS['url'], 64 << 20)
    source_archive(raw)
    commit = upstream_commit()
    fixes = provenance()
    output = args.output.absolute()
    output_fd = new_directory(output)
    try:
        with tempfile.TemporaryDirectory(prefix='kanz-openssl-input-') as temporary:
            inputs = Path(temporary)
            os.chmod(inputs, 0o755)
            for name, data in {'source.tar.gz': raw, 'build.sh': (HERE / 'build.sh').read_bytes(),
                               'VERSION.dat': (HERE / 'VERSION.dat').read_bytes()}.items():
                (inputs / name).write_bytes(data)
                os.chmod(inputs / name, 0o444)
            one, first = build(inputs, output_fd, 'one', source_sha, image)
            two, second = build(inputs, output_fd, 'two', source_sha, image)
        for name in ('native.tar.gz', 'libcrypto.so.3', 'libssl.so.3', 'LICENSE.txt', 'toolchain.txt', 'configure.txt', 'version.txt'):
            if first[name] != second[name]:
                raise ValueError('independent builds differ: ' + name)
        report = {'schema': 1, 'purpose': 'unaccepted-native-source-input', 'consumerAccepted': False,
                  'sourceSHA': source_sha, 'sourceTree': source_tree, 'runID': args.run_id,
                  'runAttempt': args.attempt, 'pins': PINS, 'upstreamCommit': commit, 'vendorFixes': fixes,
                  'builder': {'reference': PINS['builder'], 'imageID': image['Id'], 'rootFS': image['RootFS']},
                  'recipeSHA256': digest((HERE / 'build.sh').read_bytes()),
                  'independentBuildsEqual': True,
                  'ownedCleanupPassed': all(x['cleanup'].get('remaining') == 0
                                           and x['cleanup'].get('scratchUnmounted') for x in (one, two)),
                  'builds': [one, two]}
        write_new(output_fd, 'receipt.json', (json.dumps(report, sort_keys=True, indent=2) + '\n').encode())
        print('Native source input reproduced; Julia compatibility and advisory acceptance remain unverified')
    finally:
        os.close(output_fd)


if __name__ == '__main__':
    def interrupt(signum, frame):
        raise KeyboardInterrupt('producer interrupted')
    signal.signal(signal.SIGTERM, interrupt)
    if len(sys.argv) == 3 and sys.argv[1] == '--stage-tests':
        stage_tests(Path(sys.argv[2]))
    else:
        main()
