"""Bind the shipped compiler runtimes to their parent artifacts and record unresolved native advisories."""
import gzip
import hashlib
import tarfile
import tomllib

import gcc_advisories
import gcc_runtime


WRAPPERS = 'https://github.com/JuliaBinaryWrappers/'
RAW = 'https://raw.githubusercontent.com/'
LLVM_RELEASE = WRAPPERS + 'LLVM_full_jll.jl/releases/download/LLVM_full-v20.1.8%2B3/'
GCC_RELEASE = WRAPPERS + 'CompilerSupportLibraries_jll.jl/releases/download/CompilerSupportLibraries-v1.5.5%2B2/'
GCC_SHARD = 'a8a89fbf4e7dfa6a526b3ec18c060180c06bcc8e'
GCC_BUNDLE = '3ca5094dccdab7cdc190a8d429922dcf26587888197d39fd72a241bca0820433'
GCC_RUNTIME = 'a6cdcc137df8cca9baa134d9ea7f06cd8aca070bd029111d6264f86f100005be'
LLVM_FORK = '9cba7d4b7efbbc8d3755144e3deb741de8011b41'
LLVM_RUNTIME = 'af9c893f4421a12015a7129fddc9410485b661178598aa5b8be3853e0a1720bc'
INPUTS = {
    'full-parent.tar.gz': (LLVM_RELEASE + 'LLVM_full.v20.1.8.x86_64-linux-gnu-cxx11.tar.gz',
                           643900088, '176a3910fcbd5525494578f2344b61102eca80de1e81a910513951247c52a271'),
    'llvm-full-readme': (RAW + 'JuliaBinaryWrappers/LLVM_full_jll.jl/4c99685d71410eb0134fe08adb0f1b331b75eee9/README.md',
                         3879, '548d87cc290477f5b96856bbf82b66d05540bf34a5b9617a9ca4495e1e422aae'),
    'csl-log.tar.gz': (GCC_RELEASE + 'CompilerSupportLibraries-logs.v1.5.5.x86_64-linux-gnu-libgfortran5.tar.gz',
                       10002, '4f55a8ba451f083b232509f72612d95ddbc6dda0867df69e320a6cfe53ed25b7'),
    'bootstrap-artifacts': (RAW + 'JuliaPackaging/BinaryBuilderBase.jl/e436f7cf3f3ce545d55141180085e4f855c66a53/Artifacts.toml',
                            534003, '2fc59614616e2179a01c8ddc27cb1a18e3d0c9bca265502d4a5a054231cb5319'),
}


def require_member(archive, path, expected):
    names = 0
    with tarfile.open(archive, 'r|gz') as package:
        for member in package:
            if member.name != path:
                continue
            names += 1
            if (not member.isfile() or member.size != expected['size']
                    or hashlib.file_digest(package.extractfile(member), 'sha256').hexdigest() != expected['sha256']):
                raise ValueError('LLVM_full parent library differs from OCI')
    if names != 1:
        raise ValueError('LLVM_full parent member missing or ambiguous')


def gcc_pending(wrapper, gcc, mapped):
    rebuilt = wrapper.get('localRebuild')
    valid_pair = ((GCC_RUNTIME, None), (gcc_runtime.REPLACEMENT, gcc_runtime.rebuild_record()))
    if (wrapper['publisherArchiveSHA256'] != GCC_BUNDLE
            or wrapper['publisherFiles']['lib/' + gcc_runtime.LIB] != GCC_RUNTIME
            or (gcc, rebuilt) not in valid_pair):
        raise ValueError('actual C++ runtime differs from publisher')
    # The prebuilt Julia and LLVM consumers may have instantiated these headers.
    headers = ['CVE-2026-102010', 'CVE-2026-114690', 'CVE-2026-114691', 'CVE-2026-115455', 'CVE-2026-125709']
    if rebuilt:
        if mapped != {'CVE-2026-115456': gcc_advisories.FROM_CHARS, 'CVE-2026-125709': gcc_advisories.FILEBUF}:
            raise ValueError('backport mappings missing for rebuilt GCC')
        return {'libstdc++ header consumers': headers}
    return {'libstdc++': sorted([*headers, 'CVE-2026-95619', 'CVE-2026-115456'])}


