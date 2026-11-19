"""Acquire the reviewed public native inputs before launching the credential-free offline build."""
import argparse
import hashlib
import json
from pathlib import Path
from urllib.request import Request, urlopen

import compiler_owners
import gcc_advisories
import pcre2
import linked_advisory
import pcre2_advisory
import pcre2_cve
from readback import CA_ARCHIVE, CA_URL


JULIA = 'https://raw.githubusercontent.com/JuliaLang/julia/67ac47b9cd855e02134d41d5428ac9f2a73f2694/'
YGGDRASIL = 'https://raw.githubusercontent.com/JuliaPackaging/Yggdrasil/32a1bc27657f9e4d874c69fa88b071bbf90d102f/P/PCRE2/'
PCRE_RELEASE = 'https://github.com/PCRE2Project/pcre2/releases/download/pcre2-10.49/pcre2-10.49.tar.bz2'
PCRE_INPUTS = {
    'wrapperArtifacts': 'https://raw.githubusercontent.com/JuliaBinaryWrappers/PCRE2_jll.jl/cbb3bbd7d871d0d78cf0f2132466836f2a31f8dd/Artifacts.toml',
    'recipe': YGGDRASIL + 'PCRE2/build_tarballs.jl', 'common': YGGDRASIL + 'common.jl',
    'source': PCRE_RELEASE, 'signature': PCRE_RELEASE + '.sig', 'binary': pcre2.URL.replace('+', '%2B'),
    'signer': 'https://keys.openpgp.org/vks/v1/by-fingerprint/A95536204A3BB489715231282A98E77EB6F24CA8',
}


def fetch(url, target, limit, algorithm, checksum):
    digest = hashlib.new(algorithm)
    size = 0
    with urlopen(Request(url, headers={'User-Agent': 'kanz-calibration-qualification'}), timeout=60) as src:
        if src.status != 200 or not src.geturl().startswith('https://'):
            raise ValueError('unexpected publisher transport')
        with target.open('xb') as output:
            while block := src.read(1 << 20):
                size += len(block)
                if size > limit:
                    raise ValueError('oversized publisher input')
                digest.update(block)
                output.write(block)
    if not size or digest.hexdigest() != checksum:
        raise ValueError('publisher input checksum differs: ' + target.name)


def acquire(directory):
    directory.mkdir()
    for owner, pin in json.loads(Path(__file__).with_name('owner_inputs.json').read_bytes()).items():
        group = owner.removesuffix('_jll')
        for kind in ('artifacts', 'recipe'):
            fetch(pin[kind]['url'], directory / (group + '.' + kind), 3 << 20, 'sha256', pin[kind]['sha256'])
        catalog = directory / pin['sourcePin'].replace('deps/checksums/', 'checksums-')
        fetch(JULIA + pin['sourcePin'], catalog, 2 << 20, 'sha256', pin['sourceSHA256'])
        checksums = [row.split('/')[2] for row in catalog.read_text().splitlines()
                     if row.startswith(pin['checksumName'] + '/sha512/')]
        if len(checksums) != 1:
            raise ValueError('ambiguous publisher checksum')
        target = directory / (group + '.tar.gz')
        fetch(pin['url'], target, pin['archiveBytes'], 'sha512', checksums[0])
        if target.stat().st_size != pin['archiveBytes']:
            raise ValueError('publisher asset size differs')
    fetch(CA_URL, directory / 'cacerts.tar.gz', 121644, 'sha256', CA_ARCHIVE)
    pcre = directory / 'PCRE2-10.49'
    pcre.mkdir()
    for name, url in PCRE_INPUTS.items():
        fetch(url, pcre / name, 4 << 20, 'sha256', pcre2.INPUTS[name])
    pcre2.publisher(pcre)
    pcre2_advisory.collect(pcre / 'publisher.json')
    pcre2_cve.collect(pcre / 'cve')
    linked_advisory.collect(directory / 'linked')
    parents = directory / 'compilers'
    parents.mkdir()
    for name, (url, size, sha) in (compiler_owners.INPUTS | gcc_advisories.SPECS).items():
        fetch(url, parents / name, size, 'sha256', sha)
    gcc_advisories.collect_headers(parents)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('new_directory', type=Path)
    acquire(parser.parse_args().new_directory)
