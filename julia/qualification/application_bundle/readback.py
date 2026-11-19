"""Bind the complete application layer and native libraries to the actual base and compiler inputs."""
import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
import posixpath
import re
import subprocess
import tarfile
import tempfile
import tomllib
import sys
from urllib.parse import unquote

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import jll_advisory
import gcc_runtime
import pcre2


BASE_INDEX = 'be40c00dfabd86576d92666e87e406714d5618342de1a0c213ad232de255172e'
BASE_PLATFORM = '8e80c746dfbb1aa5891456d3414e5f7fafed1ecc62a6e1579f8ba4311b016d83'
JULIA = '5c61cb4b40332cb5a3504e7779e2f5dac093caa4a1116ded19f01413f85c63e4'
CA_PATH = 'share/julia/cert.pem'
CA_ARCHIVE = 'fbbfa980bd86eae3dbc77ee5f07749b68d56aad0eb6497ea6d5435c141fec51c'
CA_DATA = '796f1389bfea13c0ed78f663b765baaff54a508e5483786bd6ece73c5e3e3cbe'
CA_URL = ('https://github.com/JuliaBinaryWrappers/MozillaCACerts_jll.jl/releases/download/'
          'MozillaCACerts-v2026.10.20%2B0/MozillaCACerts.v2026.10.20.any.tar.gz')
GENERATED = {'bin/curve-worker', 'lib/julia/sys.so'}
# PackageCompiler 2.4.5's Linux runtime floor for this exact Julia 1.13.3 build.
JLL_LIBRARIES = {
    'CompilerSupportLibraries_jll': [
        'libatomic.so.1.2.0', 'libgcc_s.so.1', 'libgfortran.so.5.0.0', 'libgomp.so.1.0.0',
        'libquadmath.so.0.0.0', 'libssp.so.0.0.0', 'libstdc++.so.6.0.34',
    ],
    'dSFMT_jll': ['libdSFMT.so'],
    'GMP_jll': ['libgmp.so.10.5.0', 'libgmpxx.so.4.7.0'],
    'libLLVM_jll': ['libLLVM.so.20.1jl'],
    'LibUnwind_jll': ['libunwind.so.8.2.0'],
    'LibUV_jll': ['libuv.so.2.0.0'],
    'MPFR_jll': ['libmpfr.so.6.2.2'],
    'OpenLibm_jll': ['libopenlibm.so.4.0'],
    'PCRE2_jll': [pcre2.NEW],
    'Zlib_jll': ['libz.so.1.3.2'],
    'Zstd_jll': ['libzstd.so.1.5.7'],
}
JULIA_CORE = {'bin/julia', 'lib/libjulia.so.1.13.3', 'lib/julia/libjulia-codegen.so.1.13.3',
              'lib/julia/libjulia-internal.so.1.13.3'}
PATCHED = {'lib/julia/' + pcre2.NEW}
DISTRIBUTION = JULIA_CORE | {'lib/julia/' + name for owner, group in JLL_LIBRARIES.items()
                             for name in group if owner != 'PCRE2_jll'}
EMPTY_LAYER = 'sha256:4f4fb700ef54461cfa02571ae0db9a0dc1e0cdb5577484a6d75e68dc38e8acc1'
UNSHIPPED = re.compile(r'^(lib(?:ssl|crypto|curl|git2|ssh2|nghttp2)\.so(?:\..*)?|(?:p?7z|7zz|7za)(?:\.exe)?)$')


def hash_file(path):
    with path.open('rb') as handle:
        return hashlib.file_digest(handle, 'sha256').hexdigest()


def member_name(raw):
    name = raw.removeprefix('./').rstrip('/')
    p = PurePosixPath(name)
    if p.is_absolute() or '..' in p.parts or str(p) != (name or '.') or '\\' in name:
        raise ValueError('ambiguous archive member')
    return name


def dynamic(path):
    report = subprocess.check_output(['readelf', '--wide', '--dynamic', str(path)], text=True)
    names = re.findall(r'\((NEEDED|SONAME)\).*?\[(.*?)\]', report)
    if any(UNSHIPPED.fullmatch(name) for _, name in names):
        raise ValueError('unselected network or crypto dependency in ELF metadata')
    return names


