"""Pinned fixture-only package layer; never updates or removes the upstream base."""
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import time
import urllib.request

HERE = Path(__file__).resolve().parent / "ingestion_patch"
BEHAVIOR = ("User", "Entrypoint", "Cmd", "WorkingDir", "Env", "ExposedPorts", "Healthcheck", "StopSignal", "Shell", "OnBuild")
ADVISORY_EXIT = 42
BUILD = {"mode": "docker-legacy", "DOCKER_BUILDKIT": "0", "network": "none", "pull": False,
         "memoryBytes": 1 << 30, "swapBytes": 1 << 30, "cpuQuota": 200000, "cpuPeriod": 100000,
         "timeoutSeconds": 300, "forceRemoveIntermediateContainers": True, "tag": None}


def lock():
    return json.loads((HERE / "package-lock.json").read_text())


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True).encode()).hexdigest()


def validate_package(body, selection):
    if len(body) != selection["bytes"] or hashlib.sha256(body).hexdigest() != selection["sha256"]:
        raise ValueError("fixed package bytes differ from reviewed publisher input")


def download(selection):
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, *_args, **_kwargs):
            return None
    if selection["url"] != "https://security.ubuntu.com/ubuntu/pool/main/l/linux/linux-libc-dev_6.8.0-149.149_amd64.deb":
        raise ValueError("fixed package publisher differs")
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    try:
        with opener.open(selection["url"], timeout=30) as response:
            body = response.read(selection["bytes"] + 1)
    except OSError:
        raise ValueError("fixed package download refused") from None
    validate_package(body, selection)
    return body


def owned_spec(owner, owner_path):
    selection = lock()
    labels = {"kanz.catalog.owner": owner["nonce"], "kanz.catalog.source": owner["sourceSHA"], "kanz.catalog.run": owner["runID"], "kanz.catalog.attempt": owner["runAttempt"], "kanz.catalog.role": "ingestion-patched", "kanz.catalog.package": selection["sha256"]}
    return {"context": str(owner_path.parent / (owner["network"] + "-image-context")),
            "startedPath": str(owner_path.parent / (owner["network"] + "-build-started.json")),
            "completedPath": str(owner_path.parent / (owner["network"] + "-build-completed.json")),
            "baseImageID": selection["baseImageID"],
            "baseReference": selection["base"], "packageSHA256": selection["sha256"],
            "dockerfileSHA256": hashlib.sha256((HERE / "Dockerfile").read_bytes()).hexdigest(),
            "renderedDockerfileSHA256": hashlib.sha256(render_dockerfile(labels)).hexdigest(),
            "verifierSHA256": hashlib.sha256((HERE / "verify_patch.py").read_bytes()).hexdigest(),
            "lockSHA256": hashlib.sha256((HERE / "package-lock.json").read_bytes()).hexdigest(),
            "labels": labels}


def render_dockerfile(labels):
    template = (HERE / "Dockerfile").read_text()
    lines = template.splitlines()
    if lines[:2] != ["FROM " + lock()["base"], "LABEL __KANZ_CATALOG_OWNERSHIP__"] or template.count("__KANZ_CATALOG_OWNERSHIP__") != 1:
        raise ValueError("first build step must bind complete ownership")
    if any(not re.fullmatch(r"[A-Za-z0-9_.-]+", key) or not re.fullmatch(r"[A-Za-z0-9_.-]+", value) for key, value in labels.items()):
        raise ValueError("unsafe build ownership labels")
    return template.replace("__KANZ_CATALOG_OWNERSHIP__", " ".join(key + "=" + json.dumps(value) for key, value in sorted(labels.items()))).encode()


def validate_base(base, record):
    if base.get("Id") != record["baseImageID"] or not isinstance(base.get("Config"), dict) or base["Config"].get("OnBuild") or not any(item.endswith("@" + record["baseReference"].rsplit("@", 1)[1]) for item in base.get("RepoDigests", [])):
        raise ValueError("exact base identity or pre-label OnBuild trigger refused")


def has_owner(info, record):
    config = info.get("Config")
    labels = config.get("Labels") if isinstance(config, dict) else None
    return isinstance(labels, dict) and all(labels.get(key) == value for key, value in record["labels"].items())


def validate_derived(base, derived, record):
    validate_base(base, record)
    image_id = derived.get("Id", "")
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", image_id) or image_id == base["Id"] or derived.get("RepoTags") or derived.get("RepoDigests") or derived.get("Os") != "linux" or derived.get("Architecture") != "amd64":
        raise ValueError("derived fixture image identity differs")
    if not has_owner(derived, record):
        raise ValueError("derived fixture image owner differs")
    if any(derived["Config"].get(key) != base["Config"].get(key) for key in BEHAVIOR):
        raise ValueError("derived fixture changed CLI/user/environment behavior")
    original = base["RootFS"]["Layers"]
    if derived["RootFS"]["Layers"][:len(original)] != original or len(derived["RootFS"]["Layers"]) <= len(original):
        raise ValueError("derived fixture does not extend the exact base layers")
    return {"imageID": image_id, "baseImageID": base["Id"], "baseReference": record["baseReference"],
            "packageSHA256": record["packageSHA256"], "dockerfileSHA256": record["dockerfileSHA256"],
            "renderedDockerfileSHA256": record["renderedDockerfileSHA256"],
            "verifierSHA256": record["verifierSHA256"], "lockSHA256": record["lockSHA256"],
            "behaviorSHA256": digest({key: base["Config"].get(key) for key in BEHAVIOR}),
            "originalBehaviorPreserved": True, "labels": record["labels"]}


