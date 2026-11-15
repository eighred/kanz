"""Exact platform-manifest/config identities, independent of Docker store format.

Only public descriptor projections are retained. Registry tokens and raw image
configurations stay in memory and never enter receipts, exceptions or artifacts.
"""
import hashlib
import json
from pathlib import Path
import re
import urllib.parse
import urllib.request

HERE = Path(__file__).resolve().parent
DIGEST = re.compile(r"sha256:[0-9a-f]{64}\Z")
LIMIT = 1 << 20
MANIFEST_TYPES = {"application/vnd.docker.distribution.manifest.v2+json", "application/vnd.oci.image.manifest.v1+json"}
CONFIG_TYPES = {"application/vnd.docker.container.image.v1+json", "application/vnd.oci.image.config.v1+json"}


def selections():
    value = json.loads((HERE / "image-descriptors.json").read_text())
    images = json.loads((HERE / "images.json").read_text())
    if value.get("version") != 1 or value.get("platform") != "linux/amd64" or set(value.get("images", {})) != set(images["images"]):
        raise ValueError("closed public descriptor inventory differs")
    for role, item in value["images"].items():
        if item["reference"] != images["images"][role] or reference_identity(item["reference"]) != (item["executionRepository"], item["manifestDigest"]) or reference_identity(item["publisherReference"]) != ("docker.io/" + item["repository"], item["manifestDigest"]) or item["os"] != "linux" or item["architecture"] != "amd64" or item["manifestMediaType"] not in MANIFEST_TYPES or item["configMediaType"] not in CONFIG_TYPES or not DIGEST.fullmatch(item["configDigest"]) or not DIGEST.fullmatch(item["manifestDigest"]):
            raise ValueError("selected platform descriptor differs")
    return value["images"]


def lock_digest():
    return hashlib.sha256((HERE / "image-descriptors.json").read_bytes()).hexdigest()


def canonical_repository(value):
    # Only these explicit Docker Hub aliases are equivalent. No suffix-based
    # registry/repository match can authorize an unrelated image.
    parts = value.split("/")
    if parts[0] == "ghcr.io":
        if len(parts) != 4 or parts[1:3] != ["eighred", "base"] or not re.fullmatch(r"[a-z0-9]+(?:[._-][a-z0-9]+)*", parts[3]):
            raise ValueError("unapproved private execution repository")
        return value
    if parts[0] in {"docker.io", "index.docker.io", "registry-1.docker.io"}:
        parts = parts[1:]
    elif "." in parts[0] or ":" in parts[0] or parts[0] == "localhost":
        raise ValueError("unselected registry identity")
    if len(parts) == 1:
        parts.insert(0, "library")
    if any(not re.fullmatch(r"[a-z0-9]+(?:[._-][a-z0-9]+)*", part) for part in parts):
        raise ValueError("noncanonical repository identity")
    return "docker.io/" + "/".join(parts)


def reference_identity(value):
    repository, digest = value.rsplit("@", 1)
    if not DIGEST.fullmatch(digest):
        raise ValueError("unversioned execution/publisher descriptor")
    if ":" in repository.rsplit("/", 1)[-1]:
        repository = repository.rsplit(":", 1)[0]
    return canonical_repository(repository), digest


def manifest_bound(repo_digests, selected):
    if not isinstance(repo_digests, list):
        return False
    wanted = selected["executionRepository"]
    for value in repo_digests:
        try:
            repository, digest = value.rsplit("@", 1)
            if canonical_repository(repository) == wanted and digest == selected["manifestDigest"]:
                return True
        except (ValueError, AttributeError):
            continue
    return False


def verify_bytes(body, digest, size):
    if type(size) is not int or not 0 < size <= LIMIT or len(body) != size or "sha256:" + hashlib.sha256(body).hexdigest() != digest:
        raise ValueError("public descriptor bytes/hash refused")


def verify_raw(manifest_body, config_body, selected):
    verify_bytes(manifest_body, selected["manifestDigest"], selected["manifestBytes"])
    manifest = json.loads(manifest_body)
    descriptor = manifest.get("config", {})
    if manifest.get("schemaVersion") != 2 or manifest.get("mediaType") != selected["manifestMediaType"] or "manifests" in manifest or descriptor != {"mediaType": selected["configMediaType"], "digest": selected["configDigest"], "size": selected["configBytes"]}:
        # The selected inputs are platform manifests, not indices. An index
        # would require its own explicit platform/descriptor contract.
        raise ValueError("platform manifest/config descriptor link refused")
    verify_bytes(config_body, descriptor["digest"], descriptor["size"])
    config = json.loads(config_body)
    if config.get("os") != selected["os"] or config.get("architecture") != selected["architecture"] or config.get("rootfs") != {"type": selected["rootFSType"], "diff_ids": selected["rootFSDiffIDs"]}:
        raise ValueError("public config platform/ordered rootfs differs")
    return dict(selected, rawManifestVerified=True, rawConfigVerified=True)


