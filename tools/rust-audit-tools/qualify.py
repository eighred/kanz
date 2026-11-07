"""Build/qualify a source-free tool payload; consumers remain unchanged."""
import argparse
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import stat
import sys
import tarfile
import tempfile
import tomllib
import zlib

REPOSITORY = "ghcr.io/eighred/base/rust-audit-tools"
SIGNER = "https://github.com/eighred/kanz/.github/workflows/base-image-mirror.yml@refs/heads/main"
ISSUER = "https://token.actions.githubusercontent.com"
TOOLS = {"cargo-audit": "0.22.2", "rust-audit-info": "0.5.4"}
MAX_PAYLOAD = 256 * 1024 * 1024


def run(args, **kwargs):
    return subprocess.run(args, check=True, timeout=1200, **kwargs)


def output(args):
    return run(args, stdout=subprocess.PIPE, text=True).stdout.strip()


def read_bounded(path, limit):
    if sys.platform != "linux" or not hasattr(os, "O_NOFOLLOW") or not hasattr(os, "O_NONBLOCK"):
        raise ValueError("Linux no-follow/nonblocking file reader required")
    with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK), "rb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_size > limit:
            raise ValueError("invalid bounded artifact")
        data = stream.read(limit + 1)
    if len(data) != info.st_size or len(data) > limit:
        raise ValueError("artifact changed while reading")
    return data


