import copy
import json
from pathlib import Path
import subprocess
import tempfile
import tomllib
import unittest
from unittest.mock import patch

import rust_release_evidence as gate


def fixture(service="binance-depth"):
    names = ["kanz-" + service, "kanz-market-feed", "async-nats", "tokio", "prost"]
    packages = [{"name": name, "version": "1.0.0", "source": "crates.io"} for name in names]
    packages[0]["root"] = True
    sbom = {"packages": [{"name": p["name"], "versionInfo": p["version"], "externalRefs": [
        {"referenceType": "purl", "referenceLocator": f"pkg:cargo/{p['name']}@{p['version']}"}]} for p in packages]}
    return {"packages": packages}, {"package": copy.deepcopy(packages)}, sbom


class EvidenceTests(unittest.TestCase):
    def test_both_services_require_actual_runtime_catalog(self):
        for service in gate.SERVICES:
            data, lock, sbom = fixture(service)
            self.assertEqual(gate.validate_inventory(data, lock, service, sbom), (5, 5))

    def test_build_only_crates_need_lock_but_not_runtime_spdx(self):
        data, lock, sbom = fixture()
        package = {"name": "build-only", "version": "1.0.0", "kind": "build"}
        data["packages"].append(package)
        lock["package"].append(package)
        self.assertEqual(gate.validate_inventory(data, lock, "binance-depth", sbom), (6, 5))

    def test_mutations_refuse_partial_or_wrong_evidence(self):
        for mutation in ("empty", "wrong-root", "two-roots", "no-root", "missing-witness",
                         "wrong-version", "duplicate", "unknown-kind", "no-cargo",
                         "missing-package", "wrong-purl", "os-purl"):
            with self.subTest(mutation=mutation):
                data, lock, sbom = fixture()
                if mutation == "empty": data["packages"] = []
                if mutation == "wrong-root": data["packages"][0]["name"] = "kanz-okx-depth"
                if mutation == "two-roots": data["packages"][1]["root"] = True
                if mutation == "no-root": del data["packages"][0]["root"]
                if mutation == "missing-witness": data["packages"].pop()
                if mutation == "wrong-version": data["packages"][2]["version"] = "2.0.0"
                if mutation == "duplicate": data["packages"].append(data["packages"][1])
                if mutation == "unknown-kind": data["packages"][1]["kind"] = "unknown"
                if mutation == "no-cargo": sbom["packages"] = []
                if mutation == "missing-package": sbom["packages"].pop()
                if mutation == "wrong-purl": sbom["packages"][0]["externalRefs"][0]["referenceLocator"] += "-wrong"
                if mutation == "os-purl": sbom["packages"][0]["externalRefs"][0]["referenceLocator"] = "pkg:deb/root@1.0.0"
                with self.assertRaises(ValueError):
                    gate.validate_inventory(data, lock, "binance-depth", sbom)

    def test_release_never_accepts_tag_or_other_repository(self):
        digest = "sha256:" + "a" * 64
        gate.validate_reference("okx-depth", "ghcr.io/eighred/okx-depth@" + digest, False)
        gate.validate_reference("okx-depth", digest, True)
        for image in (digest, "ghcr.io/eighred/okx-depth:latest", "ghcr.io/eighred/binance-depth@" + digest,
                      "evilghcr.io/eighred/okx-depth@" + digest, "ghcr.io/eighred/okx-depth@" + digest + "/x"):
            with self.assertRaises(ValueError):
                gate.validate_reference("okx-depth", image, False)

    def test_missing_malformed_and_oversized_metadata_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "metadata.json"
            with self.assertRaises(ValueError): gate.read_json(path)
            path.write_text("not JSON")
            with self.assertRaises(ValueError): gate.read_json(path)
            path.write_text(json.dumps({"packages": []}))
            with self.assertRaises(ValueError): gate.read_json(path, limit=2)

    def test_tool_failure_and_timeout_propagate(self):
        for error in (subprocess.CalledProcessError(1, "audit"), subprocess.TimeoutExpired("audit", 900)):
            with patch.object(gate.subprocess, "run", side_effect=error):
                with self.assertRaises(type(error)):
                    gate.run(["cargo-audit", "audit"])

    def test_orchestration_extracts_without_execution_and_scrubs_auditor(self):
        self.orchestrate()

    def test_failed_audit_cannot_produce_pass_evidence(self):
        self.orchestrate(fail_audit=True)

    def test_extraction_failure_removes_container(self):
        self.orchestrate(fail_copy=True)

    def orchestrate(self, fail_audit=False, fail_copy=False):
        data, _, sbom = fixture()
        lock = tomllib.loads((Path(gate.__file__).resolve().parents[2] / "kanz-rs/Cargo.lock").read_text())
        versions = {p["name"]: p["version"] for p in lock["package"]}
        for package, entry in zip(data["packages"], sbom["packages"]):
            version = versions[package["name"]]
            package["version"] = entry["versionInfo"] = version
            entry["externalRefs"][0]["referenceLocator"] = f"pkg:cargo/{package['name']}@{version}"
        calls = []
        image = "ghcr.io/eighred/binance-depth@sha256:" + "a" * 64

        def fake_output(args, **kwargs):
            calls.append(args)
            if args[:3] == ["docker", "image", "inspect"]:
                return json.dumps([{"Id": "sha256:" + "b" * 64, "RepoDigests": [image]}])
            if args[:2] == ["docker", "create"]: return "container"
            return "c" * 40

        def fake_run(args, **kwargs):
            calls.append(args)
            if args[:2] == ["docker", "cp"]:
                if fail_copy: raise subprocess.CalledProcessError(1, args)
                Path(args[-1]).write_bytes(b"fixture ELF")
            if args[0] == "rust-audit-info":
                kwargs["stdout"].write(json.dumps(data))
                self.assertEqual(args[-2:], ["104857600", str(gate.LIMIT)])
            if args[0] == "/pinned/cargo-audit":
                self.assertNotIn("SECRET", kwargs["env"])
                self.assertEqual(kwargs["env"]["HOME"], str(kwargs["cwd"]))
                self.assertEqual(kwargs["env"]["CARGO_HOME"], str(kwargs["cwd"] / "cargo"))
                self.assertIn("--deny", args)
                self.assertNotIn("--no-fetch", args)
                self.assertNotIn("--ignore", args)
                self.assertNotIn("--stale", args)
                if fail_audit: raise subprocess.CalledProcessError(1, args)
                kwargs["stdout"].write("{}")

        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            (path / "sbom.json").write_text(json.dumps(sbom))
            argv = ["gate", "--service", "binance-depth", "--image", image,
                    "--sbom", str(path / "sbom.json"), "--output", str(path / "evidence")]
            with patch("sys.argv", argv), patch.object(gate, "run", side_effect=fake_run), \
                 patch.object(gate, "output", side_effect=fake_output), \
                 patch.object(gate.shutil, "which", return_value="/pinned/cargo-audit"), \
                 patch.dict(gate.os.environ, {"SECRET": "must-not-inherit"}):
                if fail_copy or fail_audit:
                    with self.assertRaises(subprocess.CalledProcessError): gate.main()
                else:
                    gate.main()
            evidence = json.loads((path / "evidence/evidence.json").read_text())
            self.assertEqual(evidence["passed"], not (fail_copy or fail_audit))
            self.assertIn(["docker", "rm", "container"], calls)
            self.assertFalse(any(c[:2] in (["docker", "run"], ["docker", "start"], ["docker", "exec"]) for c in calls))
            if not (fail_copy or fail_audit):
                audits = [c for c in calls if c[0] == "/pinned/cargo-audit"]
                self.assertEqual(len(audits), 2)
                self.assertIn("--file", audits[0])
                self.assertIn("bin", audits[1])


if __name__ == "__main__":
    unittest.main()
