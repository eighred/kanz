"""Stage the supported PCRE2 8-bit runtime into a fresh, private compiler prefix."""
import argparse
import ctypes
import hashlib
import json
from pathlib import Path
import shutil
import tarfile
import tomllib


OLD = 'libpcre2-8.so.0.15.0'
NEW = 'libpcre2-8.so.0.16.1'
OLD_HASH = '7e8f71f7a768474cf9bc12c9b4eaa68c5c3ed40a5abfbf2c524fe46c6e11152d'
NEW_HASH = '041e83a9aa6adee377ec80f12044036b6f53c6082297d8e639a210e3dbfaf262'
ALIAS = ['libpcre2-8.so', 'libpcre2-8.so.0']
INPUTS = {
    'wrapperArtifacts': '8e9502f3b29798d2308b73d4c7105efc68a1c774351cea790aa2af8f929d8d6e',
    'recipe': '0934e2084304432aa2d5e54ec72b546281edc3810c955da8849ab8d0e78efa4b',
    'common': 'f7af2525845b31a0418f021bd30ce09184295a6471b90723493af2f5a8f052a9',
    'source': '53c156e1ba416a20da8e65395daa132da0d80e76910424caca3fcdae7831d384',
    'binary': '81a9d677e1a6e4ecb7f876d7fe941a5fc8d233c6c519bbdf8c1710da9929ae9b',
}
URL = 'https://github.com/JuliaBinaryWrappers/PCRE2_jll.jl/releases/download/PCRE2-v10.49.1+0/PCRE2.v10.49.1.x86_64-linux-gnu.tar.gz'


def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def publisher(inputs):
    for name, checksum in INPUTS.items():
        if digest(inputs / name) != checksum:
            raise ValueError('PCRE2 publisher input differs: ' + name)
    rows = tomllib.loads((inputs / 'wrapperArtifacts').read_text())['PCRE2']
    targets = [r for r in rows if {k: r[k] for k in r if k not in ('download', 'git-tree-sha1')} ==
               {'arch': 'x86_64', 'os': 'linux', 'libc': 'glibc'}]
    if (len(targets) != 1 or targets[0]['download'] != [{'url': URL, 'sha256': INPUTS['binary']}]
            or targets[0]['git-tree-sha1'] != '4ebde15137d6aa3c6b6ed61bbddc1e60fbb6068c'):
        raise ValueError('PCRE2 JLL platform differs')
    recipe, common = [(inputs / k).read_text() for k in ('recipe', 'common')]
    if ('build_pcre2(ARGS, "PCRE2", v"10.49.1", [8])' not in recipe
            or INPUTS['source'] not in common or 'pcre2-$(version.major).$(version.minor).tar.bz2' not in common):
        raise ValueError('PCRE2 upstream release and wrapper recipe differ')
    with tarfile.open(inputs / 'binary', 'r:gz') as archive:
        items = [m for m in archive if m.name == 'lib/' + NEW]
        if len(items) != 1 or not items[0].isfile() or items[0].size != 743696:
            raise ValueError('PCRE2 ELF member differs')
        data = archive.extractfile(items[0]).read()
    if hashlib.sha256(data).hexdigest() != NEW_HASH:
        raise ValueError('PCRE2 native bytes differ')
    return data


def version(path):
    library = ctypes.CDLL(str(path.resolve()))
    result = ctypes.create_string_buffer(64)
    query = library.pcre2_config_8
    query.argtypes = [ctypes.c_uint32, ctypes.c_void_p]
    query.restype = ctypes.c_int
    if not 0 < query(11, result) < len(result):
        raise ValueError('PCRE2_CONFIG_VERSION failed')
    return result.value.decode('ascii').split()[0]


def install(inputs, compiler, destination):
    data = publisher(inputs)
    original = compiler / 'lib/julia'
    if (original.is_symlink() or digest(original / OLD) != OLD_HASH or version(original / OLD) != '10.47'
            or any((original / n).readlink() != Path(OLD) for n in ALIAS)):
        raise ValueError('unexpected compiler PCRE2 preimage')
    # Allocate a new private prefix. The originating Julia tree stays intact.
    shutil.copytree(compiler, destination, symlinks=True)
    libs = destination / 'lib/julia'
    with (libs / NEW).open('xb') as stream:
        stream.write(data)
    (libs / NEW).chmod(0o755)
    for alias in ALIAS:
        (libs / alias).unlink()
        (libs / alias).symlink_to(NEW)
    (libs / OLD).unlink()
    if version(libs / ALIAS[1]) != '10.49':
        raise ValueError('PCRE2 runtime ABI or version differs')
    print('PCRE2 8-bit ABI control passed for upstream 10.47 and patched 10.49.')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    for key in ('inputs', 'compiler', 'new_compiler'):
        parser.add_argument(key, type=Path)
    a = parser.parse_args()
    install(a.inputs, a.compiler, a.new_compiler)
