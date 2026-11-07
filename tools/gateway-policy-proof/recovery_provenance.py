"""Verify the retained SSM command against original tracked bytes, without execution."""
import base64
import ast
import hashlib
import json
import re

import recovery_receipts as receipt
import transaction as tx
import transport_codec as codec

INSTANCE = "i-0ee1235614219f089"
FILES = ("transaction.py", "policy_client.py", "rollout_probe.py", "rollout_engine.py", "bounded_process.py", "source_loader.py")


def variants(text):
    lf = text.replace("\r\n", "\n")
    return {lf, lf.replace("\n", "\r\n")}


def verify(command, invocation, git_file, policies, workloads):
    """git_file reads OLD_SOURCE blobs; callers cannot supply an approval flag."""
    if (command.get("CommandId") != receipt.COMMAND or command.get("InstanceIds") != [INSTANCE]
            or command.get("DocumentName") != "AWS-RunShellScript" or command.get("Status") != "Failed"
            or command.get("Comment") != "Kanz four-policy qualification " + receipt.OLD_SOURCE
            or command.get("Targets") not in (None, [])):
        raise tx.Refusal("original SSM command identity differs")
    if (invocation.get("CommandId") != receipt.COMMAND or invocation.get("InstanceId") != INSTANCE
            or invocation.get("DocumentName") != "AWS-RunShellScript" or invocation.get("Status") != "Failed"):
        raise tx.Refusal("original SSM invocation identity differs")
    output = invocation.get("StandardOutputContent")
    if not isinstance(output, str) or len(output) > 4096:
        raise tx.Refusal("original SSM result exceeds bound")
    if receipt.document(output.strip().encode()) != {"source": receipt.OLD_SOURCE, "verdict": "REFUSED", "receipt": receipt.DIRECTORY}:
        raise tx.Refusal("original SSM receipt binding differs")
    parameters = command.get("Parameters", {})
    commands = parameters.get("commands")
    if (set(parameters) != {"executionTimeout", "commands"} or parameters["executionTimeout"] != ["1500"]
            or not isinstance(commands, list) or len(commands) != 9
            or any(not isinstance(line, str) for line in commands)
            or len(json.dumps(parameters).encode()) > 30000):
        raise tx.Refusal("original SSM command shape differs")
    first = re.fullmatch(r"printf '%s' '([A-Za-z0-9+/=]+)' \| base64 -d > \"\$work/bootstrap.py\"", commands[5])
    second = re.fullmatch(r"python3 -I -B \"\$work/bootstrap.py\" '([A-Za-z0-9+/=]+)' \"\$work\"", commands[6])
    if first is None or second is None:
        raise tx.Refusal("original transfer recipe differs")
    try:
        bootstrap = base64.b64decode(first[1], validate=True).decode()
    except (ValueError, UnicodeError):
        raise tx.Refusal("original bootstrap encoding differs") from None
    old_wrapper = git_file("tools/Invoke-TokyoGatewayPolicy.ps1")
    match = re.search(r"\$bootstrap = \$codec \+ \"`n\" \+ @'\r?\n(.*?)\r?\n'@", old_wrapper, re.S)
    if match is None:
        raise tx.Refusal("original tracked bootstrap unavailable")
    old_codec = git_file("tools/gateway-policy-proof/transport_codec.py")
    if bootstrap not in {a + "\n" + b for a in variants(old_codec) for b in variants(match[1])}:
        raise tx.Refusal("original bootstrap differs from tracked source")
    original = receipt.document(codec.decode(second[1]))
    if set(original) != {"files", "payload"} or set(original["files"]) != set(FILES):
        raise tx.Refusal("original bundle closure differs")
    hashes = {}
    for name in FILES:
        text = original["files"][name]
        if not isinstance(text, str) or len(text.encode()) > 65536 or text not in variants(git_file("tools/gateway-policy-proof/" + name)):
            raise tx.Refusal("original source bytes differ")
        hashes[name] = hashlib.sha256(text.encode()).hexdigest()
    expected = ["set -eu", "python3 -I -B -c 'import lzma; assert lzma.is_check_supported(lzma.CHECK_CRC64)'",
        "umask 077", "work=$(mktemp -d /var/tmp/kanz-gateway-source.XXXXXXXX)",
        'trap \'rm -f "$work/transaction.py" "$work/policy_client.py" "$work/rollout_probe.py" "$work/rollout_engine.py" "$work/bounded_process.py" "$work/source_loader.py" "$work/input.json" "$work/bootstrap.py"; rmdir "$work"\' EXIT',
        commands[5], commands[6], 'chmod 400 "$work"/*.py "$work/input.json"',
        'timeout --signal=TERM --kill-after=180s 1200s env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin HOME="$work" TMPDIR="$work" LANG=C.UTF-8 python3 -I -B "$work/source_loader.py" "$work" rollout_engine --tokyo --source ' + receipt.OLD_SOURCE + ' --apply < "$work/input.json"']
    if commands != expected:
        raise tx.Refusal("original command is not the exact apply recipe")
    payload = original["payload"]
    if (not isinstance(payload, dict) or set(payload) != {"source", "policies", "workloads", "identity"}
            or payload["source"] != receipt.OLD_SOURCE or payload["policies"] != policies or payload["workloads"] != workloads):
        raise tx.Refusal("original rendered scope differs from fresh canonical source")
    probe_source = ast.parse(git_file("tools/gateway-policy-proof/rollout_probe.py"))
    images = [node.value.value for node in probe_source.body if isinstance(node, ast.Assign)
              and len(node.targets) == 1 and isinstance(node.targets[0], ast.Name) and node.targets[0].id == "IMAGE"
              and isinstance(node.value, ast.Constant) and isinstance(node.value.value, str)]
    if len(images) != 1 or "@sha256:" not in images[0]:
        raise tx.Refusal("original probe image pin unavailable")
    expected_root = images[0].split("@", 1)[1]
    identity = payload["identity"]
    if (not isinstance(identity, dict) or set(identity) != {"root", "rootMediaType", "manifest", "config", "platform"}
            or identity["platform"] != "linux/amd64" or identity["root"] != expected_root
            or identity["rootMediaType"] != "application/vnd.oci.image.index.v1+json"
            or any(not re.fullmatch(r"sha256:[0-9a-f]{64}", identity.get(key, "")) for key in ("root", "manifest", "config"))):
        raise tx.Refusal("original image identity shape differs")
    return {"command": receipt.COMMAND, "instance": INSTANCE, "oldSource": receipt.OLD_SOURCE,
            "directory": receipt.DIRECTORY, "commandSHA256": tx.fingerprint(parameters),
            "sourceSHA256": tx.fingerprint(hashes), "bundleSHA256": hashlib.sha256(codec.decode(second[1])).hexdigest(),
            "originalPoliciesSHA256": tx.fingerprint(policies), "originalWorkloadsSHA256": tx.fingerprint(workloads)}