def fetch(selected):
    class HTTPSRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, request, response, code, message, headers, newurl):
            if urllib.parse.urlparse(newurl).scheme != "https":
                raise ValueError("non-HTTPS descriptor redirect refused")
            redirected = super().redirect_request(request, response, code, message, headers, newurl)
            if redirected is not None and urllib.parse.urlparse(request.full_url).netloc != urllib.parse.urlparse(newurl).netloc:
                redirected.remove_header("Authorization")
            return redirected
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), HTTPSRedirect())
    def read(url, headers=None):
        try:
            with opener.open(urllib.request.Request(url, headers=headers or {}), timeout=30) as response:
                body = response.read(LIMIT + 1)
            if len(body) > LIMIT:
                raise ValueError("public descriptor exceeds byte bound")
            return body
        except (OSError, ValueError):
            raise ValueError("public descriptor request refused") from None
    try:
        repository = selected["repository"]
        if canonical_repository(repository) != "docker.io/" + repository or reference_identity(selected["publisherReference"]) != ("docker.io/" + repository, selected["manifestDigest"]):
            raise ValueError("public descriptor publisher differs")
        token = json.loads(read("https://auth.docker.io/token?service=registry.docker.io&scope=repository:" + repository + ":pull"))["token"]
        headers = {"Authorization": "Bearer " + token, "Accept": ",".join(sorted(MANIFEST_TYPES))}
        prefix = "https://registry-1.docker.io/v2/" + repository
        manifest = read(prefix + "/manifests/" + selected["manifestDigest"], headers)
        config = read(prefix + "/blobs/" + selected["configDigest"], {"Authorization": "Bearer " + token})
        return verify_raw(manifest, config, selected)
    except (ValueError, KeyError, TypeError, UnicodeError):
        raise ValueError("public manifest/config identity refused") from None


def engine_kind(image_id, selected):
    if image_id == selected["manifestDigest"]:
        return "platform-manifest"
    if image_id == selected["configDigest"]:
        return "config"
    raise ValueError("unknown Docker engine image identity")


def verify_engine(info, selected):
    kind = engine_kind(info.get("Id"), selected)
    if info.get("Os") != selected["os"] or info.get("Architecture") != selected["architecture"] or info.get("RootFS") != {"Type": selected["rootFSType"], "Layers": selected["rootFSDiffIDs"]} or not manifest_bound(info.get("RepoDigests"), selected):
        raise ValueError("Docker platform/rootfs/canonical manifest binding differs")
    return {"engineID": info["Id"], "engineIDKind": kind, "manifestDigest": selected["manifestDigest"],
            "configDigest": selected["configDigest"], "repository": selected["repository"],
            "executionRepository": selected["executionRepository"],
            "os": selected["os"], "architecture": selected["architecture"],
            "rootFSType": selected["rootFSType"], "rootFSDiffIDs": selected["rootFSDiffIDs"], "repoManifestBound": True}


def verify_projected(identity, selected):
    expected = {"engineID": identity.get("engineID"), "engineIDKind": engine_kind(identity.get("engineID"), selected),
                **{key: selected[key] for key in ("manifestDigest", "configDigest", "repository", "executionRepository", "os", "architecture", "rootFSType", "rootFSDiffIDs")}, "repoManifestBound": True}
    if identity != expected:
        raise ValueError("projected published image identity differs")


def derived_engine(info):
    if not DIGEST.fullmatch(info.get("Id", "")) or info.get("Os") != "linux" or info.get("Architecture") != "amd64" or info.get("RepoDigests") or info.get("RepoTags") or info.get("RootFS", {}).get("Type") != "layers" or not info["RootFS"].get("Layers") or any(not DIGEST.fullmatch(value) for value in info["RootFS"]["Layers"]):
        raise ValueError("owned derived Docker image identity refused")
    return {"engineID": info["Id"], "engineIDKind": "owned-build", "os": "linux", "architecture": "amd64",
            "rootFSType": "layers", "rootFSDiffIDs": info["RootFS"]["Layers"]}


def verify_scan(metadata, identity):
    config = metadata.get("ImageConfig", {})
    if metadata.get("ImageID") != identity["engineID"] or metadata.get("DiffIDs") != identity["rootFSDiffIDs"] or config.get("os") != identity["os"] or config.get("architecture") != identity["architecture"] or config.get("rootfs") != {"type": identity["rootFSType"], "diff_ids": identity["rootFSDiffIDs"]}:
        raise ValueError("Docker-only scanner image/platform/rootfs differs")
    if identity["engineIDKind"] == "owned-build":
        if metadata.get("RepoDigests") or metadata.get("RepoTags"):
            raise ValueError("owned scanner image gained a registry identity")
    elif engine_kind(identity["engineID"], identity) != identity["engineIDKind"] or not manifest_bound(metadata.get("RepoDigests"), identity):
        raise ValueError("Docker-only scanner canonical manifest binding differs")
