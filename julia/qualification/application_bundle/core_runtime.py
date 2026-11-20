"""Prepare immutable public inputs for an offline Julia 1.13.3 internal runtime rebuild."""
import argparse
import copy
import hashlib
import json
import re
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
from urllib.request import Request, urlopen

import gcc_runtime


SIGNER = '64B779A570972FFF7BFC2B54EAD471E1A1F2C10A'
ORIGINAL = '661ae03e0685b66d5f4155bc040cf59986d18bf73dc9175bee92bc311d6c3b03'
REBUILT = '0af1866831d3f3b26b4cc18193db8730e47ba4c8019db1db4423982ce4606931'
BASE_LIBC = '6b4a45352fd0c540a9c7c718f35ce8c8e46a4e482f9d3885a910c32d1a0e1421'
LLVM_FILES = {'tools/llvm-config', 'lib/libLLVMSupport.a', 'lib/libLLVMTargetParser.a', 'lib/libLLVMDemangle.a',
              'lib/libLLVMBitstreamReader.a', 'lib/libLLVMCore.a', 'lib/libLLVMBinaryFormat.a', 'lib/libLLVMRemarks.a'}
LIBRARIES = {'lib/libuv.a', 'lib/libunwind.so.8.2.0', 'lib/libzstd.so.1.5.7'}


def inputs():
    return json.loads(Path(__file__).with_name('core_runtime_inputs.json').read_bytes())


def validate_file(path, expected):
    if path.stat().st_size != expected['bytes'] or gcc_runtime.digest(path) != expected['sha256']:
        raise ValueError('Julia internal build input differs: ' + path.name)


def collect_vector_patch(output):
    record = inputs()['gcc9Headers']['114691']
    request = Request(record['source'], headers={'User-Agent': 'Kanz GCC production header verification'})
    active = False
    source = bytearray()
    with urlopen(request, timeout=30) as stream:
        consumed = 0
        for line in stream:
            consumed += len(line)
            if consumed > 24000:
                raise ValueError('unrecognized Red Hat patch layout')
            if line.startswith((b'diff --git ', b'diff -')):
                if active:
                    break
                if b'libstdc++-v3/include/bits/stl_bvector.h' not in line:
                    raise ValueError('first patch file differs from the reviewed production header')
                active = True
            if line.startswith(b'--- '):
                if b'libstdc++-v3/include/bits/stl_bvector.h' in line:
                    active = True
                elif active:
                    break
            if active:
                source.extend(line)
    if hashlib.sha256(source).hexdigest() != record['patchSHA256']:
        raise ValueError('GCC 8 production patch differs')
    with output.open('xb') as f:
        f.write(source)


def extract_file(archive, member, target):
    if not member.isfile() or target.exists():
        raise ValueError('unexpected or duplicate regular file: ' + str(target))
    target.parent.mkdir(exist_ok=True, parents=True)
    with target.open('xb') as f:
        shutil.copyfileobj(archive.extractfile(member), f)


def prepare_headers(directory, fixes, destination, metadata):
    rows = metadata['gcc9Headers']
    needed = {r['relative']: r for r in rows.values()}
    originals = set()
    with tarfile.open(directory / 'gcc9.tar.gz', 'r|gz') as src:
        for member in src:
            _, prefix, name = member.name.partition('x86_64-linux-gnu/include/c++/9.1.0/')
            if not prefix or name not in needed:
                continue
            target = destination / 'libstdc++-v3/include' / name
            extract_file(src, member, target)
            if gcc_runtime.digest(target) != needed[name]['preSHA256']:
                raise ValueError('original GCC 9 header differs')
            originals.add(name)
    if originals != needed.keys():
        raise ValueError('required GCC 9 headers missing')
    for advisory, row in rows.items():
        name = 'libstdc++-v3/include/' + row['relative']
        if advisory == '114691':
            delta = (directory / 'gcc8-vector.production.diff').read_bytes()
        else:
            delta = gcc_runtime.git(fixes, 'diff', '--no-ext-diff', '--no-textconv', row['fix'] + '^', row['fix'], '--', name)
        if hashlib.sha256(delta).hexdigest() != row['patchSHA256']:
            raise ValueError('unexpected GCC header source delta')
        subprocess.run(['git', '-c', 'core.autocrlf=false', 'apply', '--whitespace=nowarn', '-'],
                       input=delta, cwd=destination, timeout=20, check=True)
        if gcc_runtime.digest(destination / name) != row['postSHA256']:
            raise ValueError('GCC header postimage differs')


