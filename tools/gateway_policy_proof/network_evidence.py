"""Closed current network receipt validation; never extends legacy recovery."""
import json
import re

from network_observation import DENIALS, OUTCOMES


def sequence(isolated):
    result = []
    for app, api, ops in (("accounting", 8101, 8080), ("audit", 8102, 8083), ("compliance", 8095, 8091)):
        result.extend(((app, "gateway", api, "connected"), (app, "observer", ops, "connected")))
        for peer in (("untrusted", "bff", "observer", "outsider") if isolated else ()):
            result.extend(((app, "gateway", api, "connected"), (app, peer, api, "denied"),
                           (app, "gateway", api, "connected")))
    return result + [("api-gateway", "bff", 8080, "connected")]


def validate(record, nonce):
    if (not isinstance(record, dict) or set(record) != {"format", "nonce", "phase", "isolated", "observations"}
            or record["format"] != "kanz-gateway-network-v1" or record["nonce"] != nonce
            or not isinstance(nonce, str) or not re.fullmatch("[0-9a-f]{32}", nonce)
            or type(record["isolated"]) is not bool
            or record["phase"] not in ("network_verified", "network_refused")):
        raise ValueError("network receipt identity")
    items = record["observations"]
    expected = sequence(record["isolated"])
    if not isinstance(items, list) or not 0 <= len(items) <= min(48, len(expected)):
        raise ValueError("network receipt item bound")
    for item, identity in zip(items, expected):
        if (not isinstance(item, dict) or set(item) != {"target", "peer", "port", "expected", "batches", "result"}
                or type(item["port"]) is not int
                or tuple(item[k] for k in ("target", "peer", "port", "expected")) != identity
                or item["result"] not in ("unknown", "verified", "not_converged")):
            raise ValueError("network receipt target or result")
        batches = item["batches"]
        if not isinstance(batches, list) or len(batches) > 6:
            raise ValueError("network receipt batch bound")
        consecutive, unknown = 0, False
        for batch in batches:
            if not isinstance(batch, list) or len(batch) != 3:
                raise ValueError("network receipt observation bound")
            for value in batch:
                if not isinstance(value, str) or value not in OUTCOMES:
                    raise ValueError("network receipt unclassified value")
                unknown |= value == "unknown"
                accepted = value == "connected" if item["expected"] == "connected" else value in DENIALS
                consecutive = consecutive + 1 if accepted else 0
        if item["result"] == "verified" and (unknown or consecutive < 3):
            raise ValueError("network receipt false verification")
    if record["phase"] == "network_verified" and (len(items) != len(expected) or any(item["result"] != "verified" for item in items)):
        raise ValueError("network receipt incomplete verification")
    if len(json.dumps(record).encode()) > 2 * 1024 * 1024:
        raise ValueError("network receipt serialization bound")
