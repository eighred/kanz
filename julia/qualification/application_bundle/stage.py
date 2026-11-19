"""Stage the exact registered build tools and General registry into a fresh offline build directory."""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import tarfile
import tomllib


HERE = Path(__file__).resolve().parent
GENERAL = {
    'commit': '0f0f298446cd34ff4f4228983bd8eeabe1f0a83e',
    'tree': '917191972a124bb953771781e2fcbc900912a16e',
    'bytes': 12283235,
    'sha256': '428014ff420d92125fc604fd857728d913dcd921271f54494b3f471bd8f6735c',
}


def verify_archive(path, pin):
    with path.open('rb') as stream:
        if path.stat().st_size != pin['bytes'] or hashlib.file_digest(stream, 'sha256').hexdigest() != pin['sha256']:
            raise ValueError('build input archive identity differs: ' + path.name)


def verify_tree(directory, expected):
    subprocess.run(['git', 'init', '-q'], cwd=directory, check=True)
    try:
        subprocess.run(['git', 'add', '-f', '.'], cwd=directory, check=True)
        tree = subprocess.check_output(['git', 'write-tree'], cwd=directory, text=True).strip()
        if tree != expected:
            raise ValueError('build input Git tree differs: ' + directory.name)
    finally:
        shutil.rmtree(directory / '.git')


def extract_sources(path, directory):
    # These registered trees contain only regular files and directories.
    with tarfile.open(path) as archive:
        for member in archive:
            parts = member.name.split('/')
            if (any(part in ('', '.', '..', '.git') for part in parts)
                    or '\\' in member.name or not (member.isdir() or member.isfile())):
                raise ValueError('unexpected source archive member')
            dest = directory.joinpath(*parts)
            if member.isdir():
                dest.mkdir(exist_ok=True, parents=True)
                continue
            dest.parent.mkdir(exist_ok=True, parents=True)
            with dest.open('xb') as stream:
                shutil.copyfileobj(archive.extractfile(member), stream)
            dest.chmod(0o755 if member.mode & 0o111 else 0o644)


def main(inputs, dest):
    packages = json.loads((HERE / 'tool_inputs.json').read_bytes())
    archives = [(inputs / 'General.tar.gz', GENERAL), *((inputs / (p['name'] + '.tar.gz'), p) for p in packages)]
    for archive, pin in archives:
        verify_archive(archive, pin)
    dest.mkdir()
    registry_parent = dest / 'depot' / 'registries'
    registry_parent.mkdir(parents=True)
    extract_sources(archives[0][0], registry_parent)
    registry = registry_parent / 'General'
    (registry_parent / ('General-' + GENERAL['commit'])).rename(registry)
    verify_tree(registry, GENERAL['tree'])
    for (archive_path, pin) in archives[1:]:
        name = pin['name']
        package = registry / name[0] / name
        versions = tomllib.loads((package / 'Versions.toml').read_text())
        if (tomllib.loads((package / 'Package.toml').read_text())['uuid'] != pin['uuid']
                or versions[pin['version']]['git-tree-sha1'] != pin['gitTree']):
            raise ValueError('registered compiler package identity differs: ' + name)
        target = dest / 'tools' / name
        target.mkdir(parents=True)
        extract_sources(archive_path, target)
        verify_tree(target, pin['gitTree'])


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('inputs', type=Path)
    parser.add_argument('new_stage', type=Path)
    args = parser.parse_args()
    main(args.inputs, args.new_stage)