def link_targets(files):
    targets = {}
    for name, item in files.items():
        if item[0] != 'link':
            continue
        visited = set()
        current = name
        while files.get(current, [None])[0] == 'link':
            if current in visited:
                raise ValueError('cyclic application link')
            visited.add(current)
            current = posixpath.normpath(posixpath.join(posixpath.dirname(current), files[current][1]))
        if files.get(current, [None])[0] != 'file':
            raise ValueError('application link has no regular target')
        targets[name] = current
    return targets


def contents(archive, prefix='', native=False):
    files, elfs = {}, {}
    count = size = 0
    with tempfile.TemporaryDirectory() as tmp:
        for member in archive:
            count += 1
            size += member.size
            if count > 50000 or size > 2 << 30:
                raise ValueError('unbounded application layer')
            path = member_name(member.name)
            if prefix:
                if member.isdir() and path in ('opt', 'opt/kanz'):
                    continue
                if not path.startswith(prefix):
                    raise ValueError('application layer changed the base')
                path = path[len(prefix):]
            if member.isdir():
                continue
            if path in files or any(part.startswith('.wh.') for part in PurePosixPath(path).parts):
                raise ValueError('duplicate application path or layer whiteout')
            if UNSHIPPED.fullmatch(PurePosixPath(path).name):
                raise ValueError('unselected native binary shipped in application')
            if member.issym():
                target = posixpath.normpath(posixpath.join(posixpath.dirname(path), member.linkname))
                if target.startswith(('/', '../')) or target == '..':
                    raise ValueError('link outside the candidate application')
                files[path] = ['link', member.linkname]
            elif member.isfile():
                with archive.extractfile(member) as handle:
                    head = handle.read(4)
                    digest = hashlib.sha256(head)
                    if head == b'\x7fELF' and native:
                        extracted = Path(tmp) / 'elf'
                        with extracted.open('wb') as out:
                            out.write(head)
                            while block := handle.read(1024 * 1024):
                                out.write(block)
                                digest.update(block)
                        elfs[path] = {'sha256': digest.hexdigest(), 'size': member.size, 'dynamic': dynamic(extracted)}
                    else:
                        while block := handle.read(1024 * 1024):
                            digest.update(block)
                    files[path] = ['file', member.mode & 0o777, member.size, digest.hexdigest()]
            else:
                raise ValueError('unexpected application member type')
    return files, elfs


def base_native(archive, prior, temporary):
    # This signed distroless base is additive. A future replacement or whiteout
    # needs explicit overlay reconciliation before its inventory can be trusted.
    for member in archive:
        path = member_name(member.name)
        if member.isdir():
            continue
        if any(part.startswith('.wh.') for part in PurePosixPath(path).parts):
            raise ValueError('base inventory cannot certify a whiteout')
        if UNSHIPPED.fullmatch(PurePosixPath(path).name):
            raise ValueError('unselected native binary shipped in base')
        if path in prior:
            raise ValueError('base layer replaced an inventoried native file')
        if not member.isfile():
            continue
        with archive.extractfile(member) as handle:
            if handle.read(4) != b'\x7fELF':
                continue
            if member.size > 128 << 20:
                raise ValueError('oversized base executable')
            with temporary.open('wb') as out:
                out.write(b'\x7fELF')
                while block := handle.read(1 << 20):
                    out.write(block)
            prior[path] = {'sha256': hash_file(temporary), 'size': member.size, 'dynamic': dynamic(temporary)}


def validate_needed(*groups):
    provided, needed = set(), set()
    for group in groups:
        for item in group.values():
            for tag, name in item['dynamic']:
                (provided if tag == 'SONAME' else needed).add(name)
    if needed - provided:
        raise ValueError('image does not provide ELF dependencies: ' + ', '.join(sorted(needed - provided)))


