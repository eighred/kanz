"""Bounded public HTTPS Git ref advertisements; no checkout or remote code."""

import hashlib
import ipaddress
import re
import socket
import ssl
import time
from http.client import HTTPException, HTTPSConnection
from urllib.parse import quote, unquote, urlsplit, urlunsplit

LIMIT = 2 * 1024 * 1024
OID = re.compile(r"(?:[0-9a-f]{40}|[0-9a-f]{64})")


class RefError(ValueError):
    pass


def repository_url(value):
    try:
        if not isinstance(value, str) or len(value) > 2048 or any(ord(char) <= 32 or ord(char) == 127 for char in value):
            raise RefError("invalid_repository_url")
        url = urlsplit(value)
        host = (url.hostname or "").encode("idna").decode("ascii").lower()
        path = unquote(url.path, errors="strict").rstrip("/")
        if (url.scheme != "https" or url.username is not None or url.password is not None
                or url.port not in (None, 443) or url.query or url.fragment
                or not re.fullmatch(r"[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?", host)
                or "." not in host or any(not part or part.startswith("-") or part.endswith("-") for part in host.split("."))
                or not path.startswith("/") or path == "/"
                or any(part in ("", ".", "..") for part in path[1:].split("/"))
                or any(ord(char) < 33 or ord(char) == 127 or char == "\\" for char in path)):
            raise RefError("invalid_repository_url")
        return urlunsplit(("https", host, quote(path, safe="/._~-"), "", ""))
    except (ValueError, UnicodeError):
        raise RefError("invalid_repository_url") from None


def public_address(host):
    try:
        addresses = {row[4][0] for row in socket.getaddrinfo(host, 443, type=socket.SOCK_STREAM)}
        if not addresses or any((not ipaddress.ip_address(address).is_global or ipaddress.ip_address(address).is_multicast or ipaddress.ip_address(address).is_reserved) for address in addresses):
            raise RefError("nonpublic_destination")
        return sorted(addresses, key=lambda value: (":" in value, value))[0]
    except (OSError, ValueError) as exc:
        if isinstance(exc, RefError):
            raise
        raise RefError("dns_unavailable") from None


class PublicHTTPS(HTTPSConnection):
    """Connect to the checked IP while verifying TLS against the original name."""

    def connect(self):
        raw = socket.create_connection((self.address, 443), timeout=self.timeout)
        try:
            self.sock = self._context.wrap_socket(raw, server_hostname=self.host)
        except BaseException:
            raw.close()
            raise


def request(url):
    parsed = urlsplit(url)
    connection = PublicHTTPS(parsed.hostname, timeout=10, context=ssl.create_default_context())
    connection.address = public_address(parsed.hostname)
    try:
        connection.request("GET", parsed.path + ("?" + parsed.query if parsed.query else ""),
                           headers={"User-Agent": "kanz-forge-upstream", "Accept-Encoding": "identity"})
        response = connection.getresponse()
        if response.status != 200:
            raise RefError(f"http_{response.status}")
        if response.getheader("Content-Encoding", "identity") != "identity":
            raise RefError("encoded_response_refused")
        data = bytearray()
        deadline = time.monotonic() + 20
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise RefError("response_timeout")
            if connection.sock is not None:
                connection.sock.settimeout(min(10, remaining))
            chunk = response.read1(min(65536, LIMIT + 1 - len(data)))
            if not chunk:
                break
            data.extend(chunk)
            if len(data) > LIMIT:
                raise RefError("response_too_large")
        if response.length not in (None, 0):
            raise RefError("incomplete_response")
        return bytes(data), response.getheader("Content-Type", "").split(";")[0].strip()
    except (OSError, HTTPException, ValueError) as exc:
        if isinstance(exc, RefError):
            raise
        raise RefError("network_unavailable") from None
    finally:
        connection.close()


def packets(data):
    offset, result = 0, []
    while offset < len(data):
        header = data[offset:offset + 4]
        if not re.fullmatch(b"[0-9a-fA-F]{4}", header):
            raise RefError("invalid_packet_header")
        size = int(header, 16)
        offset += 4
        if size == 0:
            result.append(None)
            continue
        if size < 4 or size > 65520 or offset + size - 4 > len(data):
            raise RefError("incomplete_ref_inventory")
        result.append(data[offset:offset + size - 4])
        offset += size - 4
    return result


