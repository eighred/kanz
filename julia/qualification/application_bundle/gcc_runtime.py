"""Prepare signed GCC 15.2 production sources and verify the replacement shared-library ABI."""
import argparse
import copy
import hashlib
import io
import json
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import tempfile


RELEASE = '5115c7e447fc07457443df874bf57840e8316d5f'
TREE = '9009fb54dc3d90add1cd8af44b66194ae7895bf7'
TAG = 'dcd428f94ffb464418f996ffb70dfa398f5caa3f'
SIGNER = '13975A70E63C361C73AE69EF6EEB81F8981C74C7'
KEYRING = '8013de59880f7a59f53b6bd207f4d2a408d67b84a82fa9cba1dbbf443428e2db'
GCC_ARCHIVE = '0a1cfcef7d3903f706c94ceba9f49a08bf3acd8ce5258270f7f3016290f7c4ee'
PREPARED = 'f3f407120e0043539367a09147cf0f51bd0d66ecffdd153358b0331a1738598d'
ORIGINAL = 'a6cdcc137df8cca9baa134d9ea7f06cd8aca070bd029111d6264f86f100005be'
REPLACEMENT = '18c8d1e340b784387ca1fdda0d1b90c5fe94842517a6b5151dc71387cbf409b1'
LIB = 'libstdc++.so.6.0.34'
SCOPE = {'config', 'include', 'libstdc++-v3', 'libgcc', 'libgfortran', 'libgomp', 'libssp', 'libquadmath',
         'libatomic', 'libiberty', 'libtool', 'gcc', 'fixincludes', 'contrib', 'intl', 'zlib',
         'libbacktrace', 'libdecnumber', 'libcpp', 'lto-plugin'}


def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def inputs():
    return json.loads(Path(__file__).with_name('gcc_runtime_inputs.json').read_bytes())


def verify_shards(directory):
    for name, expected in inputs()['shards'].items():
        path = directory / name
        if path.stat().st_size != expected['bytes'] or digest(path) != expected['sha256']:
            raise ValueError('GCC bootstrap shard differs: ' + name)


def git(database, *args):
    return subprocess.check_output(['git', '--no-replace-objects', '--git-dir', str(database), *args], timeout=90)


def selected(path):
    parts = path.split('/')
    if len(parts) == 1:
        return True
    if parts[0] not in SCOPE:
        return False
    if {'testsuite', 'tests', 'test', 'regression', 'regress', 'fuzz'}.intersection(parts[:-1]):
        return parts[-1] in ('Makefile.in', 'Makefile.am')
    return True


def tag_identity(database, keyring, workspace):
    if digest(keyring) != KEYRING or git(database, 'rev-parse', RELEASE + '^{tree}').decode().strip() != TREE:
        raise ValueError('GNU keyring or source tree differs')
    tag = git(database, 'cat-file', 'tag', TAG)
    body, delimiter, signature = tag.partition(b'-----BEGIN PGP SIGNATURE-----')
    if (not delimiter or not body.startswith(('object ' + RELEASE + '\ntype commit\ntag releases/gcc-15.2.0\n').encode())
            or hashlib.sha1(b'tag ' + str(len(tag)).encode() + b'\0' + tag).hexdigest() != TAG):
        raise ValueError('signed release tag differs')
    (workspace / 'tag').write_bytes(body)
    (workspace / 'sig').write_bytes(delimiter + signature)
    status = subprocess.check_output(['gpgv', '--homedir', str(workspace), '--status-fd', '1', '--keyring',
                                      str(keyring.resolve()), str(workspace / 'sig'), str(workspace / 'tag')], timeout=20)
    valid = [line.split() for line in status.decode().splitlines() if line.startswith('[GNUPG:] VALIDSIG ')]
    if len(valid) != 1 or valid[0][-1] != SIGNER:
        raise ValueError('unexpected GCC release signer')


def backports(database, changes, workspace):
    patched = {}
    for fix, item in inputs()['backports'].items():
        path, parent = item['path'], item['parent']
        header = git(changes, 'cat-file', 'commit', fix).split(b'\n\n', 1)[0].splitlines()
        if [row[7:].decode() for row in header if row.startswith(b'parent ')] != [parent] or path in patched:
            raise ValueError('unexpected release backport parent or duplicate target')
        target = workspace / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(git(database, 'show', RELEASE + ':' + path))
        patch = git(changes, 'diff', '--no-ext-diff', '--no-textconv', parent, fix, '--', path)
        subprocess.run(['git', '-c', 'core.autocrlf=false', 'apply', '--whitespace=nowarn', '-'], input=patch, cwd=workspace, check=True, timeout=15)
        if digest(target) != item['postSHA256']:
            raise ValueError('reviewed GCC production postimage differs: ' + path)
        patched[path] = target.read_bytes()
    return patched


