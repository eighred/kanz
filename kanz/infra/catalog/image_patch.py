"""Pinned fixture-only package layer; never updates or removes the upstream base."""
import hashlib
import json
from pathlib import Path
import re
import urllib.request

HERE = Path(__file__).resolve().parent / "ingestion_patch"
BEHAVIOR = ("User", "Entrypoint", "Cmd", "WorkingDir", "Env", "ExposedPorts", "Healthcheck", "StopSignal", "Shell")
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
    return {"context": str(owner_path.parent / (owner["network"] + "-image-context")),
            "baseReference": selection["base"], "packageSHA256": selection["sha256"],
            "dockerfileSHA256": hashlib.sha256((HERE / "Dockerfile").read_bytes()).hexdigest(),
            "verifierSHA256": hashlib.sha256((HERE / "verify_patch.py").read_bytes()).hexdigest(),
            "lockSHA256": hashlib.sha256((HERE / "package-lock.json").read_bytes()).hexdigest(),
            "labels": {"kanz.catalog.owner": owner["nonce"], "kanz.catalog.source": owner["sourceSHA"], "kanz.catalog.run": owner["runID"], "kanz.catalog.attempt": owner["runAttempt"], "kanz.catalog.role": "ingestion-patched", "kanz.catalog.package": selection["sha256"]}}


def validate_derived(base, derived, record):
    image_id = derived.get("Id", "")
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", image_id) or image_id == base["Id"] or derived.get("RepoTags") or derived.get("RepoDigests") or derived.get("Os") != "linux" or derived.get("Architecture") != "amd64":
        raise ValueError("derived fixture image identity differs")
    if any(derived.get("Config", {}).get("Labels", {}).get(key) != value for key, value in record["labels"].items()):
        raise ValueError("derived fixture image owner differs")
    if any(derived["Config"].get(key) != base["Config"].get(key) for key in BEHAVIOR):
        raise ValueError("derived fixture changed CLI/user/environment behavior")
    original = base["RootFS"]["Layers"]
    if derived["RootFS"]["Layers"][:len(original)] != original or len(derived["RootFS"]["Layers"]) <= len(original):
        raise ValueError("derived fixture does not extend the exact base layers")
    return {"imageID": image_id, "baseImageID": base["Id"], "baseReference": record["baseReference"],
            "packageSHA256": record["packageSHA256"], "dockerfileSHA256": record["dockerfileSHA256"],
            "verifierSHA256": record["verifierSHA256"], "lockSHA256": record["lockSHA256"],
            "behaviorSHA256": digest({key: base["Config"].get(key) for key in BEHAVIOR}),
            "originalBehaviorPreserved": True, "labels": record["labels"]}


def cleanup(record, run):
    filters = [part for key, value in record["labels"].items() for part in ("--filter", "label=" + key + "=" + value)]
    ids = set(run(["docker", "image", "ls", "--all", "--no-trunc", *filters, "--format", "{{.ID}}"]).splitlines())
    if len(ids) > 1:
        raise ValueError("ambiguous owned derived image")
    for image_id in ids:
        info = json.loads(run(["docker", "image", "inspect", image_id]))[0]
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", image_id) or info.get("Id") != image_id or info.get("RepoTags") or info.get("RepoDigests") or any(info.get("Config", {}).get("Labels", {}).get(key) != value for key, value in record["labels"].items()):
            raise ValueError("foreign image/tag cannot authorize cleanup")
        # No tag, force, base deletion or parent pruning is authorized.
        run(["docker", "image", "rm", "--no-prune", image_id])
        if run(["docker", "image", "ls", "--all", "--no-trunc", *filters, "--format", "{{.ID}}"]):
            raise ValueError("derived image cleanup incomplete")


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
