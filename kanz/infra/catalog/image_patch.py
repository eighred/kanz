"""Pinned fixture-only package layer; never updates or removes the upstream base."""
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import time
import urllib.error
import urllib.request

import image_identity

HERE = Path(__file__).resolve().parent / "ingestion_patch"
BEHAVIOR = ("User", "Entrypoint", "Cmd", "WorkingDir", "Env", "ExposedPorts", "Healthcheck", "StopSignal", "Shell", "OnBuild")
ADVISORY_EXIT = 42
PACKAGE_REFUSALS = frozenset({"package-publisher-refused", "package-redirect-refused", "package-http-refused", "package-timeout", "package-network-refused", "package-integrity-refused"})
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


def download(selection, evidence=None):
    evidence = evidence if evidence is not None else {}
    evidence.update(category="package-request-pending", httpStatus=None, receivedBytes=None, receivedSHA256=None)
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, *_args, **_kwargs):
            return None
    if selection["url"] != "https://security.ubuntu.com/ubuntu/pool/main/l/linux/linux-libc-dev_6.8.0-149.149_amd64.deb":
        evidence["category"] = "package-publisher-refused"
        raise ValueError("fixed package publisher differs")
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    try:
        with opener.open(selection["url"], timeout=30) as response:
            evidence["httpStatus"] = response.status
            body = response.read(selection["bytes"] + 1)
    except urllib.error.HTTPError as error:
        evidence.update(category="package-redirect-refused" if 300 <= error.code < 400 else "package-http-refused", httpStatus=error.code)
        raise ValueError("fixed package HTTP request refused") from None
    except OSError as error:
        evidence["category"] = "package-timeout" if isinstance(error, TimeoutError) or isinstance(getattr(error, "reason", None), TimeoutError) else "package-network-refused"
        raise ValueError("fixed package download refused") from None
    evidence.update(receivedBytes=len(body), receivedSHA256=hashlib.sha256(body).hexdigest())
    try:
        validate_package(body, selection)
    except ValueError:
        evidence["category"] = "package-integrity-refused"
        raise ValueError("fixed package integrity refused") from None
    evidence["category"] = "passed"
    return body


def owned_spec(owner, owner_path):
    selection = lock()
    labels = {"kanz.catalog.owner": owner["nonce"], "kanz.catalog.source": owner["sourceSHA"], "kanz.catalog.run": owner["runID"], "kanz.catalog.attempt": owner["runAttempt"], "kanz.catalog.role": "ingestion-patched", "kanz.catalog.package": selection["sha256"]}
    return {"context": str(owner_path.parent / (owner["network"] + "-image-context")),
            "startedPath": str(owner_path.parent / (owner["network"] + "-build-started.json")),
            "completedPath": str(owner_path.parent / (owner["network"] + "-build-completed.json")),
            "baseConfigDigest": selection["baseImageID"],
            "baseManifestDigest": selection["base"].rsplit("@", 1)[1],
            "descriptorLockSHA256": image_identity.lock_digest(),
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


def observe_base(base, record):
    config = base.get("Config")
    triggers = config.get("OnBuild") if isinstance(config, dict) else "invalid"
    digests = base.get("RepoDigests")
    image_id = base.get("Id")
    selected = image_identity.selections()["ingestion"]
    try:
        kind = image_identity.engine_kind(image_id, selected)
    except ValueError:
        kind = "unknown"
    return {"imageID": image_id if isinstance(image_id, str) and re.fullmatch(r"sha256:[0-9a-f]{64}", image_id) else None,
            "engineIDKind": kind,
            "onBuildCount": len(triggers) if isinstance(triggers, list) else 0 if triggers is None else None,
            "onBuildEmpty": isinstance(config, dict) and (triggers is None or isinstance(triggers, list) and not triggers),
            "repoDigestCount": len(digests) if isinstance(digests, list) else None,
            "selectedManifestDigestMatches": image_identity.manifest_bound(digests, selected)}


def validate_base(base, record):
    observation = observe_base(base, record)
    selected = image_identity.selections()["ingestion"]
    if record["baseConfigDigest"] != selected["configDigest"] or record["baseManifestDigest"] != selected["manifestDigest"] or record["descriptorLockSHA256"] != image_identity.lock_digest() or not observation["onBuildEmpty"]:
        raise ValueError("exact base identity or pre-label OnBuild trigger refused")
    return image_identity.verify_engine(base, selected)


def has_owner(info, record):
    config = info.get("Config")
    labels = config.get("Labels") if isinstance(config, dict) else None
    return isinstance(labels, dict) and all(labels.get(key) == value for key, value in record["labels"].items())


def validate_derived(base, derived, record):
    base_identity = validate_base(base, record)
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
            "baseIdentity": base_identity, "baseConfigDigest": record["baseConfigDigest"],
            "baseManifestDigest": record["baseManifestDigest"], "descriptorLockSHA256": record["descriptorLockSHA256"],
            "packageSHA256": record["packageSHA256"], "dockerfileSHA256": record["dockerfileSHA256"],
            "renderedDockerfileSHA256": record["renderedDockerfileSHA256"],
            "verifierSHA256": record["verifierSHA256"], "lockSHA256": record["lockSHA256"],
            "behaviorSHA256": digest({key: base["Config"].get(key) for key in BEHAVIOR}),
            "rootFSDiffIDs": derived["RootFS"]["Layers"],
            "originalBehaviorPreserved": True, "labels": record["labels"]}