def source_libraries(archive, elfs):
    if set(elfs) != DISTRIBUTION | PATCHED | GENERATED:
        raise ValueError('required application native member is missing or unexpected')
    required = DISTRIBUTION
    found, projects, certificates = set(), {}, {}
    for member in archive:
        name = member.name.removeprefix('./').removeprefix('julia-1.13.3/')
        if name == CA_PATH:
            if certificates or not member.isfile() or member.size != 200116:
                raise ValueError('unexpected compiler CA data')
            digest = hashlib.file_digest(archive.extractfile(member), 'sha256').hexdigest()
            if digest != CA_DATA:
                raise ValueError('compiler CA data differs')
            certificates = {'path': CA_PATH, 'size': member.size, 'sha256': digest}
        project = re.fullmatch(r'share/julia/stdlib/v1\.13/(\w+_jll)/Project\.toml', name)
        if project:
            owner = project[1]
            if owner in projects or not member.isfile() or not 0 < member.size <= 65536:
                raise ValueError('invalid compiler wrapper project')
            projects[owner] = tomllib.loads(archive.extractfile(member).read().decode())
        if name in required:
            if name in found or not member.isfile() or member.size != elfs[name]['size']:
                raise ValueError('compiler library type or size differs')
            with archive.extractfile(member) as handle:
                if hashlib.file_digest(handle, 'sha256').hexdigest() != elfs[name]['sha256']:
                    raise ValueError('application library differs from the compiler distribution')
            found.add(name)
    if required != found or not GENERATED <= elfs.keys() or 'bin/julia' not in found:
        raise ValueError('missing compiler or generated native member')
    # Validate the original 23-owner compiler denominator before projecting
    # the independently verified shared libraries actually copied by the bundler.
    registry = jll_advisory.packages({'jll': projects})
    if not certificates or registry['MozillaCACerts_jll'] != {'uuid': '14a3606d-f60d-562e-9121-12d972cd8159',
                                                             'version': '2026.10.20'}:
        raise ValueError('compiler CA bundle metadata differs')
    certificates.update(registry['MozillaCACerts_jll'])
    return ({name: {**registry[name], 'files': ['lib/julia/' + item for item in libs]}
             for name, libs in JLL_LIBRARIES.items()}, certificates)


def verify_owner_inputs(directory, owners, elfs):
    ledger = json.loads((Path(__file__).with_name('owner_inputs.json')).read_bytes())
    if set(ledger) != set(owners) - {'PCRE2_jll'} or set(owners) != set(JLL_LIBRARIES):
        raise ValueError('native publisher inputs differ from the compiled inventory')
    pcre_elf = elfs['lib/julia/' + pcre2.NEW]
    binary = pcre2.publisher(directory / 'PCRE2-10.49')
    if (pcre_elf['size'] != len(binary) or pcre_elf['sha256'] != pcre2.NEW_HASH
            or ['SONAME', 'libpcre2-8.so.0'] not in [list(row) for row in pcre_elf['dynamic']]):
        raise ValueError('patched application PCRE2 runtime differs from the publisher')
    pcre = owners['PCRE2_jll']
    if (pcre['uuid'], pcre['version']) != ('efcefdf7-47ab-520b-bdef-62a2eaa19f15', '10.47.0+0'):
        raise ValueError('unexpected PCRE2 compiler preimage owner')
    pcre.update(version='10.49.1+0', upstreamVersion='10.49', publisherArchiveSHA256=pcre2.INPUTS['binary'],
                publisherFiles={'lib/' + pcre2.NEW: pcre2.NEW_HASH})
    for owner, record in ledger.items():
        catalog = directory / record['sourcePin'].replace('deps/checksums/', 'checksums-')
        if hash_file(catalog) != record['sourceSHA256']:
            raise ValueError('Julia publisher checksum catalog differs: ' + owner)
        hashes = [row.split('/')[2] for row in catalog.read_text().splitlines()
                  if row.startswith(record['checksumName'] + '/sha512/')]
        if len(hashes) != 1 or not re.fullmatch('[0-9a-f]{128}', hashes[0]):
            raise ValueError('native publisher archive checksum missing or ambiguous')
        libname = owner.removesuffix('_jll')
        if not record['checksumName'].startswith(libname + '.v' + owners[owner]['version'] + '.x86_64-linux-gnu'):
            raise ValueError('native publisher platform or build version differs')
        path = directory / (libname + '.tar.gz')
        for kind in ('artifacts', 'recipe'):
            if hash_file(directory / (libname + '.' + kind)) != record[kind]['sha256']:
                raise ValueError('native JLL publisher recipe or metadata differs: ' + owner)
        artifacts = tomllib.loads((directory / (libname + '.artifacts')).read_text())[libname]
        selected = [row for row in artifacts if row.get('arch') == 'x86_64' and row.get('os') == 'linux'
                    and row.get('libc') == 'glibc' and 'sanitize' not in row and row.get('git-tree-sha1') == record['assetTree']
                    and len(row.get('download', [])) == 1 and unquote(row['download'][0]['url']) == unquote(record['url'])]
        if len(selected) != 1 or selected[0]['download'][0]['sha256'] != hash_file(path):
            raise ValueError('native JLL publisher target or archive differs: ' + owner)
        with path.open('rb') as stream:
            if path.stat().st_size != record['archiveBytes'] or hashlib.file_digest(stream, 'sha512').hexdigest() != hashes[0]:
                raise ValueError('independent native publisher archive differs: ' + owner)
        pending = {'lib/' + name: 'lib/julia/' + name for name in JLL_LIBRARIES[owner]}
        found = {}
        with tarfile.open(path, 'r|gz') as archive:
            for item in archive:
                if item.name not in pending:
                    continue
                expected = elfs[pending[item.name]]
                if (item.name in found or not item.isfile() or item.size != expected['size']
                        or hashlib.file_digest(archive.extractfile(item), 'sha256').hexdigest() != expected['sha256']):
                    raise ValueError('independent native publisher file differs: ' + owner)
                found[item.name] = expected['sha256']
        if found.keys() != pending.keys():
            raise ValueError('native publisher file missing: ' + owner)
        owners[owner].update(publisherArchiveSHA512=hashes[0], publisherArchiveSHA256=selected[0]['download'][0]['sha256'],
                             publisherRecipe=record['recipe'], publisherFiles=found)