def read_refs(value, fetch=request):
    # Protocol authority: https://git-scm.com/docs/http-protocol (v0 and dumb).
    url = repository_url(value)
    endpoint = url + "/info/refs?service=git-upload-pack"
    data, content_type = fetch(endpoint)
    if len(data) > LIMIT:
        raise RefError("response_too_large")
    sources = [{"url": endpoint, "sha256": hashlib.sha256(data).hexdigest()}]
    if content_type == "application/x-git-upload-pack-advertisement":
        rows = packets(data)
        if (len(rows) < 4 or rows[:2] != [b"# service=git-upload-pack\n", None]
                or rows[-1] is not None or any(row is None for row in rows[2:-1])):
            raise RefError("invalid_ref_advertisement")
        first, separator, capabilities = rows[2].partition(b"\0")
        if not separator or any(b"\0" in row for row in rows[3:-1]):
            raise RefError("invalid_ref_capabilities")
        heads = [token[12:] for token in capabilities.strip().split() if token.startswith(b"symref=HEAD:")]
        if len(heads) > 1:
            raise RefError("invalid_head_identity")
        records = [first] + rows[3:-1]
        lines = []
        for row in records:
            oid, separator, name = row.rstrip(b"\n").partition(b" ")
            if not separator:
                raise RefError("invalid_ref_inventory")
            lines.append(oid + b"\t" + name)
        head = b"ref: " + heads[0] if heads else None
    else:
        # The Git HTTP specification defines this as a dumb advertisement.
        # Reuse the received inventory; never retry a denial or follow a redirect.
        if data and not data.endswith(b"\n"):
            raise RefError("incomplete_ref_inventory")
        lines = data.splitlines()
        head_data, _ = fetch(url + "/HEAD")
        if len(head_data) > 4096:
            raise RefError("invalid_head_identity")
        sources.append({"url": url + "/HEAD", "sha256": hashlib.sha256(head_data).hexdigest()})
        head = head_data.strip()
    refs = {}
    for line in lines:
        try:
            oid, name = line.decode("utf-8").split("\t")
        except (UnicodeError, ValueError):
            raise RefError("invalid_ref_inventory") from None
        plain = name.removesuffix("^{}")
        if (not OID.fullmatch(oid) or int(oid, 16) == 0 or name in refs
                or (name != "HEAD" and not ref_name(plain))):
            raise RefError("invalid_ref_inventory")
        if name.endswith("^{}") and not name.startswith("refs/tags/"):
            raise RefError("invalid_ref_inventory")
        refs[name] = oid
    if len({len(oid) for oid in refs.values()}) > 1:
        raise RefError("invalid_ref_inventory")
    if any(name.endswith("^{}") and name[:-3] not in refs for name in refs):
        raise RefError("invalid_ref_inventory")
    if head is not None and head.startswith(b"ref: "):
        try:
            target = head[5:].decode("utf-8")
        except UnicodeError:
            raise RefError("invalid_head_identity") from None
        if not target.startswith("refs/heads/") or not ref_name(target) or target not in refs:
            raise RefError("invalid_head_identity")
        if "HEAD" in refs and refs["HEAD"] != refs[target]:
            raise RefError("head_identity_mismatch")
        refs["HEAD"] = refs[target]
        prefix = "ref: " + target + "\tHEAD\n"
    else:
        prefix = ""
        if head is not None:
            try:
                refs["HEAD"] = head.decode("ascii")
            except UnicodeError:
                raise RefError("invalid_head_identity") from None
    selected = prefix + "".join(oid + "\t" + name + "\n" for name, oid in refs.items()
                                if name == "HEAD" or name.startswith("refs/tags/"))
    return selected.encode("utf-8"), sources


def ref_name(value):
    return (len(value) <= 1024 and value.startswith("refs/") and not any(ord(char) <= 32 or ord(char) == 127 or char in "~^:?*[\\" for char in value)
            and ".." not in value and "//" not in value and "@{" not in value
            and not value.endswith(("/", "."))
            and all(not part.startswith(".") and not part.endswith(".lock") for part in value.split("/")))


def summarize(data, prefix=None):
    if prefix is not None and (not isinstance(prefix, str) or len(prefix) > 128
                               or any(ord(char) < 32 for char in prefix)):
        raise RefError("invalid_tag_prefix")
    if len(data) > LIMIT or (data and not data.endswith(b"\n")):
        raise RefError("incomplete_ref_inventory")
    try:
        lines = data.decode("utf-8").splitlines()
    except UnicodeError:
        raise RefError("invalid_ref_inventory") from None
    refs, branch = {}, None
    for line in lines:
        if line.startswith("ref: "):
            target, separator, name = line[5:].partition("\t")
            if not separator or name != "HEAD" or not target.startswith("refs/heads/") or not ref_name(target) or branch:
                raise RefError("invalid_head_identity")
            branch = target[len("refs/heads/"):]
            continue
        oid, separator, name = line.partition("\t")
        plain = name.removesuffix("^{}")
        if (not separator or not OID.fullmatch(oid) or int(oid, 16) == 0 or name in refs
                or (name != "HEAD" and (not plain.startswith("refs/tags/") or not ref_name(plain)))):
            raise RefError("invalid_ref_inventory")
        refs[name] = oid
    if len({len(oid) for oid in refs.values()}) > 1 or any(name.endswith("^{}") and name[:-3] not in refs for name in refs):
        raise RefError("invalid_ref_inventory")
    if "HEAD" not in refs:
        raise RefError("head_not_observed")
    result = {"branch": branch, "head_revision": refs["HEAD"], "inventory_sha256": hashlib.sha256(data).hexdigest()}
    families = {}
    for name, oid in refs.items():
        if not name.startswith("refs/tags/") or name.endswith("^{}"):
            continue
        tag = name[len("refs/tags/"):]
        match = re.fullmatch(r"(.*?)([0-9]{1,9}(?:[._-][0-9]{1,9}){1,3})", tag)
        if match and (prefix is None or match[1] == prefix):
            parts = tuple(int(part) for part in re.split(r"[._-]", match[2]))
            parts = parts + (0,) * (4 - len(parts))
            families.setdefault(match[1], []).append((parts, tag, refs.get(name + "^{}", oid)))
    result["tag_families"] = sorted(families)[:20]
    result["tag_family_count"] = len(families)
    if prefix is None and set(families) & {"", "v"}:
        candidates = [row for name in ("", "v") for row in families.get(name, [])]
        result["tag_family_scope"] = "root_numeric_tags"
    elif len(families) == 1:
        candidates = next(iter(families.values()))
    else:
        result["tag_status"] = "ambiguous_tag_family" if families else "no_numeric_tag_observed"
        return result
    if not candidates:
        result["tag_status"] = "no_numeric_tag_observed"
        return result
    highest = max(row[0] for row in candidates)
    choices = {(tag, oid) for parts, tag, oid in candidates if parts == highest}
    if len(choices) != 1:
        result["tag_status"] = "ambiguous_version_tag"
        return result
    tag, oid = choices.pop()
    result.update(tag_status="observed", version=tag, revision=oid, revision_kind="git_object")
    return result
