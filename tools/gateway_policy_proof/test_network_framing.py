"""Exercise the actual checked wrappers without AWS or operator entrypoints."""
import ast
import base64
import hashlib
import json
import lzma
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
import copy
import transport_codec as codec
import yaml
from unittest.mock import patch

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
WRAPPERS = {"normal": "Invoke-TokyoGatewayPolicy.ps1", "plan": "Invoke-TokyoGatewayPolicyRecoveryPlan.ps1",
            "restore": "Invoke-TokyoGatewayPolicyRetainedRestore.ps1"}


def source_inventory(wrapper):
    mode = re.search(r"^\$transportMode = '(normal|plan|restore)'$", wrapper, re.M).group(1)
    return mode, codec.SOURCE_BUNDLES[mode]


def names(wrapper):
    _, inventory = source_inventory(wrapper)
    return set(inventory["files"]), set(inventory["fixed"])


def valid_framing(wrapper, expected):
    try:
        roots = re.findall(r"^\$proofRoot = Join-Path \$PSScriptRoot '([^'\r\n]+)'\r?$", wrapper, re.M)
        if roots != ["gateway_policy_proof"]:
            return False
        mode, inventory = source_inventory(wrapper)
        files, fixed = names(wrapper)
        markers = (
            "$inventoryLines = [regex]::Matches($codec, '(?m)^SOURCE_BUNDLES = (\\{[^\\r\\n]+\\})\\r?$')",
            "$inventoryLines.Count -ne 1", "ConvertFrom-Json).PSObject.Properties[$transportMode].Value",
            "$sourceNames = @($inventory.files)", "$fixedNames = @($inventory.fixed)",
            "foreach ($name in $sourceNames)", "$sourceNames.Count -gt 16", "$fixedNames.Count -gt 2",
            "$allNames | Sort-Object -Unique", "$_ -isnot [string]", "^[a-z_]+\\.py$",
            "$info.LinkType", "$info.PSIsContainer", "$info.Length -gt 65536", "$raw.Length -gt 65536",
            "ls-files --error-unmatch $relative", "sys.stdin.buffer.read(262145)", "len(raw)>262144",
            "$fixedFrame.PSObject.Properties.Name", "decoder,encoded,expanded",
            "$fixedFrame.decoder -isnot [string]", "$fixedFrame.decoder.Length -lt 1",
            "[Text.Encoding]::UTF8.GetByteCount($fixedFrame.decoder) -gt 65536",
            "[ordered]@{ codec = $codec; files = $fixed }", "$decoder = [string]$fixedFrame.decoder",
            "$bootstrap = $decoder +", "$fixedFrame.encoded -isnot [string]",
            "$fixedFrame.encoded.Length -gt 20000", "^[A-Za-z0-9+/]+={0,2}$",
            "$fixedFrame.expanded -isnot [int]", "$fixedFrame.expanded -isnot [long]",
            "$fixedFrame.expanded -lt 1", "$fixedFrame.expanded -gt 524288",
            "$fixedSize = [long]$fixedFrame.expanded", "($bytes.Length + $fixedSize) -gt 524288",
            "len(sys.argv)!=4", "len(raw)+len(fixedRaw)>524288", "len(data)>65536",
            "'$encoded' '$fixedEncoded'", "root=pathlib.Path(sys.argv[3])",
            "$encoded.Length -gt 20000", "$parameterBytes -gt 30000", "$parameters.Length -gt 30000",
            "with (root/name).open('xb')", "-I -B -c $literalBuilder")
        return (fixed == expected and files.isdisjoint(fixed)
                and "encode_fixed(values,'" + mode + "')" in wrapper
                and "decode_fixed(sys.argv[2],'" + mode + "')" in wrapper
                and "remote_decoder_source(packet['codec'],'" + mode + "')" in wrapper
                and "b=json.loads(raw.decode('utf-8',errors='strict'),object_pairs_hook=_unique_fixed); names=set(SOURCE_BUNDLES['" + mode + "']['files'])" in wrapper
                and all(value in wrapper for value in markers)
                and wrapper.index("$codecPath =") < wrapper.index("$files =")
                and wrapper.index("prepared[name]=data") < wrapper.index("with (root/name).open('xb')"))
    except (ValueError, AttributeError, SyntaxError, KeyError):
        return False


