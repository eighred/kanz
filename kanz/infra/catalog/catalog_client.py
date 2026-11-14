"""Actual DataHub readback; executed only in the isolated pinned CLI image."""
import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

from projection import load, mappings, project, urn


def request(path, body=None, authorization=True):
    headers = {"Content-Type": "application/json"}
    if authorization:
        headers["Authorization"] = os.environ["DATAHUB_GMS_AUTHORIZATION"]
    url = "http://gms:8080" + path
    req = urllib.request.Request(url, data=None if body is None else json.dumps(body).encode(), headers=headers)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(req, timeout=10) as response:
        raw = response.read(1024 * 1024 + 1)
    if len(raw) > 1024 * 1024:
        raise ValueError("catalog response exceeds bound")
    return json.loads(raw)


def aspect(entity, name):
    query = urllib.parse.urlencode({"aspect": name, "version": 0})
    response = request("/aspects/" + urllib.parse.quote(entity, safe="") + "?" + query)
    result = response["aspect"]
    # Rest.li wraps the concrete aspect in its Pegasus union name.
    if isinstance(result, dict) and len(result) == 1:
        name, value = next(iter(result.items()))
        if name.startswith("com.linkedin."):
            return value
    return result


def contains(actual, expected):
    if isinstance(expected, dict):
        return isinstance(actual, dict) and all(key in actual and contains(actual[key], value) for key, value in expected.items())
    if isinstance(expected, list):
        return isinstance(actual, list) and len(actual) == len(expected) and all(contains(left, right) for left, right in zip(actual, expected))
    return actual == expected


def readback():
    environment, rows = mappings(load("/catalog/fixture_mapping.json"))
    expected = project(load("/catalog/fixture_mapping.json"), load("/scratch/events.json"))
    wanted = {(p["entityUrn"], p["aspectName"]): p["aspect"]["json"] for p in expected}
    result = {}
    deadline = time.monotonic() + 120
    while time.monotonic() < deadline:
        try:
            for (entity, name), value in wanted.items():
                actual = aspect(entity, name)
                # The server may add audit/system fields, but every projected
                # substantive metadata field must round-trip exactly.
                for key, expected_value in value.items():
                    if key != "lastModified" and not contains(actual.get(key), expected_value):
                        raise ValueError("catalog aspect mismatch")
                result.setdefault(entity, {})[name] = actual
            break
        except (urllib.error.URLError, KeyError, ValueError):
            time.sleep(2)
    else:
        raise ValueError("actual catalog readback did not converge")
    profile = "urn:li:dataset:(urn:li:dataPlatform:kanz,kanz.customer/PersonProfile,DEV)"
    exposure = "urn:li:dataset:(urn:li:dataPlatform:kanz,kanz.risk/CustomerExposure,DEV)"
    if not contains(result[profile]["upstreamLineage"]["upstreams"], [{"dataset": exposure, "type": "TRANSFORMED"}]) or not contains(result[exposure]["upstreamLineage"]["upstreams"], [{"dataset": profile, "type": "TRANSFORMED"}]):
        raise ValueError("actual catalog lost a legitimate cyclic dataset edge")
    try:
        request("/aspects/" + urllib.parse.quote(next(iter(result)), safe="") + "?aspect=ownership&version=0", authorization=False)
    except urllib.error.HTTPError as error:
        if error.code != 401:
            raise ValueError("unexpected unauthenticated catalog status") from None
    else:
        raise ValueError("catalog accepted unauthenticated read")
    # Catalog ownership/classification is deliberately altered through the same
    # real ingestion path. The next Go phase must still deny a lineage viewer.
    altered = []
    for proposal in expected:
        if proposal["aspectName"] == "ownership":
            proposal["aspect"]["json"]["owners"] = [{"owner": "urn:li:corpGroup:viewer", "type": "DATAOWNER"}]
            altered.append(proposal)
        if proposal["aspectName"] == "globalTags":
            proposal["aspect"]["json"]["tags"] = [{"tag": "urn:li:tag:PUBLIC"}]
            altered.append(proposal)
    Path("/scratch/altered.json").write_text(json.dumps(altered))
    Path("/scratch/readback.json").write_text(json.dumps({"datasets": result, "cases": ["distinct-stewards", "distinct-sensitivity", "schema-references", "lineage-edges", "cyclic-lineage-retained", "catalog-authentication"]}, sort_keys=True))


def altered_readback():
    environment, rows = mappings(load("/catalog/fixture_mapping.json"))
    for ds in rows:
        entity = urn(ds, environment)
        owners = aspect(entity, "ownership")["owners"]
        tags = aspect(entity, "globalTags")["tags"]
        if not contains(owners, [{"owner": "urn:li:corpGroup:viewer", "type": "DATAOWNER"}]) or not contains(tags, [{"tag": "urn:li:tag:PUBLIC"}]):
            raise ValueError("altered metadata did not reach actual catalog")
    path = Path("/scratch/readback.json")
    report = load(path)
    report["cases"].append("altered-catalog-metadata")
    path.write_text(json.dumps(report, sort_keys=True))


if __name__ == "__main__":
    import sys
    if sys.argv[1:] == ["readback"]:
        readback()
    elif sys.argv[1:] == ["altered"]:
        altered_readback()
    else:
        raise ValueError("unknown catalog client phase")
