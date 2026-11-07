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
        source = actual_bootstrap(wrapper, {"source_loader.py": b"# fixed source\n", "network_observation.py": b"# fixed source\n"})
        names = ("policy_state.py", "transaction.py", "policy_client.py", "rollout_probe.py", "rollout_scope.py", "rollout_engine.py", "bounded_process.py")
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
                result = subprocess.run([sys.executable, "-I", "-B", "-c", prefix + source, encoded, directory],
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
        for encoded in ("A" * 20001, "invalid base64!", "AAAA\n", None):
            with self.assertRaises(ValueError): codec.decode(encoded)

    def test_wrapper_binds_same_source_and_preserves_caps(self):
        wrapper = (Path(__file__).parents[1] / "Invoke-TokyoGatewayPolicy.ps1").read_text()
        markers = ["$encode = $codec +", "$bootstrap = $codec +", "raw=decode(sys.argv[1])",
                   "-I -B -c $encode", "ls-files --error-unmatch tools/gateway-policy-proof/transport_codec.py",
                   "$codecInfo.LinkType", "$codecInfo.PSIsContainer", "$codecInfo.Length -gt 65536", "$encoded.Length -gt 20000",
                   "($bytes.Length + $fixedSize) -gt 524288", "len(data)>65536", "$parameterBytes -gt 30000"]
        def valid(text): return all(marker in text for marker in markers)
        self.assertTrue(valid(wrapper))
        for marker in markers:
            self.assertFalse(valid(wrapper.replace(marker, "removed")), marker)
        self.assertLess(wrapper.index("import lzma; assert"), wrapper.index("work=$(mktemp"))


if __name__ == "__main__":
    unittest.main()