def signer(directory):
    with tempfile.TemporaryDirectory() as tmp:
        gpg = Path(tmp) / 'julia.gpg'
        subprocess.run(['gpg', '--homedir', tmp, '--batch', '--dearmor', '--output', str(gpg),
                        str(directory / 'julia-release-key.asc')], timeout=30, check=True)
        status = subprocess.check_output(['gpgv', '--homedir', tmp, '--status-fd=1', '--keyring', str(gpg),
                                         str(directory / 'julia-1.13.3.tar.gz.asc'),
                                         str(directory / 'julia-1.13.3.tar.gz')], timeout=60, text=True)
    valid = [line.split() for line in status.splitlines() if line.startswith('[GNUPG:] VALIDSIG ')]
    if len(valid) != 1 or valid[0][2] != SIGNER or valid[0][-1] != SIGNER:
        raise ValueError('unexpected Julia source signer')


def prepare(directory, fixes, output):
    metadata = inputs()
    for name, record in metadata['files'].items():
        validate_file(directory / name, record)
    signer(directory)
    output.mkdir()
    shutil.copyfile(directory / 'julia-1.13.3.tar.gz', output / 'julia-1.13.3.tar.gz')
    prepare_headers(directory, fixes, output / 'headers', metadata)
    selected = set()
    with tarfile.open(directory / 'utf8proc.tar.gz') as src:
        for member in src:
            name = member.name.partition('/')[2]
            if name not in metadata['utf8procSource']:
                continue
            target = output / 'utf8proc' / name
            extract_file(src, member, target)
            data = target.read_bytes()
            oid = hashlib.sha1(b'blob ' + str(len(data)).encode() + b'\0' + data).hexdigest()
            if oid != metadata['utf8procSource'][name]['sha']:
                raise ValueError('utf8proc Git blob differs')
            selected.add(name)
    if selected != metadata['utf8procSource'].keys():
        raise ValueError('incomplete utf8proc source')
    libs = output / 'core-libraries'
    for name in ('LibUV', 'LibUnwind', 'Zstd'):
        with tarfile.open(directory / (name + '.tar.gz')) as src:
            for member in src:
                path = member.name.removeprefix('./')
                if member.isfile() and (path.startswith('include/') or path in LIBRARIES):
                    extract_file(src, member, libs / path)
    shutil.copyfile(directory / 'libstdc++.a', libs / 'lib/libstdc++.a')
    with tarfile.open(output / 'native-relink-inputs.tar', 'x') as out:
        for name in ('full-parent.tar.gz', 'julia.tar.gz'):
            with tarfile.open(directory / name, 'r|gz') as src:
                for member in src:
                    path = member.name.removeprefix('./')
                    if name == 'full-parent.tar.gz':
                        selected = path.startswith(('include/llvm/', 'include/llvm-c/')) or path in LLVM_FILES
                    else:
                        path = path.removeprefix('julia-1.13.3/')
                        selected = path.startswith(('include/julia/', 'lib/libjulia.so', 'lib/julia/libstdc++.so',
                                                    'lib/julia/libLLVM', 'lib/julia/libz.so', 'lib/julia/libgcc_s.so'))
                    if selected and (member.isfile() or member.issym()):
                        entry = copy.copy(member)
                        entry.name = path
                        if member.issym() and (member.linkname.startswith('/') or '..' in Path(member.linkname).parts):
                            raise ValueError('unexpected owner library symlink')
                        out.addfile(entry, src.extractfile(member) if member.isfile() else None)


