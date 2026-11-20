"""Check the published sad-chrono candidate against its own and inherited advisories."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import urllib.parse
import urllib.request

PACKAGE = "sad-chrono"
VERSION = "0.5.1"
CHECKSUM = "59944182ae44e540eb6a61059eefb6d437b4bb3a7eff26e75149ed35601b7c5f"
PUBLISHER = "repos/sadco-io/sad-chrono/security-advisories"
UPSTREAM = "repos/chronotope/chrono/security-advisories"
RUSTSEC = "repos/rustsec/advisory-db"
REVIEWED_PUBLISHER = {
    "full_name": "sadco-io/sad-chrono", "id": 1462205045, "owner": 169411072,
    "archived": False, "disabled": False, "policy": "f166155c67f15b1f7a7b8c1878fe3c72fc123943",
}
REGISTRY = "https://crates.io/api/v1/crates/sad-chrono"
REGISTRY_OWNER = {"id": 365806, "login": "danielrcurtis", "kind": "user"}
GLOBAL_TYPES = ("reviewed", "unreviewed", "malware")
# Both records were reviewed against the exact published fork archive. The
# legacy localtime implementation was replaced in upstream 0.4.20. The fork
# has its own maintainer and is audited separately under its real package name.
REVIEWED_RUSTSEC = {
    "RUSTSEC-2020-0159.md": "84d1a4ac0fd9bb935da8ec4c6685a91e3973df67",
    "RUSTSEC-2026-0543.md": "e8129671299e6dfbde59591b103d0764995c39fc",
}
# These are known-affected versions of the published fork, including both
# maintenance lines. They detect an empty feed or loss of affected ranges.
FIXED_IN_047 = {"GHSA-4ff5-r8mp-gg8c", "GHSA-w24w-phgj-g759"}
FIXED_IN_051 = {"GHSA-2mpg-4h67-5j3p", "GHSA-fqmw-8fp3-jxfr", "GHSA-9vvx-xqq3-wm2g"}
UPSTREAM_GLOBAL = {"GHSA-cqpr-pcm7-m3jc": "2023-01-12T05:01:24Z"}


def release(version):
    if not isinstance(version, str) or not re.fullmatch(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", version):
        raise ValueError(f"unsupported release coordinate: {version!r}")
    return tuple(map(int, version.split(".")))


def affected(version, expression):
    value = release(version)
    outcomes = []
    for part in expression.split(","):
        match = re.fullmatch(r"\s*(<=|>=|<|>|=)?\s*([0-9]+\.[0-9]+\.[0-9]+)\s*", part)
        if not match:
            raise ValueError(f"cannot evaluate publisher range: {expression!r}")
        op, bound = match.groups()
        bound = release(bound)
        outcomes.append({"<": value < bound, "<=": value <= bound, ">": value > bound,
                         ">=": value >= bound, "=": value == bound, None: value == bound}[op])
    return all(outcomes)


def published_rows(snapshot, version):
    found = set()
    seen = set()
    for advisory in snapshot["publisher"]:
        ghsa = advisory["ghsa_id"]
        if not re.fullmatch(r"GHSA-[a-z0-9]{4}-[a-z0-9]{4}-[a-z0-9]{4}", ghsa) or ghsa in seen:
            raise ValueError("invalid or duplicate publisher advisory")
        seen.add(ghsa)
        if advisory["state"] != "published" or advisory.get("withdrawn_at"):
            raise ValueError(f"publisher status needs review: {ghsa}")
        rows = advisory["vulnerabilities"]
        if not rows:
            raise ValueError(f"publisher omitted affected packages: {ghsa}")
        for row in rows:
            if row["package"] != {"ecosystem": "rust", "name": PACKAGE}:
                raise ValueError(f"publisher package identity needs review: {ghsa}")
            if affected(version, row["vulnerable_version_range"]):
                found.add(ghsa)
    return found


def evaluate(snapshot, version, checksum):
    if (version, checksum) != (VERSION, CHECKSUM):
        raise ValueError("unqualified published sad-chrono archive")
    if snapshot["repository"] != REVIEWED_PUBLISHER:
        raise ValueError("publisher identity, project status or security policy needs review")
    registry = {"name": PACKAGE, "version": VERSION, "checksum": CHECKSUM, "yanked": False,
                "rust_version": "1.85.0", "publisher": 365806, "owners": [REGISTRY_OWNER]}
    if snapshot["registry"] != registry:
        raise ValueError("registry identity, archive, publisher, owners or availability changed")
    if snapshot["publisher_global"] != {kind: [] for kind in GLOBAL_TYPES}:
        raise ValueError("global sad-chrono disclosure needs review")
    if not re.fullmatch("[0-9a-f]{40}", snapshot["rustsec_commit"]):
        raise ValueError("unbound RustSec revision")
    if snapshot["rustsec_chrono"] != REVIEWED_RUSTSEC:
        raise ValueError("new, changed or missing shared Chrono RustSec evidence")
    if snapshot["upstream_publisher"]:
        raise ValueError("shared upstream publisher advisory needs applicability review")
    shared = snapshot["upstream_global"]
    if set(shared) != set(GLOBAL_TYPES) or shared["malware"] or shared["unreviewed"]:
        raise ValueError("new or missing shared upstream GitHub advisory category")
    global_rows = shared["reviewed"]
    upstream = {r["ghsa_id"]: r["updated_at"] for r in global_rows}
    if len(upstream) != len(global_rows) or upstream != UPSTREAM_GLOBAL:
        raise ValueError("new, changed or missing upstream GitHub advisory")
    native, = global_rows
    if native.get("withdrawn_at") != "2022-07-21T18:06:06Z" or native["vulnerabilities"] != [{
        "package": {"ecosystem": "rust", "name": "chrono"},
        "vulnerable_version_range": "< 0.4.20",
        "first_patched_version": "0.4.20",
    }]:
        raise ValueError("the reviewed upstream local-time fix boundary changed")
    for control, expected in (("0.4.46", FIXED_IN_047 | FIXED_IN_051), ("0.5.0", FIXED_IN_051)):
        if not expected.issubset(published_rows(snapshot, control)):
            raise ValueError(f"publisher lost known affected control {control}")
    findings = published_rows(snapshot, version)
    if findings:
        raise ValueError(f"published fork affected by {sorted(findings)}")


def retrieve(url):
    req = urllib.request.Request(url, headers={"Accept": "application/vnd.github+json",
                                              "User-Agent": "kanz-temporal-qualification"})
    with urllib.request.urlopen(req, timeout=30) as response:
        if response.url != url:
            raise ValueError("public evidence source redirected; review the new identity")
        raw = response.read(4 * 1024 * 1024 + 1)
        link = response.headers.get("Link", "")
    if len(raw) > 4 * 1024 * 1024:
        raise ValueError("advisory source exceeded response bound")
    return json.loads(raw), link


def request(path):
    return retrieve("https://api.github.com/" + path)[0]


def next_page(header, start):
    parsed = urllib.parse.urlsplit(start)
    allowed_paths = {parsed.path}
    aliases = {"/" + PUBLISHER: "/repositories/1462205045/security-advisories",
               "/" + UPSTREAM: "/repositories/18210201/security-advisories"}
    if parsed.path in aliases:
        allowed_paths.add(aliases[parsed.path])
    next_url = None
    for entry in header.split(",") if header else []:
        match = re.fullmatch(r'\s*<([^<>]+)>;\s*rel="(next|prev|first|last)"\s*', entry)
        if not match:
            raise ValueError("unrecognized GitHub pagination link")
        if match[2] != "next":
            continue
        candidate = urllib.parse.urlsplit(match[1])
        query = urllib.parse.parse_qsl(candidate.query, strict_parsing=True, keep_blank_values=True)
        controls = [(key, value) for key, value in query if key in {"after", "before", "page"}]
        filters = [(key, value) for key, value in query if key not in {"after", "before", "page"}]
        if (next_url or candidate.scheme != "https" or candidate.netloc != "api.github.com"
                or candidate.path not in allowed_paths or candidate.fragment or len(controls) != 1
                or not controls[0][1] or sorted(filters) != sorted(urllib.parse.parse_qsl(parsed.query))):
            raise ValueError("pagination changed source identity, selector or cursor")
        next_url = match[1]
    return next_url


def rows(path, per_page=100):
    sep = "&" if "?" in path else "?"
    start = f"https://api.github.com/{path}{sep}per_page={per_page}"
    url, out, seen = start, [], set()
    for _ in range(100):
        if url in seen:
            raise ValueError("pagination cycle")
        seen.add(url)
        part, header = retrieve(url)
        if not isinstance(part, list):
            raise ValueError("unexpected advisory collection")
        out.extend(part)
        url = next_page(header, start)
        if not url:
            return out
    raise ValueError("advisory pagination exceeded bound")


def collect():
    versions = retrieve(f"{REGISTRY}/{VERSION}")[0]["version"]
    owners = retrieve(f"{REGISTRY}/owners")[0]["users"]
    registry = {"name": versions["crate"], "version": versions["num"], "checksum": versions["checksum"],
                "yanked": versions["yanked"], "rust_version": versions["rust_version"],
                "publisher": versions["published_by"]["id"],
                "owners": [{k: owner[k] for k in ("id", "login", "kind")} for owner in owners]}
    repo = request("repos/sadco-io/sad-chrono")
    publisher = {k: repo[k] for k in ("full_name", "id", "archived", "disabled")}
    publisher["owner"] = repo["owner"]["id"]
    publisher["policy"] = request("repos/sadco-io/sad-chrono/contents/SECURITY.md")["sha"]
    revision = request(f"{RUSTSEC}/commits/main")["sha"]
    if not re.fullmatch("[0-9a-f]{40}", revision):
        raise ValueError("invalid RustSec revision")
    files = request(f"{RUSTSEC}/contents/crates/chrono?ref={revision}")
    if len(files) >= 1000 or any(item["type"] != "file" for item in files):
        raise ValueError("incomplete or unfamiliar RustSec package listing")
    # Retain review-relevant public metadata, excluding free text and contacts.
    def projection(advisories):
        result = []
        for record in advisories:
            row = {k: record[k] for k in ("ghsa_id", "updated_at", "withdrawn_at") if k in record}
            if "state" in record:
                row["state"] = record["state"]
            row["vulnerabilities"] = []
            for v in record["vulnerabilities"]:
                package = v["package"]
                projected = {"package": {k: package[k] for k in ("ecosystem", "name")},
                             "vulnerable_version_range": v["vulnerable_version_range"]}
                for key in ("first_patched_version", "patched_versions"):
                    if key in v:
                        projected[key] = v[key]
                row["vulnerabilities"].append(projected)
            result.append(row)
        return result

    def global_feeds(name):
        return {kind: projection(rows(f"advisories?ecosystem=rust&affects={name}&type={kind}"))
                for kind in GLOBAL_TYPES}

    return {"repository": publisher, "registry": registry, "publisher_global": global_feeds(PACKAGE),
            "rustsec_commit": revision, "rustsec_chrono": {f["name"]: f["sha"] for f in files},
            "publisher": projection(rows(PUBLISHER)), "upstream_publisher": projection(rows(UPSTREAM)),
            "upstream_global": global_feeds("chrono")}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument("--output", type=Path, help="collect and save fresh advisory data")
    source.add_argument("--replay", type=Path, help="evaluate an archived qualification snapshot offline")
    args = parser.parse_args()
    if args.replay is None:
        snapshot = collect()
        data = json.dumps(snapshot, sort_keys=True, separators=(",", ":")).encode()
        args.output.write_bytes(data)
    else:
        data = args.replay.read_bytes()
        snapshot = json.loads(data)
    evaluate(snapshot, VERSION, CHECKSUM)
    print(f"Qualified publisher and inherited advisory inputs at {snapshot['rustsec_commit']}; "
          f"snapshot SHA-256 {hashlib.sha256(data).hexdigest()}")


if __name__ == "__main__":
    main()
