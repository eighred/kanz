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
            source.replace('"bounded_process.py"]}', '"unreviewed.py"]}'),
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

    def test_wrapper_binds_same_source_and_preserves_caps(self):
        wrapper = (Path(__file__).parents[1] / "Invoke-TokyoGatewayPolicy.ps1").read_text()
        markers = ["$encode = $codec +", "$bootstrap = $decoder +", "raw=decode(sys.argv[1])",
                   "-I -B -c $encode", "ls-files --error-unmatch tools/gateway_policy_proof/transport_codec.py",
                   "$codecInfo.LinkType", "$codecInfo.PSIsContainer", "$codecInfo.Length -gt 65536", "$encoded.Length -gt 20000",
                   "($bytes.Length + $fixedSize) -gt 524288", "len(data)>65536", "$parameterBytes -gt 30000"]
        def valid(text): return all(marker in text for marker in markers)
        self.assertTrue(valid(wrapper))
        for marker in markers:
            self.assertFalse(valid(wrapper.replace(marker, "removed")), marker)
        self.assertLess(wrapper.index("import lzma; assert"), wrapper.index("work=$(mktemp"))


if __name__ == "__main__":
    unittest.main()