def declaration(wrapper, fixed):
    builder = re.search(r"\$literalBuilder = \$codec \+ \"`n\" \+ @'\n(.*?)\n'@", wrapper, re.S).group(1)
    raw = json.dumps({"codec": (HERE / "transport_codec.py").read_text(encoding="utf-8"),
                      "files": {name: base64.b64encode(data).decode() for name, data in fixed.items()}}).encode()
    result = subprocess.run([sys.executable, "-I", "-B", "-c", (HERE / "transport_codec.py").read_text(encoding="utf-8") + "\n" + builder], input=raw,
                            capture_output=True, timeout=10, check=True)
    return json.loads(result.stdout.decode("utf-8"))


def bootstrap(wrapper):
    body = re.search(r"\$bootstrap = \$decoder \+ \"`n\" \+ @'\n(.*?)\n'@", wrapper, re.S).group(1)
    mode, _ = source_inventory(wrapper)
    source = (HERE / "transport_codec.py").read_text(encoding="utf-8")
    return codec.remote_decoder_source(source, mode) + "\n" + body


class NetworkFramingTests(unittest.TestCase):
    def test_geometry_steps_are_mandatory_and_enabled(self):
        workflow = yaml.safe_load((ROOT / ".github/workflows/gateway-policy-proof.yml").read_text(encoding="utf-8"))
        commands = {
            "python3 -B -m unittest test_network_framing.NetworkFramingTests.test_actual_wrappers_all_96_source_variants": "tools/gateway_policy_proof",
            "python3 -B -m unittest test_transfer.CNITransferTests.test_actual_powershell_32_cni_variants": "tools/cni_proof",
        }
        job = workflow["jobs"]["gateway-isolation"]
        def check(value):
            if value.get("if") or value.get("continue-on-error"):
                raise ValueError("geometry job bypass")
            for command, directory in commands.items():
                steps = [step for step in value["steps"] if step.get("run") == command]
                if len(steps) != 1:
                    raise ValueError("geometry command missing")
                step = steps[0]
                if (step.get("if") or step.get("continue-on-error") or step.get("working-directory") != directory
                        or step.get("env", {}).get("KANZ_REQUIRE_NETWORK_FRAMING") != "1"):
                    raise ValueError("geometry measurement skipped")
        check(job)
        for command in commands:
            for change in ("missing", "if", "continue-on-error", "env", "working-directory"):
                bad = copy.deepcopy(job)
                step = next(step for step in bad["steps"] if step.get("run") == command)
                if change == "missing": bad["steps"].remove(step)
                elif change == "env": step["env"] = {}
                elif change == "working-directory": step[change] = "elsewhere"
                else: step[change] = "false" if change == "if" else True
                with self.subTest(command=command, change=change), self.assertRaises(ValueError): check(bad)

    def test_closed_actual_wrapper_recipes(self):
        self.assertEqual(codec.SOURCE_BUNDLES, {'normal': {'files': ['policy_state.py', 'transaction.py', 'policy_client.py', 'rollout_probe.py', 'rollout_scope.py', 'rollout_engine.py', 'network_observation.py'], 'fixed': ['source_loader.py', 'bounded_process.py']}, 'plan': {'files': ['policy_state.py', 'policy_client.py', 'rollout_scope.py', 'recovery_receipts.py', 'recovery_plan.py', 'bounded_process.py'], 'fixed': ['source_loader.py']}, 'restore': {'files': ['policy_state.py', 'policy_client.py', 'rollout_scope.py', 'recovery_receipts.py', 'recovery_plan.py', 'recovery_restore.py', 'bounded_process.py'], 'fixed': ['source_loader.py']}})
        builders = []
        for mode, name in WRAPPERS.items():
            wrapper = (ROOT / "tools" / name).read_text(encoding="utf-8")
            expected = {"source_loader.py"} | ({"bounded_process.py"} if mode == "normal" else set())
            self.assertTrue(valid_framing(wrapper, expected))
            canonical_root = "$proofRoot = Join-Path $PSScriptRoot 'gateway_policy_proof'"
            legacy_root = "$proofRoot = Join-Path $PSScriptRoot 'gateway-policy-proof'"
            self.assertEqual(wrapper.count(canonical_root), 1)
            self.assertFalse(valid_framing(wrapper.replace(canonical_root, legacy_root), expected))
            source_root = ROOT / "tools" / "gateway_policy_proof"
            self.assertEqual(source_root, HERE)
            self.assertTrue(source_root.is_dir())
            _, fixed_names = names(wrapper)
            for source_name in sorted(fixed_names):
                self.assertTrue((source_root / source_name).is_file(), source_name)
                self.assertFalse((source_root / source_name).is_symlink(), source_name)
            builder = re.search(r"\$literalBuilder = \$codec \+ \"`n\" \+ @'\n(.*?)\n'@", wrapper, re.S).group(1)
            builders.append(re.sub(r"\((values|packet\['codec'\]),'[^']+'\)", r"(\1,MODE)", builder))
            for marker in ("$info.LinkType", "len(raw)+len(fixedRaw)>524288", "len(sys.argv)!=4",
                           "($bytes.Length + $fixedSize) -gt 524288", "with (root/name).open('xb')",
                           "$parameters.Length -gt 30000", "-I -B -c $literalBuilder",
                           "$sourceNames = @($inventory.files)", "$fixedNames = @($inventory.fixed)",
                           "foreach ($name in $sourceNames)", "$inventoryLines.Count -ne 1",
                           "ConvertFrom-Json).PSObject.Properties[$transportMode].Value", "$_ -isnot [string]",
                           "$fixedFrame.encoded.Length -gt 20000", "$fixedFrame.expanded -isnot [long]",
                           "'$encoded' '$fixedEncoded'", "root=pathlib.Path(sys.argv[3])",
                           "[ordered]@{ codec = $codec; files = $fixed }", "$bootstrap = $decoder +",
                           "$fixedFrame.decoder -isnot [string]", "$decoder = [string]$fixedFrame.decoder"):
                self.assertFalse(valid_framing(wrapper.replace(marker, "removed"), expected), marker)
        self.assertEqual(len(set(builders)), 1)  # Same semantics; only fixed inventory differs.

    def test_exact_bootstrap_thirteen_controls(self):
        wrapper = (ROOT / "tools" / WRAPPERS["normal"]).read_text(encoding="utf-8")
        files, fixed_names = names(wrapper)
        fixed = {name: (HERE / name).read_bytes() for name in source_inventory(wrapper)[1]["fixed"]}
        source = bootstrap(wrapper)
        framed = declaration(wrapper, fixed)
        self.assertEqual(set(framed), {"decoder", "encoded", "expanded"})
        self.assertTrue(source.startswith(framed["decoder"] + "\n"))
        fixed_raw, original = codec.decode_fixed(framed["encoded"], "normal")
        self.assertEqual(framed["expanded"], len(fixed_raw))
        self.assertEqual(original, fixed)
        frame = json.loads(fixed_raw)
        bundle = {"files": {name: "# exact UTF8 λ\r\n" for name in files}, "payload": {"source": "a" * 40}}
        def packed(raw, check=lzma.CHECK_CRC64, dictionary=1048576):
            return base64.b64encode(lzma.compress(raw, format=lzma.FORMAT_XZ, check=check,
                filters=[{"id": lzma.FILTER_LZMA2, "dict_size": dictionary}])).decode()
        def run(value=bundle, encoded=framed["encoded"], expected=original, collision=None, good=False, argc=4, main_raw=None):
            with tempfile.TemporaryDirectory() as directory:
                path = Path(directory)
                if collision: (path / collision).write_text("preserved", encoding="utf-8")
                raw = json.dumps(value).encode() if main_raw is None else main_raw
                arguments = ["bootstrap", codec.encode(raw), encoded, directory]
                if argc == 3: arguments.pop(2)
                if argc == 5: arguments.append("extra")
                with patch.object(sys, "argv", arguments):
                    try: exec(compile(source, "<actual-wrapper-bootstrap>", "exec"), {})
                    except (ValueError, lzma.LZMAError, FileExistsError) as error:
                        self.assertFalse(good)
                        if argc != 4: self.assertEqual(str(error), "bootstrap argument count")
                        if collision:
                            # xb protects this leaf; earlier owned writes may exist.
                            # This does not qualify the operator EXIT trap's cleanup.
                            self.assertEqual((path / collision).read_text(), "preserved")
                        else: self.assertEqual(list(path.iterdir()), [])
                    else:
                        self.assertTrue(good)
                        self.assertEqual({p.name for p in path.iterdir()}, files | fixed_names | {"input.json"})
                        for name, data in expected.items(): self.assertEqual((path / name).read_bytes(), data)
                        for name, text in value["files"].items(): self.assertEqual((path / name).read_bytes(), text.encode())
        run(good=True)
        for name in original:
            bad = copy.deepcopy(frame); bad["files"][name]["sha256"] = "0" * 64
            run(encoded=packed(json.dumps(bad).encode()))
            run(collision=name)
            text = "x" * 65537
            bad["files"][name] = {"text": text, "sha256": hashlib.sha256(text.encode()).hexdigest()}
            run(encoded=packed(json.dumps(bad).encode()))
        for mode in ("missing", "extra", "duplicate", "oversize", "combined"):
            value = copy.deepcopy(bundle); fixed_value = copy.deepcopy(frame)
            if mode == "missing": del fixed_value["files"]["bounded_process.py"]
            elif mode == "extra": fixed_value["files"]["evil.py"] = {"text": "", "sha256": hashlib.sha256(b"").hexdigest()}
            elif mode == "duplicate": value["files"]["source_loader.py"] = "substitute"
            elif mode == "oversize": value["files"]["rollout_engine.py"] = "x" * 65537
            else: value["payload"]["pad"] = "x" * (524000 - len(json.dumps(value)))
            run(value, encoded=packed(json.dumps(fixed_value).encode()))
        unicode_sources = {name: data + "# UTF8 λ ' \\\r\n".encode() for name, data in original.items()}
        unicode_frame = declaration(wrapper, unicode_sources)
        run(encoded=unicode_frame["encoded"], expected=unicode_sources, good=True)
        for control in ("corrupt", "truncated", "trailing", "concatenated", "none", "dictionary", "expanded",
                        "encoded", "invalid_utf8", "utf16", "duplicate_field", "duplicate_name", "duplicate_record", "frame_type", "frame_extra", "files_type",
                        "entry_type", "entry_extra", "missing_hash", "text_type", "hash_type", "surrogate"):
            bad = copy.deepcopy(frame)
            compressed = base64.b64decode(framed["encoded"])
            if control == "corrupt": encoded = base64.b64encode(b"invalid").decode()
            elif control == "truncated": encoded = base64.b64encode(compressed[:-1]).decode()
            elif control == "trailing": encoded = base64.b64encode(compressed + b"x").decode()
            elif control == "concatenated": encoded = base64.b64encode(compressed + compressed).decode()
            elif control == "none": encoded = packed(fixed_raw, check=lzma.CHECK_NONE)
            elif control == "dictionary": encoded = packed(fixed_raw, dictionary=67108864)
            elif control == "expanded": encoded = packed(b"x" * 524289)
            elif control == "encoded": encoded = "A" * 20001
            elif control == "invalid_utf8": encoded = packed(b"\xff")
            elif control == "utf16": encoded = packed(json.dumps(frame).encode("utf-16"))
            elif control == "duplicate_field":
                value = json.dumps(frame["files"])
                encoded = packed(('{"files":' + value + ',"files":' + value + '}').encode())
            elif control == "duplicate_name":
                name = next(iter(frame["files"])); key = json.dumps(name); value = json.dumps(frame["files"][name])
                rest = {key: value for key, value in frame["files"].items() if key != name}
                encoded = packed(('{"files":{' + key + ':' + value + ',' + key + ':' + value + ',' + json.dumps(rest)[1:] + '}').encode())
            elif control == "duplicate_record":
                name = next(iter(frame["files"])); value = json.dumps(frame["files"][name]["text"])
                text = json.dumps(frame)
                once = '"text": ' + value
                encoded = packed(text.replace(once, once + ',' + once, 1).encode())
            else:
                name = next(iter(bad["files"]))
                if control == "frame_type": bad = []
                elif control == "frame_extra": bad["extra"] = True
                elif control == "files_type": bad["files"] = []
                elif control == "entry_type": bad["files"][name] = []
                elif control == "entry_extra": bad["files"][name]["extra"] = True
                elif control == "missing_hash": del bad["files"][name]["sha256"]
                elif control == "text_type": bad["files"][name]["text"] = None
                elif control == "hash_type": bad["files"][name]["sha256"] = True
                else: bad["files"][name]["text"] = "\ud800"
                encoded = packed(json.dumps(bad).encode())
            with self.subTest(fixed_control=control): run(encoded=encoded)
        for control in ("duplicate_files", "duplicate_payload", "duplicate_name", "utf16", "utf32", "invalid_utf8", "surrogate"):
            if control == "duplicate_files":
                files_json = json.dumps(bundle["files"]); payload_json = json.dumps(bundle["payload"])
                raw = ('{"files":' + files_json + ',"files":' + files_json + ',"payload":' + payload_json + '}').encode()
            elif control == "duplicate_payload":
                files_json = json.dumps(bundle["files"]); payload_json = json.dumps(bundle["payload"])
                raw = ('{"files":' + files_json + ',"payload":' + payload_json + ',"payload":' + payload_json + '}').encode()
            elif control == "duplicate_name":
                name = next(iter(bundle["files"])); key = json.dumps(name); value = json.dumps(bundle["files"][name])
                rest = {key: value for key, value in bundle["files"].items() if key != name}
                raw = ('{"files":{' + key + ':' + value + ',' + key + ':' + value + ','
                       + json.dumps(rest)[1:] + ',"payload":' + json.dumps(bundle["payload"]) + '}').encode()
            elif control == "utf16": raw = json.dumps(bundle).encode("utf-16")
            elif control == "utf32": raw = json.dumps(bundle).encode("utf-32")
            elif control == "invalid_utf8": raw = b"\xff"
            else:
                bad = copy.deepcopy(bundle)
                bad["files"][next(iter(bad["files"]))] = "\ud800"
                raw = json.dumps(bad).encode()
            if control.startswith("duplicate_"):
                # These are valid JSON that the old last-key-wins parser accepts.
                self.assertEqual(json.loads(raw), bundle)
            with self.subTest(main_control=control): run(main_raw=raw)
        for argc in (3, 5):
            with self.subTest(argc=argc): run(argc=argc)

    def test_builder_rejects_invalid_utf8_before_literal(self):
        wrapper = (ROOT / "tools" / WRAPPERS["plan"]).read_text(encoding="utf-8")
        with self.assertRaises(subprocess.CalledProcessError):
            declaration(wrapper, {"source_loader.py": b"\xff"})

    def test_actual_checkout_geometry_is_mandatory(self):
        source = __import__("inspect").getsource(type(self).test_actual_wrappers_all_96_source_variants)
        markers = ['["git", "rev-parse", "HEAD"]', 'for variant in [*range(16), "checkout"]:',
                   'checkout if variant == "checkout" else digest("source")[:40]', 'self.assertEqual(counts, 102)']
        def valid(value): return all(marker in value for marker in markers)
        self.assertTrue(valid(source))
        for marker in markers:
            self.assertFalse(valid(source.replace(marker, "removed")))

    @unittest.skipUnless(os.environ.get("KANZ_REQUIRE_NETWORK_FRAMING") == "1", "dedicated actual PowerShell geometry gate")
    def test_actual_wrappers_all_96_source_variants(self):
        import yaml
        pwsh = shutil.which("pwsh")
        kubectl = shutil.which("kubectl")
        self.assertIsNotNone(pwsh, "mandatory PowerShell measurement unavailable")
        self.assertIsNotNone(kubectl, "mandatory rendered source unavailable")
        raw = subprocess.check_output([kubectl, "kustomize", "--load-restrictor=LoadRestrictionsNone",
                                       str(ROOT / "kanz/infra/overlays/testnet_tokyo")], cwd=ROOT, timeout=30)
        docs = [value for value in yaml.safe_load_all(raw) if value]
        policies = [value for value in docs if value.get("kind") == "NetworkPolicy"]
        workloads = {}
        for value in docs:
            if value.get("kind") == "Deployment" and value["metadata"]["name"] in {"accounting", "audit", "compliance", "api-gateway", "web-bff"}:
                template = value["spec"]["template"]
                workloads[value["metadata"]["name"]] = {"labels": template["metadata"]["labels"], "selector": value["spec"]["selector"],
                    "images": {kind: {c["name"]: c["image"] for c in template["spec"].get(kind, [])} for kind in ("containers", "initContainers")}}
        self.assertEqual(len(workloads), 5)
        self.assertEqual(len(policies), 11)
        def quoted(value): return "'" + str(value).replace("'", "''") + "'"
        checkout = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True, timeout=10).strip()
        self.assertRegex(checkout, r"^[0-9a-f]{40}$")
        counts = 0
        for mode, filename in WRAPPERS.items():
            wrapper = (ROOT / "tools" / filename).read_text(encoding="utf-8")
            files, fixed_names = names(wrapper)
            stage = (wrapper[wrapper.index("$codecPath ="):wrapper.index("$files =")]
                     + wrapper[wrapper.index("$bundle ="):wrapper.index("$latest =")])
            maxima = {key: 0 for key in ("encoded", "expanded", "parameters", "characters")}
            for ending in ("\n", "\r\n"):
                # Preserve all 96 synthetic variants and add six actual-checkout cases.
                for variant in [*range(16), "checkout"]:
                    def digest(label): return hashlib.sha256((str(variant) + label).encode()).hexdigest()
                    ordered_files = source_inventory(wrapper)[1]["files"]
                    source_files = {name: (HERE / name).read_text(encoding="utf-8").replace("\n", ending) for name in ordered_files}
                    fixed = {name: (HERE / name).read_text(encoding="utf-8").replace("\n", ending).encode() for name in source_inventory(wrapper)[1]["fixed"]}
                    fixed_frame = declaration(wrapper, fixed)
                    from rollout_probe import IMAGE
                    identity = {"root": IMAGE.split("@")[1], "manifest": IMAGE.split("@")[1],
                                "rootMediaType": "application/vnd.oci.image.manifest.v1+json", "platform": "linux/amd64",
                                "config": "sha256:4b47a20ba2e7896b29f6a8400bcbfaf2c4f5baec4a18d3d445e11b03a6bf932f"}
                    provenance = {"command": "21d730ab-18c9-43ba-9427-522d557b6f2c", "instance": "i-0ee1235614219f089",
                        "oldSource": "0769d36cf857993284109d03868393a0c7e0d7c8", "directory": "/var/tmp/kanz-gateway-policy-nu1ezqwo",
                        **{key: digest(key) for key in ("commandSHA256", "sourceSHA256", "bundleSHA256", "originalPoliciesSHA256", "originalWorkloadsSHA256")}}
                    data = {"files": source_files, "source": checkout if variant == "checkout" else digest("source")[:40], "policies": policies, "workloads": workloads,
                            "identity": identity, "provenance": provenance, "fixedFrame": fixed_frame}
                    with tempfile.TemporaryDirectory() as directory:
                        inputs = Path(directory) / "inputs.json"
                        inputs.write_text(json.dumps(data), encoding="utf-8")
                        prefix = "$ErrorActionPreference='Stop'\n$repo=" + quoted(ROOT) + "\n$proofRoot=" + quoted(HERE) + "\n$python=" + quoted(sys.executable) + "\n$Apply=$true\n"
                        prefix += "$d=Get-Content -Raw -Encoding utf8 -LiteralPath " + quoted(inputs) + " | ConvertFrom-Json -AsHashtable\n"
                        prefix += "$files=$d.files;$source=$d.source;$policies=$d.policies;$identity=$d.identity;$provenance=$d.provenance;$projection=@{workloads=$d.workloads};$decoder=$d.fixedFrame.decoder;$fixedEncoded=$d.fixedFrame.encoded;$fixedSize=$d.fixedFrame.expanded\n"
                        suffix = "\n@{encoded=$encoded.Length;expanded=($bytes.Length+$fixedSize);parameters=$parameterBytes;characters=$parameters.Length}|ConvertTo-Json -Compress\n"
                        script = Path(directory) / "measure.ps1"
                        script.write_text(prefix + stage + suffix, encoding="utf-8")
                        result = subprocess.run([pwsh, "-NoProfile", "-File", str(script)], capture_output=True, text=True, timeout=30)
                        self.assertEqual(result.returncode, 0, "actual wrapper refused geometry; no operator was executed")
                        sizes = json.loads(result.stdout.strip().splitlines()[-1])
                        for key, limit in (("encoded", 20000), ("expanded", 524288), ("parameters", 30000), ("characters", 30000)):
                            self.assertLessEqual(sizes[key], limit)
                            maxima[key] = max(maxima[key], sizes[key])
                    counts += 1
            print(json.dumps({"wrapper": mode, "variants": 34, "maxima": maxima}))
        self.assertEqual(counts, 102)


if __name__ == "__main__":
    unittest.main()
