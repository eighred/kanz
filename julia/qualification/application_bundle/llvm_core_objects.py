"""Compile the 50 mapped LLVM members with the original and corrected GCC 10.2 headers."""
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
from pathlib import Path
import shlex
import shutil
import struct
import subprocess

NATIVE_INPUTS = 'cd90329327f9a21fc0ed13c3756c73120418d904285d2094fed303d1454a9a16'
PRIVATE_CONFIG = 'a711cbb07a11e383a5c7e281cec21872765a5561aae58751c2c1f9109a7cfebf'
PUBLIC_CONFIG = 'd8fb9d66b95412cca2306a8f5f4f05b6f9cbaf57812dbd7f555aa8947b1bfe15'
TRIPLE_PUBLISHED = 'f517a6ef84ed8bb24e8fea50301ad937c8fef0fdd5c3afadcfa022634345756c'
TRIPLE_RECOMPILED = '51fbcfa1c075fc40819b9533735f75526e84ddcf335905ca1d5fc2e51e062d52'
COMPILER = 'x86_64-linux-gnu-g++'
FLAGS = ['-std=c++17', '-fPIC', '-fvisibility-inlines-hidden', '-fno-semantic-interposition', '-fno-gnu-unique',
         '-fno-lifetime-dse', '-O3', '-DNDEBUG', '-ffunction-sections', '-fdata-sections', '-fno-exceptions',
         '-funwind-tables', '-fno-rtti', '-D_GNU_SOURCE', '-D__STDC_CONSTANT_MACROS', '-D__STDC_FORMAT_MACROS',
         '-D__STDC_LIMIT_MACROS']
HEADERS = ('/bits/stl_bvector.h', '/bitset', '/erase_fn_imps.hpp', '/parallel/search.h', '/fstream.tcc')
DIRECTORY = Path('/tmp/llvm')


def checked(path, expected):
    h = hashlib.sha256()
    with path.open('rb') as source:
        for chunk in iter(lambda: source.read(1 << 20), b''):
            h.update(chunk)
    if h.hexdigest() != expected:
        raise ValueError('LLVM verification input differs: ' + str(path))


def equivalent(published, rebuilt, archive, member):
    if published == rebuilt:
        return 'byte-identical'
    if (archive != 'libLLVMTargetParser.a' or member != 'Triple.cpp.o'
            or hashlib.sha256(published).hexdigest() != TRIPLE_PUBLISHED
            or hashlib.sha256(rebuilt).hexdigest() != TRIPLE_RECOMPILED
            or rebuilt.count(b'CSWTCH.1868') != 2
            or rebuilt.replace(b'CSWTCH.1868', b'CSWTCH.1869') != published):
        raise ValueError('LLVM publisher object differs: ' + member)
    return 'two digits in the private switch-table section and symbol names'


def text_sections(data):
    offset = struct.unpack_from('<Q', data, 40)[0]
    size, count, table = struct.unpack_from('<HHH', data, 58)
    start, length = struct.unpack_from('<QQ', data, offset + size * table + 24)
    names = data[start:start + length]
    selected = {}
    for i in range(count):
        entry = offset + size * i
        name, kind, flags = struct.unpack_from('<IIQ', data, entry)
        if kind == 1 and flags & 4:
            start, length = struct.unpack_from('<QQ', data, entry + 24)
            selected[names[name:].split(b'\0', 1)[0]] = data[start:start + length]
    return selected