def verify_shards(directory):
    expected = {**gcc_runtime.inputs()['shards'], 'gcc-gnu-bootstrap.tar.gz': inputs()['files']['gcc9.tar.gz']}
    for name, pin in expected.items():
        validate_file(directory / name, pin)


def profile(path):
    value = gcc_runtime.abi(path)
    dynamic = subprocess.check_output(['readelf', '-dW', str(path)], text=True, timeout=30)
    value['runpath'] = re.findall(r'\(RUNPATH\).*?\[([^\]]+)\]', dynamic)
    return value


def check_profiles(old, new):
    if (len(old['symbols']) != 1698 or len(old['needed']) != 11
            or old['soname'] != ['libjulia-internal.so.1.13'] or old['runpath'] != ['$ORIGIN:$ORIGIN/..']
            or any(old[key] != new[key] for key in ('symbols', 'needed', 'soname', 'runpath'))
            or set(new['glibc']) != set(old['glibc']) | {'GLIBC_2.17'}):
        raise ValueError('Julia internal ABI differs from the pinned application profile')


def dynamic_symbols(path, mode):
    result = subprocess.check_output(['nm', '-D', mode, '--format=posix', str(path)], text=True, timeout=30)
    return {line.split()[0] for line in result.splitlines()}


def verify_abi(original, rebuilt, libc, libstdcpp, linkmap):
    for path, sha in [(original, ORIGINAL), (rebuilt, REBUILT), (libc, BASE_LIBC), (libstdcpp, gcc_runtime.REPLACEMENT)]:
        if gcc_runtime.digest(path) != sha:
            raise ValueError('unexpected ELF identity: ' + path.name)
    check_profiles(profile(original), profile(rebuilt))
    required = 'secure_getenv@GLIBC_2.17'
    if ({s for s in dynamic_symbols(rebuilt, '--undefined-only') if s.endswith('@GLIBC_2.17')} != {required}
            or required not in dynamic_symbols(libstdcpp, '--undefined-only')
            or 'secure_getenv@@GLIBC_2.17' not in dynamic_symbols(libc, '--defined-only')):
        raise ValueError('GLIBC_2.17 provider or pre-existing application requirement differs')
    text = linkmap.read_text()
    linked = {}
    for owner, member in re.findall(r'^[^\s()]+/(libLLVM[^/()\s]+\.a)\(([^()]+)\)', text, re.M):
        linked.setdefault(owner, set()).add(member)
    if ({k: sorted(v) for k, v in linked.items()} != inputs()['llvmMembers']
            or '/tmp/julia-1.13.3/usr/lib/libstdc++.a(new_opa.o)' not in text):
        raise ValueError('LLVM or C++ static archive member selection differs')
    print('Matched 1698 exports, 11 dependencies, SONAME, RUNPATH, libc provider and 50 LLVM members.')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    actions = parser.add_subparsers(dest='action', required=True)
    collect = actions.add_parser('collect-vector-patch')
    collect.add_argument('output', type=Path)
    prep = actions.add_parser('prepare')
    prep.add_argument('inputs', type=Path)
    prep.add_argument('gcc_fixes_git_dir', type=Path)
    prep.add_argument('output', type=Path)
    shards = actions.add_parser('verify-shards')
    shards.add_argument('inputs', type=Path)
    compare = actions.add_parser('verify-abi')
    for name in ('original', 'rebuilt', 'libc', 'libstdcpp', 'linkmap'):
        compare.add_argument(name, type=Path)
    args = parser.parse_args()
    if args.action == 'collect-vector-patch':
        collect_vector_patch(args.output)
    elif args.action == 'verify-shards':
        verify_shards(args.inputs)
    elif args.action == 'verify-abi':
        verify_abi(args.original, args.rebuilt, args.libc, args.libstdcpp, args.linkmap)
    else:
        prepare(args.inputs, args.gcc_fixes_git_dir, args.output)
