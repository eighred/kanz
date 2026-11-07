import base64
import copy
import json
import unittest

import recovery_provenance as p
import recovery_receipts as r
import transaction as tx
import transport_codec as codec


def fixture():
    policies, workloads = [], {}
    files = {"tools/gateway-policy-proof/" + name: "# original tracked " + name + "\n" for name in p.FILES}
    files["tools/gateway-policy-proof/rollout_probe.py"] += 'IMAGE = "python@sha256:' + "1" * 64 + '"\n'
    files["tools/gateway-policy-proof/transport_codec.py"] = "# original checked codec\n"
    body = "# original exact bootstrap\npass"
    files["tools/Invoke-TokyoGatewayPolicy.ps1"] = '$bootstrap = $codec + "`n" + @\'\n' + body + "\n'@"
    bootstrap = files["tools/gateway-policy-proof/transport_codec.py"] + "\n" + body
    bundle = {"files": {name: files["tools/gateway-policy-proof/" + name] for name in p.FILES},
        "payload": {"source": r.OLD_SOURCE, "policies": policies, "workloads": workloads,
            "identity": {"root": "sha256:" + "1" * 64, "manifest": "sha256:" + "2" * 64,
                         "config": "sha256:" + "3" * 64, "rootMediaType": "application/vnd.oci.image.index.v1+json", "platform": "linux/amd64"}}}
    encoded = codec.encode(json.dumps(bundle).encode())
    boot = base64.b64encode(bootstrap.encode()).decode()
    commands = ["set -eu", "python3 -I -B -c 'import lzma; assert lzma.is_check_supported(lzma.CHECK_CRC64)'",
        "umask 077", "work=$(mktemp -d /var/tmp/kanz-gateway-source.XXXXXXXX)",
        'trap \'rm -f "$work/transaction.py" "$work/policy_client.py" "$work/rollout_probe.py" "$work/rollout_engine.py" "$work/bounded_process.py" "$work/source_loader.py" "$work/input.json" "$work/bootstrap.py"; rmdir "$work"\' EXIT',
        "printf '%s' '" + boot + "' | base64 -d > \"$work/bootstrap.py\"",
        'python3 -I -B "$work/bootstrap.py" \'' + encoded + '\' "$work"',
        'chmod 400 "$work"/*.py "$work/input.json"',
        'timeout --signal=TERM --kill-after=180s 1200s env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin HOME="$work" TMPDIR="$work" LANG=C.UTF-8 python3 -I -B "$work/source_loader.py" "$work" rollout_engine --tokyo --source ' + r.OLD_SOURCE + ' --apply < "$work/input.json"']
    command = {"CommandId": r.COMMAND, "InstanceIds": [p.INSTANCE], "DocumentName": "AWS-RunShellScript",
        "Status": "Failed", "Comment": "Kanz four-policy qualification " + r.OLD_SOURCE,
        "Parameters": {"commands": commands, "executionTimeout": ["1500"]}}
    invocation = {"CommandId": r.COMMAND, "InstanceId": p.INSTANCE, "DocumentName": "AWS-RunShellScript", "Status": "Failed",
        "StandardOutputContent": json.dumps({"source": r.OLD_SOURCE, "verdict": "REFUSED", "receipt": r.DIRECTORY})}
    return command, invocation, files, policies, workloads


class ProvenanceTests(unittest.TestCase):
    def test_exact_command_git_and_bundle_binding(self):
        command, invocation, files, policies, workloads = fixture()
        result = p.verify(command, invocation, files.__getitem__, policies, workloads)
        self.assertEqual(result["oldSource"], r.OLD_SOURCE)
        self.assertEqual(result["command"], r.COMMAND)
        self.assertNotIn("Parameters", result)
        self.assertNotIn("files", result)
        self.assertLess(len(json.dumps(result)), 1024)

    def test_no_caller_boolean_or_substituted_old_source(self):
        for mode in ("instance", "command", "document", "status", "receipt", "shell", "timeout", "bootstrap", "git", "projection", "extra_parameter"):
            command, invocation, files, policies, workloads = fixture()
            with self.subTest(mode=mode), self.assertRaises(tx.Refusal):
                if mode == "instance": command["InstanceIds"].append("another-instance")
                if mode == "command": invocation["CommandId"] = "other"
                if mode == "document": command["DocumentName"] = "other"
                if mode == "status": command["Status"] = "Success"
                if mode == "receipt": invocation["StandardOutputContent"] = '{"verified":true}'
                if mode == "shell": command["Parameters"]["commands"][-1] += "; true"
                if mode == "timeout": command["Parameters"]["executionTimeout"] = ["1600"]
                if mode == "bootstrap": command["Parameters"]["commands"][5] = "printf '%s' 'cGFzcw==' | base64 -d > \"$work/bootstrap.py\""
                if mode == "git": files["tools/gateway-policy-proof/transaction.py"] += "print('substitute')\n"
                if mode == "projection": policies.append({"changed": True})
                if mode == "extra_parameter": command["Parameters"]["verified"] = ["true"]
                p.verify(command, invocation, files.__getitem__, policies, workloads)


if __name__ == "__main__":
    unittest.main()
