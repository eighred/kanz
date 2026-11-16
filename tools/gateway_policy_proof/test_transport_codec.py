import base64
import json
import lzma
from pathlib import Path
import unittest
import re
import subprocess
import sys
import tempfile
import inspect
import transport_codec as codec
from test_network_framing import bootstrap as actual_bootstrap


from transaction_fixture import qualify_codec, run_fixture, CASES


class TransportCodecTests(unittest.TestCase):
    def test_fixed_encoder_and_legacy_decoder(self):
        from unittest.mock import patch
        import ast
        import hashlib
        source = inspect.getsource(codec)
        decoder = next(n for n in ast.parse(source).body if isinstance(n, ast.FunctionDef) and n.name == "decode")
        # Frozen 0769 decoder AST; encoder tuning cannot broaden input acceptance.
        options = {"show_empty": True} if "show_empty" in inspect.signature(ast.dump).parameters else {}
        self.assertEqual(hashlib.sha256(ast.dump(decoder, include_attributes=False, **options).encode()).hexdigest(), "76068d99d95d5035e326ebf09a82f7f66cdba98e64d9de5ab8a5ac80f277a1e8")
        raw = "legacy and current UTF8 λ\r\n".encode()
        with patch.object(lzma, "compress", wraps=lzma.compress) as compress:
            self.assertEqual(codec.decode(codec.encode(raw)), raw)
            self.assertEqual(compress.call_args.kwargs, {"format": lzma.FORMAT_XZ, "check": lzma.CHECK_CRC64,
                "filters": [{"id": lzma.FILTER_LZMA2, "preset": 6, "dict_size": 1048576,
                             "pb": 0, "lc": 3, "nice_len": 80, "depth": 64}]})
        legacy = lzma.compress(raw, format=lzma.FORMAT_XZ, check=lzma.CHECK_CRC64,
                              filters=[{"id": lzma.FILTER_LZMA2, "preset": 6, "dict_size": 1048576}])
        self.assertEqual(codec.decode(base64.b64encode(legacy).decode()), raw)

    def test_linux_fixture_requires_codec_before_acceptance(self):
        source = inspect.getsource(run_fixture)
        marker = 'qualify_codec()\n            report["cases"].append("bounded_xz_transport")'
        # Independent actual execution order: codec runs before cluster creation
        # and therefore before every real API transaction. Membership alone hid
        # the first Linux run's evidence-order mismatch.
        self.assertEqual(CASES, ["bounded_xz_transport", "response_loss_1", "response_loss_2",
            "response_loss_3", "response_loss_4", "response_loss_5", "response_loss_6",
            "response_loss_7", "response_loss_8", "dry_run_no_write", "four_policy_success",
            "annotation_transport_preserved", "concurrent_change_refuses_rollback", "empty_endpoint_slice_null", "ipv6_endpoint_metadata", "deployment_uid_lineage",
            "controller_adoption_refusal", "never_ready_endpoint_nonparticipation", "publish_unready_refusal",
            "actual_tcp_policy_controls", "probe_uid_cleanup", "retained_restore_verified", "retained_restore_response_loss",
            "retained_restore_stale_cas", "retained_restore_metadata_refusal", "retained_restore_birth_projection_refusal",
            "retained_restore_scope_refusal", "retained_restore_receipt_refusal", "retained_restore_exclusive_journal"])
        self.assertIn(marker, source)
        self.assertLess(source.index(marker), source.index("proof.claim_cluster("))
        self.assertLess(source.index(marker), source.index("for at in range(1, 9):"))
        self.assertNotIn(marker, source.replace("qualify_codec()", "pass"))

    def test_exact_bootstrap_refuses_before_source_writes(self):
        here = Path(__file__).parent
        wrapper = (here.parent / "Invoke-TokyoGatewayPolicy.ps1").read_text()
        source = actual_bootstrap(wrapper)
        fixed = codec.encode_fixed({name: b"# fixed source\n" for name in codec.SOURCE_BUNDLES["normal"]["fixed"]}, "normal")
        names = codec.SOURCE_BUNDLES["normal"]["files"]
        bundle = {"files": {n: "# exact UTF8 λ\r\n" for n in names}, "payload": {"source": "a" * 40}}
        for mode in ("valid", "truncated", "extra", "oversized_file", "missing_lzma", "unsupported_crc64"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                value = json.loads(json.dumps(bundle))
                if mode == "extra": value["extra"] = True
                if mode == "oversized_file": value["files"][names[-1]] = "x" * 65537
                encoded = codec.encode(json.dumps(value).encode())
                if mode == "truncated": encoded = base64.b64encode(base64.b64decode(encoded)[:-1]).decode()
                prefix = "import sys; sys.modules['lzma']=None\n" if mode == "missing_lzma" else ""
                if mode == "unsupported_crc64":
                    prefix = "import lzma; lzma.is_check_supported=lambda check:False\n"
                result = subprocess.run([sys.executable, "-I", "-B", "-c", prefix + source, encoded, fixed["encoded"], directory],
                                        capture_output=True, timeout=10)
                if mode == "valid":
                    self.assertEqual(result.returncode, 0)
                    for name in names:
                        self.assertEqual((Path(directory) / name).read_bytes(), bundle["files"][name].encode())
                else:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertEqual(list(Path(directory).iterdir()), [])

    def test_roundtrip_and_runtime_refusal_controls(self):
        self.assertTrue(qualify_codec())

    def test_exact_bounds_and_bytes(self):
        for raw in (b"", b"\x00\xff\r\n", b"x" * 524288):
            self.assertEqual(codec.decode(codec.encode(raw)), raw)
        with self.assertRaises(ValueError): codec.encode(b"x" * 524289)
        import hashlib
        incompressible = b"".join(hashlib.sha256(index.to_bytes(4, "big")).digest() for index in range(1024))
        with self.assertRaisesRegex(ValueError, "^encoded bundle bound$"): codec.encode(incompressible)
        for encoded in ("A" * 20001, "invalid base64!", "AAAA\n", None):
            with self.assertRaises(ValueError): codec.decode(encoded)

    def test_fixed_frame_contract_modes_and_input_refusals(self):
        for mode, inventory in codec.SOURCE_BUNDLES.items():
            values = {name: ("# UTF8 λ\r\n" + name).encode() for name in inventory["fixed"]}
            framed = codec.encode_fixed(values, mode)
            raw, actual = codec.decode_fixed(framed["encoded"], mode)
            self.assertEqual(actual, values)
            self.assertEqual(framed["expanded"], len(raw))
            self.assertEqual(set(framed), {"encoded", "expanded"})
            name = next(iter(values))
            for control in ("missing", "extra", "not_dict", "text", "bool", "oversize", "utf8"):
                bad = dict(values)
                if control == "missing": del bad[name]
                elif control == "extra": bad["extra.py"] = b""
                elif control == "not_dict": bad = []
                elif control == "text": bad[name] = "text"
                elif control == "bool": bad[name] = True
                elif control == "oversize": bad[name] = b"x" * 65537
                else: bad[name] = b"\xff"
                with self.subTest(mode=mode, control=control), self.assertRaises(ValueError):
                    codec.encode_fixed(bad, mode)
        for mode in (None, True, "unknown"):
            with self.subTest(mode=mode), self.assertRaises(ValueError):
                codec.encode_fixed({}, mode)
            with self.subTest(decode_mode=mode), self.assertRaises(ValueError):
                codec.decode_fixed(codec.encode(b'{"files":{}}'), mode)

    def test_normal_fixed_partition_refuses_stale_placement(self):
        import hashlib
        canonical = {"source_loader.py": b"# loader\n", "rollout_probe.py": b"# probe\n"}
        self.assertEqual(set(codec.SOURCE_BUNDLES["normal"]["fixed"]), set(canonical))
        self.assertEqual(len(codec.SOURCE_BUNDLES["normal"]["files"]), 7)
        self.assertEqual(len(codec.SOURCE_BUNDLES["normal"]["fixed"]), 2)
        controls = {
            "missing_loader": {"rollout_probe.py": canonical["rollout_probe.py"]},
            "missing_probe": {"source_loader.py": canonical["source_loader.py"]},
            "former_fixed_extra": {**canonical, "bounded_process.py": b"# former fixed\n"},
            "stale_two_file_partition": {"source_loader.py": canonical["source_loader.py"], "bounded_process.py": b"# former fixed\n"},
            "third_fixed_module": {**canonical, "network_observation.py": b"# extra\n"},
        }
        for control, values in controls.items():
            with self.subTest(control=control):
                with self.assertRaisesRegex(ValueError, "^fixed source inventory$"):
                    codec.encode_fixed(values, "normal")
                frame = {"files": {name: {"text": data.decode("utf-8"), "sha256": hashlib.sha256(data).hexdigest()}
                                   for name, data in values.items()}}
                encoded = codec.encode(json.dumps(frame).encode("utf-8"))
                with self.assertRaisesRegex(ValueError, "^fixed source inventory$"):
                    codec.decode_fixed(encoded, "normal")

    def test_remote_decoder_source_exact_segments_and_modes(self):
        import ast
        source = (Path(__file__).parent / "transport_codec.py").read_text(encoding="utf-8")
        required = {"decode", "_unique_fixed", "_fixed_names", "decode_fixed"}
        signatures = {"decode": ("encoded",), "_unique_fixed": ("pairs",),
                      "_fixed_names": ("mode",), "decode_fixed": ("encoded", "mode")}
        for ending in ("\n", "\r\n"):
            canonical = source.replace("\n", ending)
            original = ast.parse(canonical)
            functions = {node.name: node for node in original.body if isinstance(node, ast.FunctionDef)}
            constants = {node.targets[0].id: node for node in original.body if isinstance(node, ast.Assign)}
            gate = next(node for node in original.body if isinstance(node, ast.If))
            for mode in ("normal", "plan", "restore"):
                with self.subTest(mode=mode, ending=repr(ending)):
                    remote = codec.remote_decoder_source(canonical, mode)
                    tree = ast.parse(remote)
                    actual = {node.name: node for node in tree.body if isinstance(node, ast.FunctionDef)}
                    self.assertEqual(set(actual), required)
                    for name in required:
                        arguments = actual[name].args
                        self.assertEqual(tuple(argument.arg for argument in arguments.args), signatures[name])
                        self.assertEqual(arguments.posonlyargs, [])
                        self.assertEqual(arguments.kwonlyargs, [])
                        self.assertIsNone(arguments.vararg)
                        self.assertIsNone(arguments.kwarg)
                        self.assertEqual(ast.get_source_segment(remote, actual[name]),
                                         ast.get_source_segment(canonical, functions[name]))
                    assignments = {node.targets[0].id: node for node in tree.body if isinstance(node, ast.Assign)}
                    self.assertEqual(set(assignments), {"ENCODED_LIMIT", "EXPANDED_LIMIT", "MEMORY_LIMIT", "SOURCE_BUNDLES"})
                    for name in ("ENCODED_LIMIT", "EXPANDED_LIMIT", "MEMORY_LIMIT"):
                        self.assertEqual(ast.get_source_segment(remote, assignments[name]),
                                         ast.get_source_segment(canonical, constants[name]))
                    self.assertEqual(ast.literal_eval(assignments["SOURCE_BUNDLES"].value),
                                     {mode: codec.SOURCE_BUNDLES[mode]})
                    self.assertEqual([node.names[0].name for node in tree.body if isinstance(node, ast.Import)],
                                     ["base64", "lzma", "hashlib", "json"])
                    actual_gate = next(node for node in tree.body if isinstance(node, ast.If))
                    self.assertEqual(ast.get_source_segment(remote, actual_gate), ast.get_source_segment(canonical, gate))
                    self.assertNotIn("def encode", remote)
                    self.assertNotIn("remote_decoder_source", remote)

    def test_remote_decoder_source_dependency_and_binding_refusals(self):
        import ast
        source = (Path(__file__).parent / "transport_codec.py").read_text(encoding="utf-8")
        tree = ast.parse(source)
        functions = {node.name: node for node in tree.body if isinstance(node, ast.FunctionDef)}
        helper = ast.get_source_segment(source, functions["_unique_fixed"])
        mutations = [
            source.replace("def decode(encoded):", "def decode(encoded):\n    unreviewed_helper()"),
            source.replace("def decode(encoded):", "def decode(encoded):\n    import os"),
            source.replace("def decode(encoded):", "def decode(encoded):\n    return (lambda: encoded)()"),
            source.replace("def decode(encoded):", "def decode(encoded):\n    return [value for value in encoded]"),
            source.replace("len(raw) > EXPANDED_LIMIT", "len(raw) > UNREVIEWED_LIMIT"),
            source.replace(helper, ""), source + "\n" + helper + "\n",
            source + "\nENCODED_LIMIT = 20000\n", source + "\ndecode = encode\n",
            source.replace("ENCODED_LIMIT = 20000", "ENCODED_LIMIT = 20001"),
            source.replace("ENCODED_LIMIT = 20000", "ENCODED_LIMIT = True"),
            source.replace("if not lzma.is_check_supported", "if lzma.is_check_supported"),
            source.replace("import base64", "import base64 as other"),
            source + "\nimport base64\n", source + "\nside_effect()\n",
            source.replace('hashlib.sha256(data).hexdigest() != value["sha256"]', "False"),
            source.replace('object_pairs_hook=_unique_fixed', 'object_pairs_hook=dict'),
            source.replace('"rollout_probe.py"]}', '"unreviewed.py"]}', 1),
        ]
        self.assertTrue(all(value != source for value in mutations))
        for index, changed in enumerate(mutations):
            with self.subTest(mutation=index), self.assertRaises(ValueError):
                codec.remote_decoder_source(changed, "normal")
        signatures = {"decode": ("encoded",), "_unique_fixed": ("pairs",),
                      "_fixed_names": ("mode",), "decode_fixed": ("encoded", "mode")}
        for name, arguments in signatures.items():
            parameters = ", ".join(arguments)
            canonical = "def " + name + "(" + parameters + "):"
            self.assertEqual(source.count(canonical), 1)
            for form in (parameters + ", unused", parameters + ", *, unused", parameters + ", *unused",
                         parameters + ", **unused", parameters + ", /"):
                changed = source.replace(canonical, "def " + name + "(" + form + "):")
                self.assertNotEqual(changed, source)
                with self.subTest(signature=name, form=form), self.assertRaisesRegex(ValueError, "^remote decoder signature$"):
                    codec.remote_decoder_source(changed, "normal")
        for value, mode in ((None, "normal"), ("x" * 65537, "normal"), (source, None), (source, True), (source, "unknown")):
            with self.subTest(mode=mode), self.assertRaises(ValueError):
                codec.remote_decoder_source(value, mode)

    def _bootstrap_shell(self, command, directory, after=""):
        import shlex
        for tool in ("/usr/bin/dash", "/usr/bin/cat"):
            self.assertTrue(Path(tool).is_file(), "canonical POSIX fixture tool unavailable")
        script = "set -eu\nwork=" + shlex.quote(str(directory)) + "\n" + command + after
        return subprocess.run(["/usr/bin/dash", "-c", script], cwd=directory,
                              env={"HOME": str(directory), "TMPDIR": str(directory), "LANG": "C.UTF-8"},
                              capture_output=True, timeout=10)

    def test_bootstrap_heredoc_refuses_unsafe_or_oversized_data(self):
        import ast
        function = ast.parse(inspect.getsource(codec.bootstrap_command)).body[0]
        self.assertEqual([arg.arg for arg in function.args.args], ["source"])
        self.assertFalse(function.args.posonlyargs or function.args.kwonlyargs or function.args.defaults)
        self.assertIsNone(function.args.vararg)
        self.assertIsNone(function.args.kwarg)
        delimiter = "KANZ_GATEWAY_FIXED_BOOTSTRAP_V1"
        controls = (None, True, b"bytes", "\ud800", "\x00", "x" * 65537,
                    delimiter, delimiter + "\n", "prefix\n" + delimiter + "\nsuffix",
                    "prefix\r\n" + delimiter + "\r\nsuffix")
        for index, source in enumerate(controls):
            with self.subTest(control=index), self.assertRaises(ValueError):
                codec.bootstrap_command(source)
        self.assertIsInstance(codec.bootstrap_command("x" * 65536), str)
        self.assertIsInstance(codec.bootstrap_command(delimiter + "_not_a_delimiter"), str)

    def test_bootstrap_heredoc_actual_dash_preserves_bytes_without_expansion(self):
        cases = ("", "no final newline", "one\n", "two\n\n", "CRLF\r\n", "two CRLF\r\n\r\n",
                 "mixed\r\nLF\nlast", "terminalX", "terminalX\n", "UTF8 λ € Ω\r\n",
                 "quotes ' \" \\ and $HOME $$ ${PATH}\n",
                 "$(printf expanded > DOLLAR_EXPANDED) `printf expanded > BACKTICK_EXPANDED`\n")
        for index, source in enumerate(cases):
            with self.subTest(case=index), tempfile.TemporaryDirectory() as directory:
                target = Path(directory)
                result = self._bootstrap_shell(codec.bootstrap_command(source), target,
                                               "printf 'READY\\n'\n")
                self.assertEqual(result.returncode, 0)
                self.assertEqual(result.stdout, b"READY\n")
                self.assertEqual(result.stderr, b"")
                actual = (target / "bootstrap.py").read_bytes()
                self.assertTrue(actual == source.encode("utf-8"), "bootstrap byte identity changed")
                self.assertEqual({path.name for path in target.iterdir()}, {"bootstrap.py"})

    def test_bootstrap_heredoc_actual_cat_failure_stops_before_launch(self):
        command = codec.bootstrap_command("# canonical writer failure fixture\n")
        marker = "/usr/bin/cat <<'"
        self.assertEqual(command.count(marker), 1)
        # Fault only the fixed read input: use the real cat and its real captured
        # stdout. No replacement binary, function, mocked stream or shell body.
        failed = command.replace(marker, "/usr/bin/cat MISSING_OWNED_CAT_INPUT <<'", 1)
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory)
            result = self._bootstrap_shell(failed, target,
                "printf 'LAUNCHED\\n' > \"$work/launch-marker\"\n")
            self.assertEqual(result.returncode, 1)
            self.assertEqual(result.stdout, b"")
            self.assertGreater(len(result.stderr), 0)
            self.assertEqual(list(target.iterdir()), [])

    def test_bootstrap_heredoc_actual_write_failure_stops_before_launch(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory)
            (target / "bootstrap.py").mkdir()
            result = self._bootstrap_shell(codec.bootstrap_command("# bounded writer fixture\n"), target,
                "printf 'LAUNCHED\\n' > \"$work/launch-marker\"\n")
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(result.stdout, b"")
            self.assertGreater(len(result.stderr), 0)
            self.assertTrue((target / "bootstrap.py").is_dir())
            self.assertEqual({path.name for path in target.iterdir()}, {"bootstrap.py"})

    def test_bootstrap_heredoc_binds_all_three_actual_bootstrap_sources(self):
        import re
        tools = Path(__file__).parents[1]
        codec_source = inspect.getsource(codec).replace("\r\n", "\n")
        wrappers = (("normal", "Invoke-TokyoGatewayPolicy.ps1"),
                    ("plan", "Invoke-TokyoGatewayPolicyRecoveryPlan.ps1"),
                    ("restore", "Invoke-TokyoGatewayPolicyRetainedRestore.ps1"))
        pattern = r"\$bootstrap = \$decoder \+ \"" + chr(96) + r"n\" \+ @'\r?\n(.*?)\r?\n'@"
        for mode, name in wrappers:
            canonical = (tools / name).read_bytes().decode("utf-8").replace("\r\n", "\n")
            for ending in ("\n", "\r\n"):
                with self.subTest(mode=mode, CRLF=ending == "\r\n"), tempfile.TemporaryDirectory() as directory:
                    wrapper = canonical.replace("\n", ending)
                    match = re.search(pattern, wrapper, re.S)
                    self.assertIsNotNone(match)
                    source = codec.remote_decoder_source(codec_source.replace("\n", ending), mode) + "\n" + match.group(1)
                    target = Path(directory)
                    result = self._bootstrap_shell(codec.bootstrap_command(source), target, "printf 'READY\\n'\n")
                    self.assertEqual((result.returncode, result.stdout, result.stderr), (0, b"READY\n", b""))
                    self.assertTrue((target / "bootstrap.py").read_bytes() == source.encode("utf-8"),
                                    "actual canonical bootstrap bytes changed in shell transport")
                    self.assertEqual({path.name for path in target.iterdir()}, {"bootstrap.py"})

    def test_wrapper_binds_same_source_and_preserves_caps(self):
        wrapper = (Path(__file__).parents[1] / "Invoke-TokyoGatewayPolicy.ps1").read_text()
        markers = ["$encode = $codec +", "$bootstrap = $decoder +", "raw=decode(sys.argv[1])",
                   "-I -B -c $encode", "ls-files --error-unmatch tools/gateway_policy_proof/transport_codec.py",
                   "$codecInfo.LinkType", "$codecInfo.PSIsContainer", "$codecInfo.Length -gt 65536", "$encoded.Length -gt 20000",
                   "($bytes.Length + $fixedSize) -gt 524288", "len(data)>65536", "$parameterBytes -gt 30000",
                   "$bootstrapBuilder = $codec +", "sys.stdin.buffer.read(87387)", "len(framed)>87386",
                   "len(raw)>65536", "bootstrap_command(raw.decode('utf-8',errors='strict'))",
                   "$bootstrapEncoded | & $python -I -B -c $bootstrapBuilder",
                   "$bootstrapFrame.command -isnot [string]", "$bootstrapCommand,"]
        def valid(text): return all(marker in text for marker in markers)
        self.assertTrue(valid(wrapper))
        for marker in markers:
            self.assertFalse(valid(wrapper.replace(marker, "removed")), marker)
        self.assertLess(wrapper.index("import lzma; assert"), wrapper.index("work=$(mktemp"))


if __name__ == "__main__":
    unittest.main()