def review(directory, receipt):
    for name, (_, size, sha) in INPUTS.items():
        file = directory / name
        with file.open('rb') as stream:
            if file.stat().st_size != size or hashlib.file_digest(stream, 'sha256').hexdigest() != sha:
                raise ValueError('compiler publisher evidence differs: ' + name)
    full = (directory / 'llvm-full-readme').read_text()
    if 'https://github.com/JuliaLang/llvm-project.git (revision: `' + LLVM_FORK + '`)' not in full:
        raise ValueError('LLVM parent source differs')
    llvm = receipt['native']['lib/julia/libLLVM.so.20.1jl']
    require_member(directory / 'full-parent.tar.gz', 'lib/libLLVM.so.20.1jl', llvm)
    if llvm['sha256'] != LLVM_RUNTIME:
        raise ValueError('LLVM library differs from reviewed native candidate')
    catalog = tomllib.loads((directory / 'bootstrap-artifacts').read_text())
    shard, = catalog['GCCBootstrap-x86_64-linux-gnu.v15.2.0.x86_64-linux-musl.unpacked']
    archive, = shard['download']
    if (shard['git-tree-sha1'] != GCC_SHARD or archive['sha256'] != '4641a9b4233a99addda84f71961c09c8f8b5f907c3a55f02f927b41bed2bf472'):
        raise ValueError('GCC compiler shard differs')
    with tarfile.open(directory / 'csl-log.tar.gz') as t:
        member = t.getmember('logs/CompilerSupportLibraries/CompilerSupportLibraries.log.gz')
        with gzip.GzipFile(fileobj=t.extractfile(member)) as raw:
            log = raw.read(1 << 20)
        if len(log) == 1 << 20 or '/artifacts/' + GCC_SHARD + '" -> "/opt/x86_64-linux-gnu/GCCBootstrap-15.2.0"' not in log.decode():
            raise ValueError('producer log does not select this GCC toolchain')
    wrapper = receipt['wrapperMetadata']['CompilerSupportLibraries_jll']
    gcc = receipt['native']['lib/julia/libstdc++.so.6.0.34']['sha256']
    rebuilt = wrapper.get('localRebuild')
    mappings = gcc_advisories.mappings(directory) if rebuilt else {}
    gcc_review = gcc_pending(wrapper, gcc, mappings)
    return {'image': receipt['image'], 'config': receipt['config'], 'gccRuntime': gcc, 'llvmRuntime': llvm['sha256'],
            'gccShard': GCC_SHARD, 'llvmFork': LLVM_FORK,
            'gcc15ReviewHead': 'a311d911f75385702f25eb0df5f54d46e39ba8da',
            'gcc15SourceChanges': {
                'CVE-2026-95619': '638de129df3a22917ff2eda13bd0fd861ca9befc',
                'CVE-2026-102010': '29e51a820d76c766ae417fcba912714337b0eb17',
                'CVE-2026-114690': 'b346c341218a6a9a52c56e2bd1516b35b51c2bd2',
                'CVE-2026-114691': 'dfc15010eda603e814caf9681a76c726dea2bf64',
                'CVE-2026-115455': '2ffbcde8634f69ae6086e2677bd90ecbad687f9f',
                'PR127666': '816c1a39ca9b656ba2d001c7b6fa421ebe44b3a0',
            },
            'gccPR118099SourceChanges': [
                '2d296e9f024edbb49cad1f3ebb5ae34c71ea5f7a', '43e4d697586364decbad7f67a204189afef6e765',
                'f46b50adcf752de0e1e0e24e17f42a5075770f48',
            ],
            'llvmUpstreamFix': '7bb9626d5ab901e5c1a8e9acbbb1684c982401b4',
            'localRebuild': rebuilt, 'packageOwnerMappings': mappings, 'pendingNativeReview': {**gcc_review, 'libLLVM': ['CVE-2026-13573', 'CVE-2026-13574']}}


def require_reviewed_compilers(report):
    if report['pendingNativeReview']:
        raise ValueError('native compiler runtime acceptance pending: ' + ', '.join(sorted(report['pendingNativeReview'])))
