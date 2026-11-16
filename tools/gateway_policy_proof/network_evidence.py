"""Closed current network receipt validation; never extends legacy recovery."""
import json
import re

from network_observation import DENIALS, OUTCOMES


def _integer(value, low, high):
    return type(value) is int and low <= value <= high


def _process(value):
    fields = {"outcome", "timeoutUs", "elapsedUs", "stdoutBytes", "stderrBytes",
              "stdoutLimitBytes", "stderrLimitBytes", "launched", "killRequested",
              "reaped", "joinedStreams", "cleanupFailed", "exitCode", "signal"}
    if (not isinstance(value, dict) or set(value) != fields
            or type(value["outcome"]) is not str
            or value["outcome"] not in {"launch_error", "deadline", "nonzero", "stdout_limit",
                                        "stderr_limit", "stream_error", "stream_join_timeout", "failure"}
            or not _integer(value["timeoutUs"], 1, 100000000)
            or not _integer(value["elapsedUs"], 0, 480000000)
            or value["stdoutLimitBytes"] != 4 << 20 or type(value["stdoutLimitBytes"]) is not int
            or value["stderrLimitBytes"] != 65536 or type(value["stderrLimitBytes"]) is not int
            or not _integer(value["stdoutBytes"], 0, (4 << 20) + 65536)
            or not _integer(value["stderrBytes"], 0, 131072)
            or not _integer(value["joinedStreams"], 0, 3)
            or any(type(value[key]) is not bool for key in ("launched", "killRequested", "reaped", "cleanupFailed"))):
        raise ValueError("network command process projection")
    code, sig = value["exitCode"], value["signal"]
    if ((code is not None and not _integer(code, 0, 255))
            or (sig is not None and not _integer(sig, 1, 64))
            or (code is not None and sig is not None)
            or (value["reaped"] and code is None and sig is None)
            or (not value["reaped"] and (code is not None or sig is not None))):
        raise ValueError("network command exit projection")
    if not value["launched"]:
        if (value["outcome"] != "launch_error" or value["killRequested"] or value["reaped"]
                or value["joinedStreams"] or value["stdoutBytes"] or value["stderrBytes"] or value["cleanupFailed"]):
            raise ValueError("network command launch projection")
    elif value["outcome"] == "launch_error":
        raise ValueError("network command launch projection")
    if value["outcome"] == "deadline" and (not value["killRequested"] or value["elapsedUs"] < value["timeoutUs"]):
        raise ValueError("network command deadline projection")
    if value["outcome"] == "nonzero" and (not value["reaped"] or code == 0):
        raise ValueError("network command nonzero projection")
    if value["outcome"] == "stdout_limit" and value["stdoutBytes"] <= value["stdoutLimitBytes"]:
        raise ValueError("network command byte projection")
    if value["outcome"] == "stderr_limit" and value["stderrBytes"] <= value["stderrLimitBytes"]:
        raise ValueError("network command byte projection")
    if value["outcome"] == "stream_join_timeout" and value["joinedStreams"] == 3 and not value["cleanupFailed"]:
        raise ValueError("network command stream projection")


def _command_failure(value, item):
    fields = {"phase", "kind", "attempt", "verificationElapsedUs", "lifetimeRemainingUs",
              "activeRemainingUs", "observationRemainingUs", "inventoryElapsedUs", "requestedTimeoutUs", "process"}
    if (not isinstance(value, dict) or set(value) != fields
            or type(value["phase"]) is not str or value["phase"] not in {"inventory", "tcp_exec"}
            or type(value["kind"]) is not str or value["kind"] not in {"command", "insufficient_batch_budget"}
            or not _integer(value["attempt"], 1, 6) or value["attempt"] != len(item["batches"]) + 1
            or item["result"] != "unknown"
            or not _integer(value["verificationElapsedUs"], 0, 420000000)
            or not _integer(value["lifetimeRemainingUs"], 0, 420000000)
            or value["verificationElapsedUs"] + value["lifetimeRemainingUs"] > 420000000
            or not _integer(value["activeRemainingUs"], 0, min(180000000, value["lifetimeRemainingUs"]))
            or not _integer(value["observationRemainingUs"], 0, min(20000000, value["activeRemainingUs"]))
            or not _integer(value["inventoryElapsedUs"], 0, 480000000)
            or not _integer(value["requestedTimeoutUs"], 0, 100000000)):
        raise ValueError("network command timing projection")
    if value["kind"] == "insufficient_batch_budget":
        if (value["phase"] != "tcp_exec" or value["observationRemainingUs"] >= 400000
                or value["requestedTimeoutUs"] != 0 or value["process"] is not None):
            raise ValueError("network command insufficient budget projection")
        return
    _process(value["process"])
    bound = min(10000000, value["observationRemainingUs"]) if value["phase"] == "tcp_exec" else min(100000000, value["activeRemainingUs"])
    if (not 0 < value["requestedTimeoutUs"] <= bound
            or value["process"]["timeoutUs"] > value["requestedTimeoutUs"]
            or (value["phase"] == "tcp_exec" and value["observationRemainingUs"] < 400000)):
        raise ValueError("network command granted budget projection")


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
        fields = {"target", "peer", "port", "expected", "batches", "result"}
        if (not isinstance(item, dict) or set(item) not in (fields, fields | {"commandFailure"})
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
        if "commandFailure" in item:
            if record["phase"] != "network_refused":
                raise ValueError("network command diagnostics cannot verify a record")
            _command_failure(item["commandFailure"], item)
    if record["phase"] == "network_verified" and (len(items) != len(expected) or any(item["result"] != "verified" for item in items)):
        raise ValueError("network receipt incomplete verification")
    if len(json.dumps(record).encode()) > 2 * 1024 * 1024:
        raise ValueError("network receipt serialization bound")