def strict(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate metadata key")
        result[key] = value
    return result


def sha(path):
    return hashlib.sha256(read_bounded(path, MAX_PAYLOAD)).hexdigest()


def source():
    value = output(["git", "rev-parse", "HEAD"])
    if not re.fullmatch("[0-9a-f]{40}", value) or value != os.environ["GITHUB_SHA"]:
        raise ValueError("source identity mismatch")
    return value


def manifest(commit):
    return {"version": 1, "source_sha": commit, "platform": "linux/amd64", "rust": "1.90.0", **TOOLS}


def tar_files(raw, maximum):
    if len(raw) > MAX_PAYLOAD:
        raise ValueError("oversized archive")
    entries = {}
    total = 0
    with tarfile.open(fileobj=io.BytesIO(raw), mode="r:") as archive:
        for item in archive:
            name = item.name.removeprefix("./")
            path = PurePosixPath(name)
            if len(name) > 512 or path.is_absolute() or ".." in path.parts or any(p.startswith(".wh.") for p in path.parts) or item.issym() or item.islnk():
                raise ValueError("unsafe archive entry")
            if item.isdir():
                continue
            total += item.size
            if not item.isfile() or name in entries or len(entries) >= maximum or total > MAX_PAYLOAD:
                raise ValueError("invalid archive inventory")
            entries[name] = archive.extractfile(item).read()
    return entries


def descriptor(desc, entries, media):
    if desc.get("mediaType") not in media:
        raise ValueError("unsupported archive media type")
    digest = desc["digest"]
    if not re.fullmatch("sha256:[0-9a-f]{64}", digest):
        raise ValueError("invalid archive descriptor")
    path = "blobs/sha256/" + digest[7:]
    raw = entries[path]
    if type(desc["size"]) is not int or len(raw) != desc["size"] or "sha256:" + hashlib.sha256(raw).hexdigest() != digest:
        raise ValueError("archive descriptor bytes differ")
    return path


def metadata(raw):
    if len(raw) > 1024 * 1024:
        raise ValueError("oversized image metadata")
    return json.loads(raw, object_pairs_hook=strict)


def saved_layer(raw, expected_image, commit):
    entries = tar_files(raw, 32)
    manifests = metadata(entries["manifest.json"])
    if not isinstance(manifests, list) or len(manifests) != 1:
        raise ValueError("single saved image required")
    m = manifests[0]
    if len(m["Layers"]) != 1:
        raise ValueError("one final scratch COPY layer required")
    config_name, layer_name = m["Config"], m["Layers"][0]
    config_bytes, layer = entries[config_name], entries[layer_name]
    if "sha256:" + hashlib.sha256(config_bytes).hexdigest() != expected_image:
        raise ValueError("saved configuration identity differs")
    config = metadata(config_bytes)
    if (config["os"], config["architecture"]) != ("linux", "amd64") or config["config"]["Labels"].get("org.opencontainers.image.revision") != commit:
        raise ValueError("image platform/source mismatch")
    diff = "sha256:" + hashlib.sha256(layer).hexdigest()
    if config["rootfs"] != {"type": "layers", "diff_ids": [diff]}:
        raise ValueError("uncompressed final layer identity differs")
    history = [h for h in config["history"] if not h.get("empty_layer", False)]
    if len(history) != 1 or not history[0]["created_by"].startswith("COPY /payload/ /"):
        raise ValueError("unexpected nonempty build history")
    allowed = {"manifest.json", config_name, layer_name}
    # Current Docker saves may include the equivalent OCI graph. Verify every
    # descriptor and reject extra blobs/images rather than ignoring archive data.
    if "index.json" in entries:
        index = metadata(entries["index.json"])
        if index.get("schemaVersion") != 2 or len(index["manifests"]) != 1 or metadata(entries["oci-layout"]) != {"imageLayoutVersion": "1.0.0"}:
            raise ValueError("invalid saved OCI index")
        name = descriptor(index["manifests"][0], entries, {"application/vnd.oci.image.manifest.v1+json", "application/vnd.docker.distribution.manifest.v2+json"})
        graph = metadata(entries[name])
        if graph.get("schemaVersion") != 2 or len(graph["layers"]) != 1 or descriptor(graph["config"], entries, {"application/vnd.oci.image.config.v1+json", "application/vnd.docker.container.image.v1+json"}) != config_name or descriptor(graph["layers"][0], entries, {"application/vnd.oci.image.layer.v1.tar", "application/vnd.docker.image.rootfs.diff.tar"}) != layer_name:
            raise ValueError("saved OCI and Docker graphs differ")
        allowed.update(("index.json", "oci-layout", name))
    if "repositories" in entries:
        if metadata(entries["repositories"]) != {}:
            raise ValueError("unexpected saved tag inventory")
        allowed.add("repositories")
    if set(entries) != allowed:
        raise ValueError("unreachable saved image data")
    return layer


def extract_saved(raw, expected_image, target, commit):
    layer = saved_layer(raw, expected_image, commit)
    files = {}
    with tarfile.open(fileobj=io.BytesIO(layer), mode="r:") as tar:
        total = 0
        for item in tar:
            name = item.name.removeprefix("./")
            path = PurePosixPath(name)
            if len(name) > 512 or len(files) >= 10000 or path.is_absolute() or ".." in path.parts or any(p.startswith(".wh.") for p in path.parts) or item.issym() or item.islnk():
                raise ValueError("unsafe image entry")
            if item.isdir():
                if name not in ("", ".", "tools", "notices") and not name.startswith("notices/"):
                    raise ValueError("unexpected image directory")
                continue
            notice = name.startswith("notices/") and (path.name == "package.toml" or any(word in path.name.lower() for word in ("license", "copying", "notice", "copyright")) or name.startswith("notices/rust-1.90.0/licenses/"))
            allowed = name in ("manifest.json", "tools/cargo-audit", "tools/rust-audit-info") or notice
            total += item.size
            if not allowed or not item.isfile() or name in files or total > MAX_PAYLOAD:
                raise ValueError("unexpected image file")
            data = tar.extractfile(item).read()
            files[name] = hashlib.sha256(data).hexdigest()
            if name == "manifest.json":
                decoded = metadata(data)
                if type(decoded.get("version")) is not int or decoded != manifest(commit):
                    raise ValueError("tool manifest mismatch")
            elif name.startswith("tools/"):
                dest = target / path.name
                dest.write_bytes(data)
                dest.chmod(0o755)
            elif path.name == "package.toml":
                if len(data) > 1024 * 1024:
                    raise ValueError("oversized package metadata")
                package = tomllib.loads(data.decode())["package"]
                if package["name"] in TOOLS and package.get("version") != TOOLS[package["name"]]:
                    raise ValueError("public tool package version mismatch")
                if not package.get("license") and not package.get("license-file"):
                    raise ValueError("missing public package licensing metadata")
    for name in ("manifest.json", "tools/cargo-audit", "tools/rust-audit-info"):
        if name not in files:
            raise ValueError("missing tool payload")
    for name, version in TOOLS.items():
        prefix = f"notices/{name}-{version}/"
        if prefix + "package.toml" not in files or not any(p.startswith(prefix) and "license" in p.lower() for p in files):
            raise ValueError("missing direct-tool license notices")
    for name in ("COPYRIGHT.html", "COPYRIGHT-library.html", "licenses/Apache-2.0.txt", "licenses/MIT.txt"):
        if "notices/rust-1.90.0/" + name not in files:
            raise ValueError("missing Rust distribution notices")
    return expected_image, files


def exercise(directory):
    audit = str(directory / "cargo-audit")
    if output([audit, "--version"]) != "cargo-audit 0.22.2":
        raise ValueError("cargo-audit ABI/version mismatch")
    # Public synthetic ELF metadata validates extraction and both byte limits.
    metadata = b'{"packages":[{"name":"fixture","version":"1.0.0","source":"local","root":true}]}'
    section = directory / "metadata.zlib"
    section.write_bytes(zlib.compress(metadata))
    sample = directory / "sample"
    run(["objcopy", "--add-section", ".dep-v0=" + str(section), audit, str(sample)])
    info = str(directory / "rust-audit-info")
    actual = run([info, str(sample), str(MAX_PAYLOAD), "1048576"], stdout=subprocess.PIPE).stdout
    if actual != metadata:
        raise ValueError("audit-info extraction mismatch")
    for limits in (("1", "1048576"), (str(MAX_PAYLOAD), "1")):
        refused = subprocess.run([info, str(sample), *limits], stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30)
        if refused.returncode == 0:
            raise ValueError("audit-info bound was ignored")
    env = {k: os.environ[k] for k in ("PATH", "SSL_CERT_FILE", "SSL_CERT_DIR") if k in os.environ}
    env.update(HOME=str(directory), CARGO_HOME=str(directory / "cargo"))
    db = directory / "advisory-db"
    commits = []
    for vulnerable in (False, True):
        lock = directory / ("vulnerable.lock" if vulnerable else "clean.lock")
        lock.write_text('version = 3\n' + ('[[package]]\nname = "time"\nversion = "0.1.12"\nsource = "registry+https://github.com/rust-lang/crates.io-index"\n' if vulnerable else ''), encoding="utf8")
        result = subprocess.run([audit, "audit", "--db", str(db), "--deny", "warnings", "--json", "--file", str(lock)], cwd=directory, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=300)
        report = json.loads(result.stdout)
        found = report["vulnerabilities"]["list"]
        if vulnerable:
            if result.returncode == 0 or not any(p["package"]["name"] == "time" and p["package"]["version"] == "0.1.12" for p in found):
                raise ValueError("known-vulnerable audit did not refuse")
        elif result.returncode != 0 or found:
            raise ValueError("clean audit control failed")
        commit = output(["git", "-C", str(db), "rev-parse", "HEAD"])
        if not re.fullmatch("[0-9a-f]{40}", commit):
            raise ValueError("invalid fresh advisory identity")
        commits.append(commit)
    return commits


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=("build", "publish"))
    parser.add_argument("directory", type=Path)
    args = parser.parse_args()
    commit = source()
    directory = args.directory.resolve()
    directory.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory() as temporary:
        temp = Path(temporary)
        if args.mode == "build":
            recipe = Path(__file__).with_name("Dockerfile").read_bytes()
            iid = temp / "image.id"
            # A text Dockerfile on stdin supplies NO filesystem build context.
            run(["docker", "build", "--platform", "linux/amd64", "--build-arg", "SOURCE_SHA=" + commit, "--iidfile", str(iid), "-"], input=recipe)
            image = iid.read_text().strip()
            if not re.fullmatch("sha256:[0-9a-f]{64}", image):
                raise ValueError("invalid image identity")
            run(["docker", "save", "--output", str(directory / "image.tar"), image])
            image_bytes = read_bounded(directory / "image.tar", MAX_PAYLOAD)
            image, files = extract_saved(image_bytes, image, temp, commit)
            advisories = exercise(temp)
            proof = {"run": os.environ["GITHUB_RUN_ID"], "attempt": os.environ["GITHUB_RUN_ATTEMPT"], "source": commit, "tree": output(["git", "rev-parse", "HEAD^{tree}"]), "image": image, "manifest": manifest(commit), "files": files, "advisory_commits": advisories, "image_tar_sha256": sha(directory / "image.tar")}
            (directory / "proof.json").write_text(json.dumps(proof, sort_keys=True) + "\n", encoding="utf8")
            print("source-free tool payload, ABI, extraction limits and fresh audit controls PASS")
        else:
            if os.environ.get("GITHUB_REPOSITORY") != "eighred/kanz" or os.environ.get("GITHUB_REF") != "refs/heads/main" or os.environ.get("GITHUB_EVENT_NAME") not in ("push", "workflow_dispatch", "schedule"):
                raise ValueError("publication requires canonical main")
            proof = json.loads(read_bounded(directory / "proof.json", 4 * 1024 * 1024), object_pairs_hook=strict)
            if set(proof) != {"run", "attempt", "source", "tree", "image", "manifest", "files", "advisory_commits", "image_tar_sha256"} or len(proof["advisory_commits"]) != 2 or not all(re.fullmatch("[0-9a-f]{40}", c) for c in proof["advisory_commits"]):
                raise ValueError("invalid qualification receipt")
            if (proof["run"], proof["attempt"]) != (os.environ["GITHUB_RUN_ID"], os.environ["GITHUB_RUN_ATTEMPT"]):
                raise ValueError("qualification run identity mismatch")
            image_bytes = read_bounded(directory / "image.tar", MAX_PAYLOAD)
            if proof["tree"] != output(["git", "rev-parse", "HEAD^{tree}"]) or proof["source"] != commit or proof["manifest"] != manifest(commit) or proof["image_tar_sha256"] != hashlib.sha256(image_bytes).hexdigest():
                raise ValueError("qualified image binding mismatch")
            image, files = extract_saved(image_bytes, proof["image"], temp, commit)
            run(["docker", "load"], input=image_bytes)
            if files != proof["files"]:
                raise ValueError("qualified payload changed")
            target = REPOSITORY + ":" + commit
            existing = subprocess.run(["crane", "digest", target], stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=60)
            if existing.returncode == 0:
                prior = json.loads(output(["crane", "manifest", target]))
                if prior.get("config", {}).get("digest") != image:
                    raise ValueError("existing source tag differs")
            elif not any(code in existing.stderr for code in (b"MANIFEST_UNKNOWN", b"NAME_UNKNOWN")):
                raise ValueError("unknown registry lookup failure")
            run(["docker", "tag", image, target])
            run(["docker", "push", target])
            digest = output(["crane", "digest", target])
            if not re.fullmatch("sha256:[0-9a-f]{64}", digest):
                raise ValueError("invalid published digest")
            pinned = REPOSITORY + "@" + digest
            registry = json.loads(output(["crane", "manifest", pinned]))
            if registry.get("config", {}).get("digest") != image:
                raise ValueError("registry readback differs")
            run(["docker", "pull", "--platform", "linux/amd64", pinned])
            check = temp / "readback"
            check.mkdir()
            readback = check / "image.tar"
            run(["docker", "save", "--output", str(readback), image])
            if extract_saved(read_bounded(readback, MAX_PAYLOAD), image, check, commit) != (image, files):
                raise ValueError("registry payload readback differs")
            run(["cosign", "sign", "--yes", "-a", "source_sha=" + commit, pinned])
            verified = json.loads(output(["cosign", "verify", "--certificate-oidc-issuer", ISSUER, "--certificate-identity", SIGNER, "-a", "source_sha=" + commit, pinned]))
            if not verified:
                raise ValueError("signature readback missing")
            (directory / "published.json").write_text(json.dumps({"run": proof["run"], "attempt": proof["attempt"], "source": commit, "image": pinned, "config": image, "files": files, "signer": SIGNER, "issuer": ISSUER, "source_annotation": commit}, sort_keys=True) + "\n", encoding="utf8")
            print("published tool payload registry readback PASS: " + pinned)


if __name__ == "__main__":
    main()