def build_member(item):
    archive, member = item
    owner = archive[len('libLLVM'):-2]
    source = DIRECTORY / 'lib' / owner / member[:-2]
    output = DIRECTORY / (owner + '-' + member)
    published = subprocess.check_output(['x86_64-linux-gnu-ar', 'p', str(DIRECTORY / 'lib' / archive), member])
    flags = [*FLAGS, '-I' + str(DIRECTORY / 'include'), '-I' + str(source.parent)]
    deps = subprocess.check_output([COMPILER, *flags, '-M', str(source)], universal_newlines=True, timeout=120)
    original_deps = set(shlex.split(deps.replace('\\\n', ' '))[1:])
    headers = sorted(p for p in original_deps if p.endswith(HEADERS))
    corrected_deps = subprocess.check_output([COMPILER, '-I/fixed', *flags, '-M', str(source)],
                                             universal_newlines=True, timeout=120)
    expected_deps = {'/fixed/bits/stl_bvector.h' if p.endswith('/bits/stl_bvector.h') else p for p in original_deps}
    if set(shlex.split(corrected_deps.replace('\\\n', ' '))[1:]) != expected_deps:
        raise ValueError('corrected GNU header dependency selection differs: ' + member)
    if any('/include/c++/10.2.0/' not in p for p in headers):
        raise ValueError('unexpected GNU header provider')
    binaries = []
    for extra in ([], ['-I/fixed']):
        subprocess.run([COMPILER, *extra, *flags, '-c', str(source), '-o', str(output)],
                       cwd=DIRECTORY, check=True, timeout=180)
        binaries.append(output.read_bytes())
    if binaries[0] != binaries[1]:
        raise ValueError('corrected header changes the object: ' + member)
    identity = equivalent(published, binaries[1], archive, member)
    sections = text_sections(published)
    if sections != text_sections(binaries[1]):
        raise ValueError('executable sections differ: ' + member)
    return {'archive': archive, 'member': member, 'headers': headers, 'comparison': identity,
            'publishedSHA256': hashlib.sha256(published).hexdigest(),
            'correctedSHA256': hashlib.sha256(binaries[1]).hexdigest(), 'executableSections': len(sections)}


def run():
    root = Path(__file__).resolve().parent
    if subprocess.check_output([COMPILER, '-dumpfullversion']).strip() != b'10.2.0':
        raise ValueError('GCC 10.2.0 required')
    checked(Path('/original-inputs.tar'), NATIVE_INPUTS)
    checked(Path('/fixed/bits/stl_bvector.h'), 'c830134fa1d0c8979f09724d1d251e318d9883462d3c8962eff8e473bbcb737f')
    checked(Path('/config/config.h'), PRIVATE_CONFIG)
    checked(Path('/config/llvm-config.h'), PUBLIC_CONFIG)
    checked(Path('/source.tar'), '0964601c2bfcac083e04a55d369408b62ff167dab42928dadd74d21ac47019be')
    DIRECTORY.mkdir()
    subprocess.run(['tar', '-xf', '/source.tar', '--wildcards', 'llvm/lib/*'], cwd='/tmp', check=True)
    owners = json.loads((root / 'core_runtime_inputs.json').read_bytes())['llvmMembers']
    members = [(archive, member) for archive in owners for member in owners[archive]]
    subprocess.run(['tar', '-xf', '/original-inputs.tar', '--wildcards', 'include/*',
                    *['lib/' + archive for archive in owners]], cwd=DIRECTORY, check=True)
    checked(DIRECTORY / 'include/llvm/Config/llvm-config.h', PUBLIC_CONFIG)
    shutil.copyfile('/config/config.h', DIRECTORY / 'include/llvm/Config/config.h')
    fork = json.loads((root / 'llvm_core_sources.json').read_bytes())['fork']
    for name, expected in fork.items():
        if name.startswith('llvm/lib/'):
            checked(Path('/tmp') / name, expected)
    with ThreadPoolExecutor(max_workers=3) as pool:
        rows = list(pool.map(build_member, members))
    counts = {h: sum(any(path.endswith(h) for path in row['headers']) for row in rows) for h in HEADERS}
    if (len(rows) != 50 or sum(row['executableSections'] for row in rows) != 2758
            or sum(row['comparison'] == 'byte-identical' for row in rows) != 49
            or list(counts.values()) != [44, 1, 0, 0, 0]):
        raise ValueError('LLVM member or GNU header denominator differs')
    Path('/out/llvm-core-members.json').write_text(json.dumps(rows, indent=2) + '\n')
    print('Matched all 50 corrected objects, 49 publisher SHA-256 digests and 2758 executable sections.')


if __name__ == '__main__':
    run()