def marker_body(record, phase, image_id=None, chain=None, base=None):
    if not base or image_identity.engine_kind(base.get("engineID"), image_identity.selections()["ingestion"]) != base.get("engineIDKind"):
        raise ValueError("actual base engine binding required")
    return {"phase": phase, "labels": record["labels"], "baseImageID": base["engineID"], "baseEngineIDKind": base["engineIDKind"],
            "descriptorLockSHA256": record["descriptorLockSHA256"],
            "renderedDockerfileSHA256": record["renderedDockerfileSHA256"], "imageID": image_id, "imageChain": chain}


def write_marker(record, phase, image_id=None, chain=None, base=None):
    body = marker_body(record, phase, image_id, chain, base)
    path = record["startedPath" if phase == "started" else "completedPath"]
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, "w") as stream:
        stream.write(json.dumps(body, sort_keys=True))
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
    base = {"engineID": marker.get("baseImageID"), "engineIDKind": marker.get("baseEngineIDKind")}
    if marker != marker_body(record, phase, image_id, chain, base) or phase == "completed" and (not isinstance(chain, list) or not 1 <= len(chain) <= 16 or any(not isinstance(value, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", value) for value in chain) or len(set(chain)) != len(chain) or chain[0] != image_id or base["engineID"] in chain):
        raise ValueError("foreign build terminal marker")
    return marker


def image_chain(images, record, base_image_id):
    if len(images) > 16 or base_image_id in images or (images and not base_image_id):
        raise ValueError("owned image chain exceeds bounds or includes base")
    children = {}
    for image_id, info in images.items():
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", image_id) or not isinstance(info, dict) or info.get("Id") != image_id or info.get("RepoTags") or info.get("RepoDigests") or not has_owner(info, record):
            raise ValueError("foreign image/tag cannot authorize cleanup")
        parent = info.get("Parent")
        if not isinstance(parent, str) or (parent != base_image_id and parent not in images) or parent in children:
            raise ValueError("image ancestry absent, branched or disconnected")
        children[parent] = image_id
    if not images:
        return []
    tips = set(images) - set(children)
    if len(tips) != 1:
        raise ValueError("image chain has no unique final child")
    chain, current = [], tips.pop()
    while current != base_image_id:
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
    base_image_id = started["baseImageID"] if started else None
    if started:
        base = json.loads(run(["docker", "image", "inspect", base_image_id], timeout=15))[0]
        actual = validate_base(base, record)
        if actual["engineID"] != base_image_id or actual["engineIDKind"] != started["baseEngineIDKind"] or (completed and (completed["baseImageID"], completed["baseEngineIDKind"]) != (base_image_id, started["baseEngineIDKind"])):
            raise ValueError("build markers do not bind one actual base engine identity")
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
        chain = image_chain(images, record, base_image_id)
        if completed and chain and not any(chain == completed["imageChain"][index:] for index in range(len(completed["imageChain"]))):
            raise ValueError("terminal build image differs from owned chain tip")
        owned_containers = []
        for container_id in containers:
            info = json.loads(run(["docker", "inspect", "--type", "container", container_id], timeout=15))[0]
            if info.get("Id") != container_id or info.get("Image") not in (set(images) | {base_image_id}) or not has_owner(info, record):
                raise ValueError("foreign build container cannot authorize cleanup")
            owned_containers.append(container_id)
        if not ids and not containers:
            empty += 1
            if empty >= 2:
                if started and not completed:
                    raise ValueError("interrupted daemon build completion remains unknown")
                return {"baseImageID": base_image_id, "baseEngineIDKind": started["baseEngineIDKind"] if started else None, "imageIDsRemoved": removed_images,
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
                "onlyFixedPackageChanged": True, "pythonUnchanged": True, "datahubVersion": selection["cliPackageVersion"]}
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
    if evidence.get("completionAcknowledged") is not True or evidence.get("imageID") != derived.get("imageID") or evidence.get("baseImageID") != derived.get("baseImageID") or evidence.get("baseEngineIDKind") != derived.get("baseIdentity", {}).get("engineIDKind") or type(evidence.get("emptyRechecks")) is not int or evidence["emptyRechecks"] < 2 or evidence.get("imageIDsRemoved") != chain or not isinstance(containers, list) or len(containers) > 24 or any(not isinstance(value, str) or not re.fullmatch(r"[0-9a-f]{64}", value) for value in containers):
        raise ValueError("catalog build chain/terminal cleanup evidence incomplete")


def validate_build(build):
    if not isinstance(build, dict) or any(build.get(key) != value for key, value in BUILD.items()) or set(build.get("docker", {})) != {"client", "server", "clientAPI", "serverAPI"} or any(not isinstance(value, str) or not re.fullmatch(r"[A-Za-z0-9.+_-]{1,128}", value) for value in build["docker"].values()):
        raise ValueError("actual Docker/builder resource identity absent")


def build_diagnostic(output, code):
    text = output.decode("utf-8", errors="replace")
    phases = {"before-inventory", "package-install", "after-inventory", "completed"}
    phase, observations, refused, proof = None, {}, False, False
    for line in text.splitlines():
        if line.startswith("KANZ_CATALOG_PATCH_PHASE=") and line.removeprefix("KANZ_CATALOG_PATCH_PHASE=") in phases:
            phase = line.removeprefix("KANZ_CATALOG_PATCH_PHASE=")
        elif line == "KANZ_CATALOG_PATCH_PROOF=passed":
            proof = True
        elif line.startswith("KANZ_CATALOG_PATCH_OBSERVATION="):
            try:
                if len(line) > 1024:
                    raise ValueError("observation exceeds bound")
                item = json.loads(line.removeprefix("KANZ_CATALOG_PATCH_OBSERVATION="))
                if set(item) != {"phase", "targetVersionState", "datahubVersionMatches", "pythonVersion", "packageCount", "pythonPackageCount"} or item["phase"] not in {"before", "after"} or item["targetVersionState"] not in {"expected", "missing", "unexpected"} or type(item["datahubVersionMatches"]) is not bool or not isinstance(item["pythonVersion"], str) or not re.fullmatch(r"[0-9]{1,2}\.[0-9]{1,2}\.[0-9]{1,3}", item["pythonVersion"]) or any(type(item[key]) is not int or not 1 <= item[key] <= 10000 for key in ("packageCount", "pythonPackageCount")):
                    raise ValueError("observation shape refused")
                observations[item["phase"]] = item
            except (ValueError, KeyError, TypeError):
                refused = True
    lower = text.lower()
    signals = (
        ("build-disk-budget", ("no space left on device", "disk quota exceeded")),
        ("build-memory-budget", ("out of memory", "cannot allocate memory", "oomkilled")),
        ("build-legacy-unavailable", ("legacy builder is not supported", "legacy builder is no longer supported", "legacy builder has been removed")),
        ("build-profile-unsupported", ("unknown flag:", "requires buildkit", "unsupported build option")),
        ("build-registry-access-refused", ("unauthorized:", "authentication required", "pull access denied", "403 forbidden")),
        ("build-daemon-unavailable", ("cannot connect to the docker daemon", "error during connect:")),
        ("build-permission-refused", ("operation not permitted", "permission denied")),
        ("build-tool-unavailable", ("python: not found", "dpkg: not found", "dpkg-query: not found")),
        ("build-package-version-refused", ("fixed package versions differ",)),
        ("build-python-package-proof-refused", ("single-package/python compatibility proof refused",)),
        ("build-package-integrity-refused", ("fixed package bytes differ",)),
        ("build-inventory-budget", ("package inventory exceeded bound",)),
    )
    matched = [category for category, markers in signals if any(marker in lower for marker in markers)]
    category = "passed" if code == 0 else "build-multiple-signals" if len(matched) > 1 else matched[0] if matched else "build-recipe-" + phase + "-refused" if phase in phases - {"completed"} else "build-command-exit"
    return {"category": category, "exitCode": code, "informationalOnly": True, "processCaptureComplete": True,
            "capturedBytes": len(output), "capturedSHA256": hashlib.sha256(output).hexdigest(),
            "matchedSignals": matched, "lastRecipePhase": phase,
            "observations": [observations[key] for key in ("before", "after") if key in observations],
            "observationRefused": refused, "packageProofObserved": proof}


def scan_verdicts(identities, roles):
    if set(identities) != set(roles) or any(item.get("scanVerdict") not in {"passed", "advisory"} or not re.fullmatch(r"[0-9a-f]{64}", item.get("scanSHA256", "")) for item in identities.values()):
        raise ValueError("six-image scanner inventory incomplete")
    return [role for role in roles if identities[role]["scanVerdict"] == "advisory"]


def validate_scan(scan, reference, identity):
    if scan.get("SchemaVersion") != 2 or scan.get("ArtifactType") != "container_image" or scan.get("ArtifactName") != reference or not isinstance(scan.get("Results", []), list):
        raise ValueError("scanner report does not bind the executed image")
    image_identity.verify_scan(scan.get("Metadata", {}), identity)
    findings = [item for result in scan.get("Results", []) for item in result.get("Vulnerabilities", [])]
    if any(item.get("Severity") != "CRITICAL" or not item.get("FixedVersion") or not item.get("VulnerabilityID") or not item.get("PkgName") for item in findings):
        raise ValueError("scanner report differs from fixed-CRITICAL policy")
    return bool(findings)