def prepare(source, database, changes, keyring, output):
    if source.stat().st_size != 151808915 or digest(source) != GCC_ARCHIVE:
        raise ValueError('GCC source archive differs')
    with tempfile.TemporaryDirectory() as tmp:
        workspace = Path(tmp)
        tag_identity(database, keyring, workspace)
        patched = backports(database, changes, workspace)
    records = {}
    for row in git(database, 'ls-tree', '-r', '-z', RELEASE).split(b'\0'):
        if row:
            header, name = row.split(b'\t', 1)
            mode, kind, oid = header.split()
            path = name.decode('utf-8', 'surrogateescape')
            if kind == b'blob' and selected(path):
                records[path] = (mode, oid.decode('ascii'))
    prefix = 'gcc-' + RELEASE + '/'
    seen = set()
    with tarfile.open(source, 'r|gz') as archive, tarfile.open(output, 'x') as prepared:
        for original in archive:
            name = original.name.removeprefix(prefix)
            if name not in records:
                continue
            mode, oid = records[name]
            if name in seen:
                raise ValueError('duplicate GCC source')
            seen.add(name)
            if original.issym() and mode == b'120000':
                data = original.linkname.encode()
            elif original.isfile() and mode in (b'100644', b'100755'):
                data = archive.extractfile(original).read()
            else:
                raise ValueError('GCC Git mode differs')
            if hashlib.sha1(b'blob ' + str(len(data)).encode() + b'\0' + data).hexdigest() != oid:
                raise ValueError('GCC source differs from signed Git tree')
            entry = copy.copy(original)
            entry.name = 'gcc/' + name
            if original.issym():
                prepared.addfile(entry)
            else:
                content = patched.get(name, data)
                entry.size = len(content)
                prepared.addfile(entry, io.BytesIO(content))
    if seen != records.keys() or not patched.keys() <= seen or digest(output) != PREPARED:
        raise ValueError('prepared production source inventory differs')


def abi(path):
    def read(args):
        return subprocess.check_output([*args, str(path)], text=True, timeout=40)
    table = read(['nm', '-D', '--defined-only', '--format=posix']).splitlines()
    dynamic = read(['readelf', '-dW'])
    return {'symbols': sorted(tuple(row.split()[:2]) for row in table),
            'needed': sorted(re.findall(r'\(NEEDED\).*?\[([^\]]+)\]', dynamic)),
            'soname': re.findall(r'\(SONAME\).*?\[([^\]]+)\]', dynamic),
            'glibc': sorted(set(re.findall(r'Name: (GLIBC_[0-9.]+)', read(['readelf', '-VW']))))}


def validate_abi(original, replacement):
    if (digest(original), digest(replacement)) != (ORIGINAL, REPLACEMENT):
        raise ValueError('original or rebuilt libstdc++ differs')
    baseline, current = abi(original), abi(replacement)
    if (len(baseline['symbols']) != 6097 or baseline['symbols'] != current['symbols']
            or current['soname'] != baseline['soname'] or current['needed'] != baseline['needed']
            or not set(current['glibc']) <= set(baseline['glibc'])):
        raise ValueError('libstdc++ ABI differs')
    print('Matched all 6097 exported names/types, SONAME, dynamic dependencies and glibc bounds.')


def stage_compiler(compiler, rebuilt, new_compiler):
    original = compiler / 'lib/julia' / LIB
    validate_abi(original, rebuilt)
    shutil.copytree(compiler, new_compiler, symlinks=True)
    target = new_compiler / 'lib/julia' / LIB
    if target.is_symlink() or digest(target) != ORIGINAL:
        raise ValueError('unexpected compiler library target')
    with rebuilt.open('rb') as src, target.open('wb') as out:
        shutil.copyfileobj(src, out)
    if digest(target) != REPLACEMENT:
        raise ValueError('staged compiler libstdc++ differs')


def publisher_view(elfs, rebuilt, wrapper_archive):
    name = 'lib/julia/' + LIB
    actual = elfs[name]
    if actual['sha256'] != REPLACEMENT or rebuilt.stat().st_size != actual['size']:
        raise ValueError('packaged C++ runtime differs from reviewed build')
    with tempfile.TemporaryDirectory() as tmp, tarfile.open(wrapper_archive) as package:
        item, = [m for m in package if m.name == 'lib/' + LIB]
        if not item.isfile() or item.size != 21645816:
            raise ValueError('unsupported GCC publisher original')
        original = Path(tmp) / LIB
        with original.open('xb') as output:
            shutil.copyfileobj(package.extractfile(item), output)
        validate_abi(original, rebuilt)
    return {**elfs, name: {'size': item.size, 'sha256': ORIGINAL}}


def rebuild_record():
    return {'source': RELEASE, 'preparedSHA256': PREPARED, 'originalSHA256': ORIGINAL,
            'sha256': REPLACEMENT, 'path': 'lib/julia/' + LIB, 'exportedSymbols': 6097}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='action', required=True)
    prep = commands.add_parser('prepare')
    for arg in ('archive', 'release_git_dir', 'fixes_git_dir', 'gnu_keyring', 'new_output'):
        prep.add_argument(arg, type=Path)
    shards = commands.add_parser('verify-shards')
    shards.add_argument('directory', type=Path)
    check = commands.add_parser('abi')
    check.add_argument('original', type=Path)
    check.add_argument('rebuilt', type=Path)
    stage = commands.add_parser('stage-compiler')
    for arg in ('compiler', 'rebuilt', 'new_compiler'):
        stage.add_argument(arg, type=Path)
    a = parser.parse_args()
    if a.action == 'prepare':
        prepare(a.archive, a.release_git_dir, a.fixes_git_dir, a.gnu_keyring, a.new_output)
    elif a.action == 'verify-shards':
        verify_shards(a.directory)
    elif a.action == 'stage-compiler':
        stage_compiler(a.compiler, a.rebuilt, a.new_compiler)
    else:
        validate_abi(a.original, a.rebuilt)
