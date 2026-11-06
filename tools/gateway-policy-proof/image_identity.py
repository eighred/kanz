"""Resolve only the reviewed public Python index; never emit auth/config bytes."""
import hashlib
import json
import re
import urllib.error
import urllib.parse
import urllib.request

from rollout_probe import IMAGE, verify_cri

ROOT_DIGEST = IMAGE.split("@", 1)[1]
REGISTRY = "https://registry-1.docker.io/v2/library/python/"
ACCEPT = ",".join(("application/vnd.oci.image.index.v1+json", "application/vnd.oci.image.manifest.v1+json",
                   "application/vnd.docker.distribution.manifest.list.v2+json", "application/vnd.docker.distribution.manifest.v2+json"))


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def document(raw):
    def pairs(values):
        result = {}
        for key, value in values:
            if key in result:
                raise ValueError("duplicate image metadata field")
            result[key] = value
        return result
    if not raw or len(raw) > 1048576:
        raise ValueError("image metadata exceeds bound")
    value = json.loads(raw, object_pairs_hook=pairs)
    if not isinstance(value, dict):
        raise ValueError("image metadata is not an object")
    return value


def checked(raw, digest, size=None):
    if not isinstance(digest, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        raise ValueError("invalid image digest")
    if size is not None and (type(size) is not int or not 0 < size <= 1048576 or len(raw) != size):
        raise ValueError("image descriptor size differs")
    if "sha256:" + hashlib.sha256(raw).hexdigest() != digest:
        raise ValueError("image bytes differ from digest")
    return document(raw)


def resolve(fetch):
    index = checked(fetch("manifests/" + ROOT_DIGEST), ROOT_DIGEST)
    if index.get("schemaVersion") != 2:
        raise ValueError("image root schema differs")
    if index.get("mediaType") in ("application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json"):
        entries = index.get("manifests")
        if not isinstance(entries, list) or not 1 <= len(entries) <= 64:
            raise ValueError("image platform inventory invalid")
        matches = [p for p in entries if isinstance(p, dict) and p.get("platform", {}).get("os") == "linux"
                   and p.get("platform", {}).get("architecture") == "amd64" and not p["platform"].get("variant")]
        if len(matches) != 1:
            raise ValueError("linux amd64 image is ambiguous")
        child = matches[0]
        manifest_digest = child["digest"]
        manifest = checked(fetch("manifests/" + manifest_digest), manifest_digest, child["size"])
    else:
        manifest_digest, manifest = ROOT_DIGEST, index
    if manifest.get("schemaVersion") != 2 or manifest.get("mediaType") not in (
            "application/vnd.oci.image.manifest.v1+json", "application/vnd.docker.distribution.manifest.v2+json"):
        raise ValueError("image child is not a manifest")
    config = manifest["config"]
    if config.get("mediaType") not in ("application/vnd.oci.image.config.v1+json", "application/vnd.docker.container.image.v1+json"):
        raise ValueError("unsupported image config")
    value = checked(fetch("blobs/" + config["digest"]), config["digest"], config["size"])
    if value.get("os") != "linux" or value.get("architecture") != "amd64":
        raise ValueError("image config platform differs")
    return {"root": ROOT_DIGEST, "rootMediaType": index["mediaType"], "manifest": manifest_digest,
            "config": config["digest"], "platform": "linux/amd64"}


def public_identity():
    # Docker's documented auth/registry/CDN endpoints only. No ambient proxy,
    # credential helper, Docker config, or user token is consulted.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    def get(url, headers):
        request = urllib.request.Request(url, headers=headers)
        with opener.open(request, timeout=25) as response:
            if response.status != 200:
                raise ValueError("public registry response refused")
            data = response.read(1048577)
            if len(data) > 1048576:
                raise ValueError("public registry response exceeds bound")
            return data
    stage = "anonymous_auth"
    try:
        token_doc = document(get("https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/python:pull", {}))
        token = token_doc.get("token")
        if not isinstance(token, str) or not re.fullmatch(r"[A-Za-z0-9._-]{1,8192}", token):
            raise ValueError("anonymous registry token invalid")
        def fetch(path):
            nonlocal stage
            stage = "config_blob" if path.startswith("blobs/") else "index_or_manifest"
            if not re.fullmatch(r"(?:manifests|blobs)/sha256:[0-9a-f]{64}", path):
                raise ValueError("registry request outside fixed image")
            try:
                return get(REGISTRY + path, {"Authorization": "Bearer " + token, "Accept": ACCEPT})
            except urllib.error.HTTPError as error:
                if error.code != 307 or not path.startswith("blobs/"):
                    raise
                location = error.headers.get("Location", "")
                parsed = urllib.parse.urlsplit(location)
                if (parsed.scheme != "https" or parsed.hostname != "production.cloudfront.docker.com"
                        or parsed.port not in (None, 443) or parsed.username or parsed.password or parsed.fragment):
                    raise ValueError("public blob redirect outside documented CDN") from None
                # Never forward the registry Authorization header across hosts.
                return get(location, {})
        return resolve(fetch)
    except Exception as error:
        # URLs can carry CDN signatures and errors can quote auth-bearing URLs.
        code = error.code if isinstance(error, urllib.error.HTTPError) else 0
        reason = str(error) if str(error) in {
            "linux amd64 image is ambiguous", "image bytes differ from digest", "image descriptor size differs",
            "public blob redirect outside documented CDN", "image platform inventory invalid",
            "reviewed image root is not an index", "image child is not a manifest", "image config platform differs",
        } else type(error).__name__
        raise ValueError("fixed public image identity refused at " + stage + ": " + reason + " HTTP " + str(code)) from None




if __name__ == "__main__":
    print(json.dumps(public_identity(), sort_keys=True))
