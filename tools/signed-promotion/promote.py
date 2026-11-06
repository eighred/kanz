#!/usr/bin/env python3
"""Copy a verified release pair; never build, sign, or deploy an image."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import threading

ROOT = Path(__file__).resolve().parents[2]
SERVICES = ("tv-sync", "kanz-migrate")
REGION = "ap-northeast-1"
ACCOUNT = "012619468098"
MAX_META = 8 * 1024 * 1024
MAX_BLOB = 4 * 1024**3
MAX_GRAPH = 16 * 1024**3
MAX_NODES = 4096
MANIFESTS = {
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
}


class Refused(Exception):
    pass


def require(value, message):
    if not value:
        raise Refused(message)


def digest(value):
    require(isinstance(value, str) and re.fullmatch(r"sha256:[0-9a-f]{64}", value), "invalid digest")
    return value


def command(*args, limit=MAX_META, hashed=False, timeout=180):
    # No command stderr, environment, config or token ever enters evidence.
    proc = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    timer = threading.Timer(timeout, proc.kill)
    timer.start()
    total, parts, result = 0, [], hashlib.sha256()
    try:
        while block := proc.stdout.read(65536):
            total += len(block)
            require(total <= limit, "tool output exceeds bound")
            if hashed:
                result.update(block)
            else:
                parts.append(block)
        require(proc.wait() == 0, "trusted tool failed")
        return ("sha256:" + result.hexdigest(), total) if hashed else b"".join(parts)
    finally:
        timer.cancel()
        if proc.poll() is None:
            proc.kill()
        proc.wait()
        proc.stdout.close()


def document(*args):
    return json.loads(command(*args))


def descriptor(d):
    require(isinstance(d, dict), "invalid descriptor")
    identity = digest(d.get("digest"))
    size = d.get("size")
    require(type(size) is int and 0 <= size <= MAX_BLOB, "invalid descriptor size")
    require(isinstance(d.get("mediaType"), str) and d["mediaType"], "missing media type")
    require(not d.get("urls"), "external content is unsupported")
    if "data" in d:
        require(isinstance(d["data"], str) and len(d["data"]) <= MAX_META, "embedded content exceeds bound")
        try:
            inline = base64.b64decode(d["data"], validate=True)
        except (ValueError, TypeError):
            raise Refused("invalid embedded content") from None
        require(len(inline) == size and "sha256:" + hashlib.sha256(inline).hexdigest() == identity,
                "embedded content identity mismatch")
    return identity, {"size": size, "mediaType": d["mediaType"]}


def resolve(ref):
    return digest(command("oras", "resolve", ref).decode().strip())


def verify_release(ref, tag, sha):
    identity = ["--certificate-oidc-issuer", "https://token.actions.githubusercontent.com",
                "--certificate-identity", f"https://github.com/eighred/kanz/.github/workflows/release.yml@refs/tags/{tag}",
                "--certificate-github-workflow-sha", sha]
    command("cosign", "verify", ref, *identity)
    for kind in ("spdxjson", "vuln"):
        command("cosign", "verify-attestation", ref, "--type", kind, *identity)


def graph(repository, root, legacy_lookup=None):
    """Fresh remote bytes, including every index child and recursive referrer.

    Legacy Cosign tags are enumerated separately because OCI referrers do not
    describe them. No raw image/config/bundle payload is retained or printed.
    """
    tags = command("oras", "repo", "tags", repository).decode().splitlines() if legacy_lookup is None else []
    require(len(tags) <= 100000 and len(tags) == len(set(tags)), "invalid tag inventory")
    require(all(re.fullmatch(r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}", t) for t in tags), "invalid registry tag")
    tags = set(tags)
    inventory, legacy, edges, pending = {}, {}, {}, [root]
    total = 0
    while pending:
        current = digest(pending.pop())
        if current in edges:
            continue
        require(len(edges) < MAX_NODES, "manifest graph exceeds bound")
        raw = command("oras", "manifest", "fetch", repository + "@" + current)
        require("sha256:" + hashlib.sha256(raw).hexdigest() == current, "manifest hash mismatch")
        body = json.loads(raw)
        require(body.get("schemaVersion") == 2 and body.get("mediaType") in MANIFESTS, "unsupported manifest")
        is_index = body["mediaType"] in {
            "application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json"}
        require("blobs" not in body, "unsupported artifact shape")
        if is_index:
            require(isinstance(body.get("manifests"), list) and body["manifests"] and
                    "config" not in body and "layers" not in body, "invalid index shape")
        else:
            require(isinstance(body.get("config"), dict) and isinstance(body.get("layers"), list) and
                    "manifests" not in body, "invalid image shape")
        desc = {"size": len(raw), "mediaType": body["mediaType"]}
        require(current not in inventory or inventory[current] == desc, "manifest descriptor mismatch")
        inventory[current] = desc
        total += len(raw)
        require(total <= MAX_GRAPH and len(inventory) <= MAX_NODES, "content graph exceeds bound")
        children = body.get("manifests", []) + body.get("layers", []) + body.get("blobs", [])
        if "config" in body:
            children.append(body["config"])
        if "subject" in body:
            children.append(body["subject"])
        edges[current] = []
        for child in children:
            key, metadata = descriptor(child)
            require(key not in inventory or inventory[key] == metadata, "conflicting descriptor")
            edges[current].append(key)
            if metadata["mediaType"] in MANIFESTS:
                inventory[key] = metadata
                pending.append(key)
            elif key not in inventory:
                if "data" not in child:
                    require(total + metadata["size"] <= MAX_GRAPH, "content graph exceeds bound")
                    actual, size = command("oras", "blob", "fetch", "--output", "-", repository + "@" + key,
                                           hashed=True, limit=metadata["size"], timeout=600)
                    require(actual == key and size == metadata["size"], "blob hash or size mismatch")
                    total += size
                # Inline bytes were freshly fetched inside this manifest and
                # already hash/size-checked by descriptor(), per OCI1.1.
                inventory[key] = metadata
            require(total <= MAX_GRAPH and len(inventory) <= MAX_NODES, "content graph exceeds bound")
        refs = document("oras", "discover", "--format", "json", "--depth", "1", repository + "@" + current)
        require(refs.get("digest") == current and isinstance(refs.get("referrers"), list), "invalid referrer inventory")
        require(descriptor(refs)[1] == desc, "discovery root descriptor mismatch")
        for ref in refs["referrers"]:
            key, metadata = descriptor(ref)
            require(metadata["mediaType"] in MANIFESTS, "unsupported referrer")
            require(key not in inventory or inventory[key] == metadata, "conflicting referrer")
            inventory[key] = metadata
            require(len(inventory) <= MAX_NODES, "referrer graph exceeds bound")
            pending.append(key)
            edges[current].append(key)
        for suffix in ("sig", "att", "sbom"):
            tag = current.replace(":", "-") + "." + suffix
            key = None
            if legacy_lookup is not None:
                key = legacy_lookup(tag)
            elif tag in tags:
                key = resolve(repository + ":" + tag)
            if key is not None:
                digest(key)
                legacy[tag] = key
                pending.append(key)
                edges[current].append(key)
        edges[current] = sorted(set(edges[current]))
    return {"root": root, "descriptors": inventory, "edges": edges, "legacy": legacy}


def target_digest(account, service, tag):
    result = document("aws", "ecr", "batch-get-image", "--region", REGION, "--registry-id", account,
                      "--repository-name", service, "--image-ids", "imageTag=" + tag, "--no-cli-pager")
    images, failures = result.get("images", []), result.get("failures", [])
    if not images:
        require(len(failures) == 1 and failures[0].get("failureCode") == "ImageNotFound", "destination unavailable")
        return None
    require(not failures and len(images) == 1 and images[0].get("registryId") == account and
            images[0].get("repositoryName") == service and images[0].get("imageId", {}).get("imageTag") == tag,
            "invalid destination lookup")
    return digest(images[0]["imageId"].get("imageDigest"))


def target_available(account, service, tag, expected):
    actual = target_digest(account, service, tag)
    require(actual is None or actual == expected, "destination tag conflicts")


def promote(account, tag, sha, images):
    require(account == ACCOUNT, "unexpected destination account")
    require(len(tag) <= 100 and re.fullmatch(r"v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?", tag), "unsupported release tag")
    require(re.fullmatch(r"[0-9a-f]{40}", sha), "invalid release SHA")
    require(set(images) == set(SERVICES), "pair must contain exactly the reviewed services")
    require(document("aws", "sts", "get-caller-identity", "--no-cli-pager").get("Account") == account, "unexpected AWS account")
    require(command("git", "-C", str(ROOT), "rev-parse", f"refs/tags/{tag}^{{commit}}").decode().strip() == sha,
            "release tag source mismatch")
    command("git", "-C", str(ROOT), "merge-base", "--is-ancestor", sha, "origin/main")
    registry = account + ".dkr.ecr." + REGION + ".amazonaws.com"
    destination_tag = "release-" + tag
    prepared = {}
    # All signature, graph and conflict preflight completes for BOTH images
    # before the first write. A later error never emits paired acceptance.
    for service in SERVICES:
        root = digest(images[service])
        source, target = "ghcr.io/eighred/" + service, registry + "/" + service
        # release.yml metadata-action uses semver {{version}} without leading v.
        # Certificate identity still names the exact Git refs/tags/v... value.
        require(resolve(source + ":" + tag[1:]) == root, "source release tag mismatch")
        verify_release(source + "@" + root, tag, sha)
        observed = graph(source, root)
        target_available(account, service, destination_tag, root)
        for legacy, identity in observed["legacy"].items():
            target_available(account, service, legacy, identity)
        prepared[service] = (source, target, observed)
    for source, target, observed in prepared.values():
        command("oras", "cp", "-r", source + "@" + observed["root"], target + ":" + destination_tag, timeout=1200)
        for legacy, identity in observed["legacy"].items():
            command("oras", "cp", "-r", source + "@" + identity, target + ":" + legacy, timeout=1200)
    accepted = {}
    for service, (source, target, observed) in prepared.items():
        root = observed["root"]
        require(resolve(source + ":" + tag[1:]) == root and resolve(target + ":" + destination_tag) == root, "tag moved during promotion")
        require(graph(source, root) == observed, "source graph changed during promotion")
        require(graph(target, root, legacy_lookup=lambda name: target_digest(account, service, name)) == observed,
                "destination graph differs")
        verify_release(source + "@" + root, tag, sha)
        verify_release(target + "@" + root, tag, sha)
        accepted[service] = {"source": source + "@" + root, "destination": target + "@" + root, "graph": observed}
    return {"status": "verified-pair", "release_tag": tag, "release_sha": sha,
            "destination_tag": destination_tag, "images": accepted}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--account", required=True)
    parser.add_argument("--release-tag", required=True)
    parser.add_argument("--release-sha", required=True)
    parser.add_argument("--tv-sync", required=True)
    parser.add_argument("--kanz-migrate", required=True)
    parser.add_argument("--evidence", type=Path, required=True)
    args = parser.parse_args()
    require(os.environ.get("GITHUB_REPOSITORY") == "eighred/kanz" and
            os.environ.get("GITHUB_REF") == "refs/heads/main" and
            os.environ.get("GITHUB_EVENT_NAME") == "workflow_dispatch", "trusted main dispatch required")
    require(args.account == ACCOUNT, "unexpected destination account")
    os.environ["PROMOTION_AWS_ACCOUNT"] = args.account
    # Only helper names are written. Neither Docker nor ORAS login is called.
    with tempfile.TemporaryDirectory(prefix="kanz-promotion-") as tmp:
        os.chmod(tmp, 0o700)
        config = {"credHelpers": {"ghcr.io": "kanz-promotion",
                  args.account + ".dkr.ecr." + REGION + ".amazonaws.com": "kanz-promotion"}}
        Path(tmp, "config.json").write_text(json.dumps(config))
        os.environ["DOCKER_CONFIG"] = tmp
        os.environ["PATH"] = str(Path(__file__).parent) + os.pathsep + os.environ["PATH"]
        # Refuse ambient redirects/bypass knobs rather than inheriting them.
        require(not any(k.startswith("COSIGN_") for k in os.environ), "ambient Cosign configuration refused")
        result = promote(args.account, args.release_tag, args.release_sha,
                         {"tv-sync": args.tv_sync, "kanz-migrate": args.kanz_migrate})
        result["workflow_sha"] = command("git", "-C", str(ROOT), "rev-parse", "HEAD").decode().strip()
    # Cleanup must finish before success evidence exists.
    with args.evidence.open("x") as output:
        json.dump(result, output, indent=2, sort_keys=True)


if __name__ == "__main__":
    try:
        main()
    except Refused as exc:
        raise SystemExit(str(exc) + "; no paired acceptance recorded") from None
    except Exception:
        raise SystemExit("signed promotion refused; no paired acceptance recorded") from None