def marker_body(record, phase, image_id=None, chain=None):
    return {"phase": phase, "labels": record["labels"], "baseImageID": record["baseImageID"],
            "renderedDockerfileSHA256": record["renderedDockerfileSHA256"], "imageID": image_id, "imageChain": chain}


def write_marker(record, phase, image_id=None, chain=None):
    path = record["startedPath" if phase == "started" else "completedPath"]
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, "w") as stream:
        stream.write(json.dumps(marker_body(record, phase, image_id, chain), sort_keys=True))
        stream.flush()
        os.fsync(stream.fileno())


def read_marker(record, phase):
    path = record["startedPath" if phase == "started" else "completedPath"]
    if not os.path.lexists(path):
        return None
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(descriptor) as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o600 or not 0 < info.st_size <= 4096:
            raise ValueError("unsafe build terminal marker")
        def unique(pairs):
            result = {}
            for key, value in pairs:
                if key in result:
                    raise ValueError("duplicate build marker key")
                result[key] = value
            return result
        marker = json.loads(stream.read(4097), object_pairs_hook=unique)
    if not isinstance(marker, dict):
        raise ValueError("invalid build terminal marker shape")
    image_id = marker.get("imageID") if phase == "completed" else None
    chain = marker.get("imageChain") if phase == "completed" else None
    if marker != marker_body(record, phase, image_id, chain) or phase == "completed" and (not isinstance(chain, list) or not 1 <= len(chain) <= 16 or any(not isinstance(value, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", value) for value in chain) or len(set(chain)) != len(chain) or chain[0] != image_id or record["baseImageID"] in chain):
        raise ValueError("foreign build terminal marker")
    return marker


def image_chain(images, record):
    if len(images) > 16 or record["baseImageID"] in images:
        raise ValueError("owned image chain exceeds bounds or includes base")
    children = {}
    for image_id, info in images.items():
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", image_id) or not isinstance(info, dict) or info.get("Id") != image_id or info.get("RepoTags") or info.get("RepoDigests") or not has_owner(info, record):
            raise ValueError("foreign image/tag cannot authorize cleanup")
        parent = info.get("Parent")
        if not isinstance(parent, str) or (parent != record["baseImageID"] and parent not in images) or parent in children:
            raise ValueError("image ancestry absent, branched or disconnected")
        children[parent] = image_id
    if not images:
        return []
    tips = set(images) - set(children)
    if len(tips) != 1:
        raise ValueError("image chain has no unique final child")
    chain, current = [], tips.pop()
    while current != record["baseImageID"]:
        if current in chain or current not in images:
            raise ValueError("image chain does not end at the exact base")
        chain.append(current)
        current = images[current]["Parent"]
    if set(chain) != set(images):
        raise ValueError("disconnected owned image chain")
    return chain


def cleanup(record, run):
    started, completed = read_marker(record, "started"), read_marker(record, "completed")
    if completed and not started:
        raise ValueError("build terminal acknowledgement lacks launch ownership")
    if started:
        base = json.loads(run(["docker", "image", "inspect", record["baseImageID"]], timeout=15))[0]
        validate_base(base, record)
    filters = [part for key, value in record["labels"].items() for part in ("--filter", "label=" + key + "=" + value)]
    removed_images, removed_containers, empty = [], [], 0
    # A bounded quiet inventory is cleanup evidence, never proof that a killed
    # build request is terminal. Only the exclusive completion marker can ack
    # a successful build response and bind its returned immutable image ID.
    for _ in range(6):
        ids = set(run(["docker", "image", "ls", "--all", "--no-trunc", *filters, "--format", "{{.ID}}"], timeout=15).splitlines())
        containers = set(run(["docker", "ps", "--all", "--no-trunc", *filters, "--format", "{{.ID}}"], timeout=15).splitlines())
        if ((ids or containers) and not started) or len(ids) > 16 or len(containers) > 4 or any(not re.fullmatch(r"sha256:[0-9a-f]{64}", value) for value in ids) or any(not re.fullmatch(r"[0-9a-f]{64}", value) for value in containers):
            raise ValueError("build resources lack bounded launch ownership")
        images = {image_id: json.loads(run(["docker", "image", "inspect", image_id], timeout=15))[0] for image_id in ids}
        chain = image_chain(images, record)
        if completed and chain and not any(chain == completed["imageChain"][index:] for index in range(len(completed["imageChain"]))):
            raise ValueError("terminal build image differs from owned chain tip")
        owned_containers = []
        for container_id in containers:
            info = json.loads(run(["docker", "inspect", "--type", "container", container_id], timeout=15))[0]
            if info.get("Id") != container_id or info.get("Image") not in images or not has_owner(info, record):
                raise ValueError("foreign build container cannot authorize cleanup")
            owned_containers.append(container_id)
        if not ids and not containers:
            empty += 1
            if empty >= 2:
                if started and not completed:
                    raise ValueError("interrupted daemon build completion remains unknown")
                return {"baseImageID": record["baseImageID"], "imageIDsRemoved": removed_images,
                        "buildContainerIDsRemoved": removed_containers, "emptyRechecks": empty,
                        "completionAcknowledged": bool(completed), "imageID": completed["imageID"] if completed else None}
        else:
            empty = 0
            try:
                for container_id in owned_containers:
                    run(["docker", "rm", "--force", "--volumes", container_id], timeout=15)
                    removed_containers.append(container_id)
                for image_id in chain:
                    # No force, tag, base deletion or parent pruning is allowed.
                    run(["docker", "image", "rm", "--no-prune", image_id], timeout=15)
                    removed_images.append(image_id)
            except (ValueError, OSError):
                # A cancelled builder can still unwind or publish an owned
                # child. Reinspect the entire identity/chain before retrying.
                pass
        time.sleep(0.2)
    raise ValueError("owned build cleanup did not reach a bounded empty inventory")


def validate_proof(proof, dependencies):
    selection = lock()
    expected = {"baseReference": selection["base"], "packageSHA256": selection["sha256"],
                "package": selection["package"] + ":" + selection["architecture"],
                "before": selection["before"], "after": selection["after"],
                "onlyFixedPackageChanged": True, "pythonUnchanged": True, "datahubVersion": "1.8.0"}
    if any(proof.get(key) != value for key, value in expected.items()) or proof.get("pythonVersion") != dependencies.get("python") or proof.get("pythonPackagesSHA256") != digest(dependencies.get("packages")):
        raise ValueError("executed CLI does not match the exact package/Python build proof")
    for key in ("beforePackagesSHA256", "afterPackagesSHA256", "pythonPackagesSHA256"):
        if not re.fullmatch(r"[0-9a-f]{64}", proof.get(key, "")):
            raise ValueError("package inventory proof hash absent")


def validate_cleanup(evidence, derived):
    chain = derived.get("imageChain")
    if not isinstance(chain, list) or not 1 <= len(chain) <= 16 or any(not isinstance(value, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", value) for value in chain) or len(set(chain)) != len(chain) or chain[0] != derived.get("imageID") or derived.get("baseImageID") in chain:
        raise ValueError("catalog build image-chain receipt incomplete")
    containers = evidence.get("buildContainerIDsRemoved")
    if evidence.get("completionAcknowledged") is not True or evidence.get("imageID") != derived.get("imageID") or evidence.get("baseImageID") != derived.get("baseImageID") or type(evidence.get("emptyRechecks")) is not int or evidence["emptyRechecks"] < 2 or evidence.get("imageIDsRemoved") != chain or not isinstance(containers, list) or len(containers) > 24 or any(not isinstance(value, str) or not re.fullmatch(r"[0-9a-f]{64}", value) for value in containers):
        raise ValueError("catalog build chain/terminal cleanup evidence incomplete")


def validate_build(build):
    if not isinstance(build, dict) or any(build.get(key) != value for key, value in BUILD.items()) or set(build.get("docker", {})) != {"client", "server", "clientAPI", "serverAPI"} or any(not isinstance(value, str) or not re.fullmatch(r"[A-Za-z0-9.+_-]{1,128}", value) for value in build["docker"].values()):
        raise ValueError("actual Docker/builder resource identity absent")


def scan_verdicts(identities, roles):
    if set(identities) != set(roles) or any(item.get("scanVerdict") not in {"passed", "advisory"} or not re.fullmatch(r"[0-9a-f]{64}", item.get("scanSHA256", "")) for item in identities.values()):
        raise ValueError("six-image scanner inventory incomplete")
    return [role for role in roles if identities[role]["scanVerdict"] == "advisory"]


def validate_scan(scan, reference, image_id):
    if scan.get("SchemaVersion") != 2 or scan.get("ArtifactType") != "container_image" or scan.get("ArtifactName") != reference or scan.get("Metadata", {}).get("ImageID") != image_id or not isinstance(scan.get("Results", []), list):
        raise ValueError("scanner report does not bind the executed image")
    findings = [item for result in scan.get("Results", []) for item in result.get("Vulnerabilities", [])]
    if any(item.get("Severity") != "CRITICAL" or not item.get("FixedVersion") or not item.get("VulnerabilityID") or not item.get("PkgName") for item in findings):
        raise ValueError("scanner report differs from fixed-CRITICAL policy")
    return bool(findings)
