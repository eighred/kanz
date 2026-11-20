"""Stage pinned production source and CMake inputs for the 50 LLVM members linked into Julia."""
import argparse
import hashlib
import io
import json
from pathlib import Path
import subprocess
import tarfile
from urllib.request import Request, urlopen

import compiler_owners
import core_runtime
import gcc_runtime


SHARD = {'bytes': 877555457, 'sha256': '2911dc9c27d085985d2274b1b7a3cb7f2d3e96ef8ae0e83d979b927cead46cd5'}
ORIGINAL_HEADER = 'c254d598f6cd70afc229131226854e75a69226736faddb92e46d89ebddbc0df6'
FIXED_HEADER = 'c830134fa1d0c8979f09724d1d251e318d9883462d3c8962eff8e473bbcb737f'


def ledger():
    return json.loads(Path(__file__).with_name('llvm_core_sources.json').read_bytes())


def collect(directory):
    for name, expected in ledger()['fork'].items():
        path = directory / name
        if path.exists():
            if gcc_runtime.digest(path) != expected:
                raise ValueError('cached fork input differs: ' + name)
            continue
        url = 'https://raw.githubusercontent.com/JuliaLang/llvm-project/' + compiler_owners.LLVM_FORK + '/' + name
        with urlopen(Request(url, headers={'User-Agent': 'Kanz LLVM source qualification'}), timeout=30) as stream:
            data = stream.read(3 << 20)
        if hashlib.sha256(data).hexdigest() != expected:
            raise ValueError('LLVM fork input differs: ' + name)
        path.parent.mkdir(exist_ok=True, parents=True)
        with path.open('xb') as out:
            out.write(data)


def tar_entry(output, name, data, expected):
    if hashlib.sha256(data).hexdigest() != expected:
        raise ValueError('LLVM source or publisher CMake module differs: ' + name)
    entry = tarfile.TarInfo(name)
    entry.size = len(data)
    entry.mode = 0o755 if name == 'llvm/cmake/config.guess' else 0o644
    output.addfile(entry, io.BytesIO(data))


def prepare(sources, llvm, bootstrap, delta, output):
    core_runtime.validate_file(llvm, core_runtime.inputs()['files']['full-parent.tar.gz'])
    pins = {**gcc_runtime.inputs()['shards'], 'gcc-gnu-bootstrap.tar.gz': SHARD}
    for name, pin in pins.items():
        core_runtime.validate_file(bootstrap / name, pin)
    row = core_runtime.inputs()['gcc9Headers']['114691']
    if gcc_runtime.digest(delta) != row['patchSHA256']:
        raise ValueError('Red Hat production header delta differs')
    output.mkdir()
    fixed = output / 'headers/libstdc++-v3/include/bits/stl_bvector.h'
    with tarfile.open(bootstrap / 'gcc-gnu-bootstrap.tar.gz', 'r|gz') as archive:
        for member in archive:
            if member.name.endswith('/include/c++/10.2.0/bits/stl_bvector.h'):
                core_runtime.extract_file(archive, member, fixed)
    if gcc_runtime.digest(fixed) != ORIGINAL_HEADER:
        raise ValueError('GNU 10.2 vector header differs')
    subprocess.run(['git', '-c', 'core.autocrlf=false', 'apply', '--whitespace=nowarn', '-'],
                   cwd=output / 'headers', input=delta.read_bytes(), check=True, timeout=20)
    if gcc_runtime.digest(fixed) != FIXED_HEADER:
        raise ValueError('GNU 10.2 vector header postimage differs')
    records = ledger()
    remaining = records['jllCMake'].copy()
    with tarfile.open(output / 'source.tar', 'x') as target:
        for name, sha in records['fork'].items():
            tar_entry(target, name, (sources / name).read_bytes(), sha)
        with tarfile.open(llvm, 'r|gz') as archive:
            for member in archive:
                parent = Path(member.name).parent.as_posix()
                name = 'llvm/cmake/modules/' + Path(member.name).name
                if parent == 'lib/cmake/llvm' and member.isfile() and name in remaining:
                    tar_entry(target, name, archive.extractfile(member).read(), remaining.pop(name))
    if gcc_runtime.digest(output / 'source.tar') != '0964601c2bfcac083e04a55d369408b62ff167dab42928dadd74d21ac47019be':
        raise ValueError('prepared LLVM source archive differs')
    if remaining:
        raise ValueError('publisher CMake modules missing')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    actions = parser.add_subparsers(dest='action', required=True)
    actions.add_parser('collect').add_argument('sources', type=Path)
    prep = actions.add_parser('prepare')
    for name in ('sources', 'llvm_full_archive', 'bootstrap', 'delta', 'output'):
        prep.add_argument(name, type=Path)
    args = parser.parse_args()
    if args.action == 'collect':
        collect(args.sources)
    else:
        prepare(args.sources, args.llvm_full_archive, args.bootstrap, args.delta, args.output)
