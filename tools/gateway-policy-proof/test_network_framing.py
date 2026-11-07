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
import yaml
from unittest.mock import patch

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
WRAPPERS = {"normal": "Invoke-TokyoGatewayPolicy.ps1", "plan": "Invoke-TokyoGatewayPolicyRecoveryPlan.ps1",
            "restore": "Invoke-TokyoGatewayPolicyRetainedRestore.ps1"}


def names(wrapper):
    transfer = re.search(r"foreach \(\$name in @\((.*?)\)\)", wrapper).group(1)
    fixed = re.search(r"\$fixedNames = @\((.*?)\)", wrapper).group(1)
    return set(re.findall("'([^']+)'", transfer)), set(re.findall("'([^']+)'", fixed))


def valid_framing(wrapper, expected):
    try:
        files, fixed = names(wrapper)
        bootstrap_names = ast.literal_eval(re.search(r"b=json.loads\(raw\); names=(\{[^\n]+\})", wrapper).group(1))
        fixed_checks = re.findall(r"if set\((?:values|fixed)\)!=(\{[^\n]+\}):", wrapper)
        return (fixed == expected and files.isdisjoint(fixed) and bootstrap_names == files
                and len(fixed_checks) == 2 and all(ast.literal_eval(v) == fixed for v in fixed_checks)
                and all(v in wrapper for v in (
                    "$info.LinkType", "$info.PSIsContainer", "$info.Length -gt 65536", "$raw.Length -gt 65536",
                    "ls-files --error-unmatch $relative", "data.decode('utf-8',errors='strict')",
                    "sys.stdin.buffer.read(262145)", "len(raw)>262144", "len(data)>65536",
                    "hashlib.sha256(data).hexdigest()!=sha", "($bytes.Length + $fixedSize) -gt 524288",
                    "len(raw)+sum(len(v[0]) for v in fixed.values())>524288",
                    "$encoded.Length -gt 20000", "$parameterBytes -gt 30000", "$parameters.Length -gt 30000",
                    "with (root/name).open('xb')", "-I -B -c $literalBuilder"))
                and wrapper.index("prepared[name]=data") < wrapper.index("with (root/name).open('xb')"))
    except (ValueError, AttributeError, SyntaxError):
        return False


def declaration(wrapper, fixed):
    builder = re.search(r"\$literalBuilder = @'\n(.*?)\n'@", wrapper, re.S).group(1)
    raw = json.dumps({name: base64.b64encode(data).decode() for name, data in fixed.items()}).encode()
    result = subprocess.run([sys.executable, "-I", "-B", "-c", builder], input=raw,
                            capture_output=True, timeout=10, check=True)
    return result.stdout.decode("utf-8").strip()


def bootstrap(wrapper, fixed):
    body = re.search(r"\$bootstrap = \$codec \+ \"`n\" \+ \$fixedDeclaration \+ \"`n\" \+ @'\n(.*?)\n'@", wrapper, re.S).group(1)
    return (HERE / "transport_codec.py").read_text(encoding="utf-8") + "\n" + declaration(wrapper, fixed) + "\n" + body