def verify(image, image_id, base_index, base_platform, bundle, compiler, owner_inputs, rebuilt_gcc=None):
    if hash_file(base_index) != BASE_INDEX or hash_file(base_platform) != BASE_PLATFORM:
        raise ValueError('base image bytes differ from the pinned publisher')
    indexes = json.loads(base_index.read_bytes())['manifests']
    base, = [m for m in indexes if m.get('platform') == {'architecture': 'amd64', 'os': 'linux'}]
    if base['digest'] != 'sha256:' + BASE_PLATFORM or base['size'] != base_platform.stat().st_size:
        raise ValueError('unexpected base platform')
    base_layers = json.loads(base_platform.read_bytes())['layers']
    with tarfile.open(bundle, 'r:') as packed:
        expected, elfs = contents(packed, native=True)
    targets = link_targets(expected)
    if any(targets.get('lib/julia/' + name) != 'lib/julia/' + pcre2.NEW for name in pcre2.ALIAS):
        raise ValueError('application PCRE2 ABI aliases differ')
    if hash_file(compiler) != JULIA:
        raise ValueError('compiler libraries must come from the pinned Julia distribution')
    published = elfs
    if rebuilt_gcc is not None:
        published = gcc_runtime.publisher_view(elfs, rebuilt_gcc, owner_inputs / 'CompilerSupportLibraries.tar.gz')
    with tarfile.open(compiler, 'r|gz') as packed:
        owners, certificates = source_libraries(packed, published)
    if expected.get(CA_PATH, [])[2:] != [certificates['size'], certificates['sha256']]:
        raise ValueError('packaged CA bundle differs from the original Julia distribution')
    ca = owner_inputs / 'cacerts.tar.gz'
    if ca.stat().st_size != 121644 or hash_file(ca) != CA_ARCHIVE:
        raise ValueError('independent Mozilla CA wrapper archive differs')
    with tarfile.open(ca, 'r:gz') as packed:
        member, = [m for m in packed if m.name == 'share/cacert.pem']
        if (not member.isfile() or member.size != certificates['size']
                or hashlib.file_digest(packed.extractfile(member), 'sha256').hexdigest() != CA_DATA):
            raise ValueError('packaged CA data differs from the Mozilla CA wrapper publisher')
    certificates.update(publisherArchiveSHA256=CA_ARCHIVE, publisherVersion='2026.10.20+0')
    verify_owner_inputs(owner_inputs, owners, published)
    if rebuilt_gcc is not None:
        owners['CompilerSupportLibraries_jll']['localRebuild'] = gcc_runtime.rebuild_record()
    with tarfile.open(image, 'r:') as archive:
        members = archive.getmembers()
        if len(members) > 256 or len({m.name for m in members}) != len(members):
            raise ValueError('ambiguous OCI export')
        def blob(desc):
            oid = desc['digest']
            if not re.fullmatch('sha256:[0-9a-f]{64}', oid):
                raise ValueError('invalid OCI digest')
            item = archive.getmember('blobs/sha256/' + oid[7:])
            if not item.isfile() or item.size != desc['size']:
                raise ValueError('OCI descriptor size or type differs')
            with archive.extractfile(item) as f:
                if hashlib.file_digest(f, 'sha256').hexdigest() != oid[7:]:
                    raise ValueError('OCI blob content differs')
            return item
        def document(desc):
            if not 0 < desc['size'] < 1 << 20:
                raise ValueError('unbounded OCI metadata')
            return json.load(archive.extractfile(blob(desc)))
        root, = json.load(archive.extractfile('index.json'))['manifests']
        if root['digest'] != image_id:
            raise ValueError('saved image differs from the selected Docker descriptor')
        idx = document(root)
        platform, = [p for p in idx['manifests'] if p.get('platform') == {'architecture': 'amd64', 'os': 'linux'}]
        manifest = document(platform)
        cfg = document(manifest['config'])
        if (cfg['os'], cfg['architecture'], cfg['config']['Entrypoint']) != ('linux', 'amd64', ['/opt/kanz/bin/curve-worker']):
            raise ValueError('application image configuration differs')
        source = cfg['config']['Labels']['org.opencontainers.image.revision']
        if not re.fullmatch('[0-9a-f]{40}', source):
            raise ValueError('source revision missing from image config')
        layers = manifest['layers']
        # The Dockerfile WORKDIR emits the canonical empty tar after ADD.
        # Accept it only when the referenced bytes actually contain no members.
        if layers[-1]['digest'] == EMPTY_LAYER:
            with archive.extractfile(blob(layers[-1])) as stream, tarfile.open(fileobj=stream, mode='r|gz') as packed:
                if next(iter(packed), None) is not None:
                    raise ValueError('unexpected content after the application layer')
            layers = layers[:-1]
        if layers[:-1] != base_layers:
            raise ValueError('image does not preserve the full publisher base')
        os_elfs = {}
        with tempfile.TemporaryDirectory() as tmp:
            for layer in base_layers:
                with archive.extractfile(blob(layer)) as stream, tarfile.open(fileobj=stream, mode='r|*') as packed:
                    base_native(packed, os_elfs, Path(tmp) / 'elf')
        validate_needed(elfs, os_elfs)
        app_layer = layers[-1]
        with archive.extractfile(blob(app_layer)) as stream, tarfile.open(fileobj=stream, mode='r|*') as packed:
            actual, _ = contents(packed, 'opt/kanz/')
        if actual != expected:
            raise ValueError('final application filesystem differs from the actual compiler output')
    return {'image': image_id, 'source': source, 'manifest': platform['digest'], 'config': manifest['config']['digest'],
            'baseLayers': len(base_layers), 'applicationLayer': app_layer['digest'], 'filesAndLinks': len(actual),
            'native': elfs, 'baseNative': os_elfs, 'wrapperMetadata': owners, 'dataOwners': {'MozillaCACerts_jll': certificates}, 'releaseQualified': False}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    for key in ('image', 'base_index', 'base_platform', 'bundle', 'compiler', 'owner_inputs', 'output'):
        parser.add_argument(key, type=Path)
    parser.add_argument('image_id')
    parser.add_argument('--rebuilt-gcc', type=Path)
    a = parser.parse_args()
    receipt = verify(a.image, a.image_id, a.base_index, a.base_platform, a.bundle, a.compiler, a.owner_inputs, a.rebuilt_gcc)
    a.output.write_text(json.dumps(receipt, indent=2) + '\n')
    print('Verified native application entries:', len(receipt['native']))