class NetworkFramingTests(unittest.TestCase):
    def test_geometry_steps_are_mandatory_and_enabled(self):
        workflow = yaml.safe_load((ROOT / ".github/workflows/gateway-policy-proof.yml").read_text(encoding="utf-8"))
        commands = {
            "python3 -B -m unittest test_network_framing.NetworkFramingTests.test_actual_wrappers_all_96_source_variants": "tools/gateway-policy-proof",
            "python3 -B -m unittest test_transfer.CNITransferTests.test_actual_powershell_32_cni_variants": "tools/cni-proof",
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
        builders = []
        for mode, name in WRAPPERS.items():
            wrapper = (ROOT / "tools" / name).read_text(encoding="utf-8")
            expected = {"source_loader.py"} | ({"network_observation.py"} if mode == "normal" else set())
            self.assertTrue(valid_framing(wrapper, expected))
            builder = re.search(r"\$literalBuilder = @'\n(.*?)\n'@", wrapper, re.S).group(1)
            builders.append(re.sub(r"if set\(values\)!=\{[^\n]+\}:", "if CLOSED:", builder))
            for marker in ("data.decode('utf-8',errors='strict')", "hashlib.sha256(data).hexdigest()!=sha",
                           "$info.LinkType", "len(raw)+sum(len(v[0]) for v in fixed.values())>524288",
                           "($bytes.Length + $fixedSize) -gt 524288", "with (root/name).open('xb')",
                           "$parameters.Length -gt 30000", "-I -B -c $literalBuilder"):
                self.assertFalse(valid_framing(wrapper.replace(marker, "removed"), expected), marker)
        self.assertEqual(len(set(builders)), 1)  # Same semantics; only fixed inventory differs.

    def test_exact_bootstrap_thirteen_controls(self):
        wrapper = (ROOT / "tools" / WRAPPERS["normal"]).read_text(encoding="utf-8")
        files, fixed_names = names(wrapper)
        fixed = {name: (HERE / name).read_bytes() for name in sorted(fixed_names)}
        source = bootstrap(wrapper, fixed)
        tree = ast.parse(source)
        node = next(n for n in tree.body if isinstance(n, ast.Assign) and any(isinstance(t, ast.Name) and t.id == "fixed" for t in n.targets))
        original = ast.literal_eval(node.value)
        lines = source.splitlines(keepends=True)
        def changed(value):
            return "".join(lines[:node.lineno-1]) + "fixed=" + repr(value) + "\n" + "".join(lines[node.end_lineno:])
        bundle = {"files": {name: "# exact UTF8 λ\r\n" for name in files}, "payload": {"source": "a" * 40}}
        def run(value, script=source, expected=original, collision=None, good=False):
            packed = lzma.compress(json.dumps(value).encode(), format=lzma.FORMAT_XZ, check=lzma.CHECK_CRC64,
                                   filters=[{"id": lzma.FILTER_LZMA2, "preset": 6, "dict_size": 1048576}])
            with tempfile.TemporaryDirectory() as directory:
                path = Path(directory)
                if collision: (path / collision).write_text("preserved", encoding="utf-8")
                with patch.object(sys, "argv", ["bootstrap", base64.b64encode(packed).decode(), directory]):
                    try: exec(compile(script, "<actual-wrapper-bootstrap>", "exec"), {})
                    except (ValueError, FileExistsError):
                        self.assertFalse(good)
                        if collision: self.assertEqual((path / collision).read_text(), "preserved")
                        else: self.assertEqual(list(path.iterdir()), [])
                    else:
                        self.assertTrue(good)
                        self.assertEqual({p.name for p in path.iterdir()}, files | fixed_names | {"input.json"})
                        for name, (data, _) in expected.items(): self.assertEqual((path / name).read_bytes(), data)
                        for name, text in value["files"].items(): self.assertEqual((path / name).read_bytes(), text.encode())
        run(bundle, good=True)
        for name in original:
            bad = dict(original); bad[name] = (bad[name][0], "0" * 64); run(bundle, changed(bad))
            run(bundle, collision=name)
            data = b"x" * 65537; bad[name] = (data, hashlib.sha256(data).hexdigest()); run(bundle, changed(bad))
        for mode in ("missing", "extra", "duplicate", "oversize", "combined"):
            value = json.loads(json.dumps(bundle)); fixed_value = dict(original)
            if mode == "missing": del fixed_value["network_observation.py"]
            elif mode == "extra": fixed_value["evil.py"] = (b"", hashlib.sha256(b"").hexdigest())
            elif mode == "duplicate": value["files"]["source_loader.py"] = "substitute"
            elif mode == "oversize": value["files"]["rollout_engine.py"] = "x" * 65537
            else: value["payload"]["pad"] = "x" * (524000 - len(json.dumps(value)))
            run(value, changed(fixed_value))
        unicode_sources = {name: data + "# UTF8 λ ' \\\r\n".encode() for name, (data, _) in original.items()}
        expected = {name: (data, hashlib.sha256(data).hexdigest()) for name, data in unicode_sources.items()}
        run(bundle, bootstrap(wrapper, unicode_sources), expected, good=True)

    def test_builder_rejects_invalid_utf8_before_literal(self):
        wrapper = (ROOT / "tools" / WRAPPERS["plan"]).read_text(encoding="utf-8")
        with self.assertRaises(subprocess.CalledProcessError):
            declaration(wrapper, {"source_loader.py": b"\xff"})

    @unittest.skipUnless(os.environ.get("KANZ_REQUIRE_NETWORK_FRAMING") == "1", "dedicated actual PowerShell geometry gate")
    def test_actual_wrappers_all_96_source_variants(self):
        import yaml
        pwsh = shutil.which("pwsh")
        kubectl = shutil.which("kubectl")
        self.assertIsNotNone(pwsh, "mandatory PowerShell measurement unavailable")
        self.assertIsNotNone(kubectl, "mandatory rendered source unavailable")
        raw = subprocess.check_output([kubectl, "kustomize", "--load-restrictor=LoadRestrictionsNone",
                                       str(ROOT / "kanz/infra/overlays/testnet-tokyo")], cwd=ROOT, timeout=30)
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
        counts = 0
        for mode, filename in WRAPPERS.items():
            wrapper = (ROOT / "tools" / filename).read_text(encoding="utf-8")
            files, fixed_names = names(wrapper)
            stage = wrapper[wrapper.index("$bundle ="):wrapper.index("$latest =")]
            maxima = {key: 0 for key in ("encoded", "expanded", "parameters", "characters")}
            for ending in ("\n", "\r\n"):
                for variant in range(16):
                    def digest(label): return hashlib.sha256((str(variant) + label).encode()).hexdigest()
                    ordered_files = re.findall("'([^']+)'", re.search(r"foreach \(\$name in @\((.*?)\)\)", wrapper).group(1))
                    source_files = {name: (HERE / name).read_text(encoding="utf-8").replace("\n", ending) for name in ordered_files}
                    fixed = {name: (HERE / name).read_text(encoding="utf-8").replace("\n", ending).encode() for name in re.findall("'([^']+)'", re.search(r"\$fixedNames = @\((.*?)\)", wrapper).group(1))}
                    literal = declaration(wrapper, fixed)
                    from rollout_probe import IMAGE
                    identity = {"root": IMAGE.split("@")[1], "manifest": IMAGE.split("@")[1],
                                "rootMediaType": "application/vnd.oci.image.manifest.v1+json", "platform": "linux/amd64",
                                "config": "sha256:4b47a20ba2e7896b29f6a8400bcbfaf2c4f5baec4a18d3d445e11b03a6bf932f"}
                    provenance = {"command": "21d730ab-18c9-43ba-9427-522d557b6f2c", "instance": "i-0ee1235614219f089",
                        "oldSource": "0769d36cf857993284109d03868393a0c7e0d7c8", "directory": "/var/tmp/kanz-gateway-policy-nu1ezqwo",
                        **{key: digest(key) for key in ("commandSHA256", "sourceSHA256", "bundleSHA256", "originalPoliciesSHA256", "originalWorkloadsSHA256")}}
                    data = {"files": source_files, "source": digest("source")[:40], "policies": policies, "workloads": workloads,
                            "identity": identity, "provenance": provenance, "literal": literal, "fixedSize": sum(map(len, fixed.values()))}
                    with tempfile.TemporaryDirectory() as directory:
                        inputs = Path(directory) / "inputs.json"
                        inputs.write_text(json.dumps(data), encoding="utf-8")
                        prefix = "$ErrorActionPreference='Stop'\n$repo=" + quoted(ROOT) + "\n$proofRoot=" + quoted(HERE) + "\n$python=" + quoted(sys.executable) + "\n$Apply=$true\n"
                        prefix += "$d=Get-Content -Raw -Encoding utf8 -LiteralPath " + quoted(inputs) + " | ConvertFrom-Json -AsHashtable\n"
                        prefix += "$files=$d.files;$source=$d.source;$policies=$d.policies;$identity=$d.identity;$provenance=$d.provenance;$projection=@{workloads=$d.workloads};$fixedDeclaration=$d.literal;$fixedSize=$d.fixedSize\n"
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
            print(json.dumps({"wrapper": mode, "variants": 32, "maxima": maxima}))
        self.assertEqual(counts, 96)


if __name__ == "__main__":
    unittest.main()
