import copy
import io
import tarfile
import sys
import json
from pathlib import Path
import subprocess
import tempfile
import tomllib
import unittest
from unittest.mock import patch

import rust_release_evidence as gate


def fixture(service="binance-depth"):
    recipe = gate.image_recipe(service)
    names = [recipe.rust_root, *sorted(recipe.witnesses)]
    packages = [{"name": name, "version": "1.0.0", "source": "crates.io"} for name in names]
    packages[0]["root"] = True
    sbom = {"packages": [{"name": p["name"], "versionInfo": p["version"], "externalRefs": [
        {"referenceType": "purl", "referenceLocator": f"pkg:cargo/{p['name']}@{p['version']}"}]} for p in packages]}
    return {"packages": packages}, {"package": copy.deepcopy(packages)}, sbom


class EvidenceTests(unittest.TestCase):
    def test_closed_hybrid_recipe_requires_exact_paths(self):
        recipe = gate.image_recipe("lake-sink")
        self.assertEqual(recipe.rust_path, "/kanz-lake-snapshot")
        self.assertEqual(recipe.rust_root, "kanz-lake-snapshot")
        self.assertEqual(recipe.go_path, "/lake-sink")
        self.assertEqual(recipe.go_main, "github.com/eighred/kanz/services/lake-sink/cmd/lake-sink")
        self.assertEqual(recipe.native_base, gate.CC_RUNTIME)
        for name in ("lake", "kanz-lake-snapshot", "../lake-sink", "lake-sink/other"):
            with self.assertRaises(ValueError):
                gate.image_recipe(name)
        gate.validate_reference("lake-sink", "sha256:" + "a" * 64, True)

    def test_hybrid_cargo_root_and_storage_witnesses_are_distinct(self):
        recipe = gate.image_recipe("lake-sink")
        names = [recipe.rust_root, *sorted(recipe.witnesses)]
        packages = [{"name": n, "version": "1.0.0"} for n in names]
        packages[0]["root"] = True
        data = {"packages": packages}
        lock = {"package": copy.deepcopy(packages)}
        sbom = {"packages": [{"name": p["name"], "versionInfo": p["version"], "externalRefs": [
            {"referenceType": "purl", "referenceLocator": f"pkg:cargo/{p['name']}@{p['version']}"}]} for p in packages]}
        self.assertEqual(gate.validate_inventory(data, lock, "lake-sink", sbom), (6, 6))
        for name in names:
            bad = copy.deepcopy(data)
            bad["packages"] = [p for p in bad["packages"] if p["name"] != name]
            with self.assertRaises(ValueError):
                gate.validate_inventory(bad, lock, "lake-sink", sbom)

    def test_native_inventory_requires_exact_base_and_both_owned_members(self):
        def record(char, mode=0o755):
            return {"sha256": char * 64, "size": 64, "uid": 0, "gid": 0, "mode": mode, "paxMetadata": {}}
        base = {"elf": {"/lib/libc.so": record("a")}, "links": {"/lib/loader": "libc.so"}}
        actual = copy.deepcopy(base)
        actual["elf"].update({"/lake-sink": record("b"), "/kanz-lake-snapshot": record("c", 0o555)})
        self.assertEqual(gate.validate_hybrid_native(base, actual, "b" * 64, "c" * 64), 3)
        for mutation in ("extra", "missing", "library", "link", "worker-mode", "owner", "go-hash", "library-capability", "worker-capability"):
            bad = copy.deepcopy(actual)
            if mutation == "extra": bad["elf"]["/bin/extra"] = record("d")
            if mutation == "missing": del bad["elf"]["/lake-sink"]
            if mutation == "library": bad["elf"]["/lib/libc.so"]["sha256"] = "e" * 64
            if mutation == "link": bad["links"]["/lib/loader"] = "/lake-sink"
            if mutation == "worker-mode": bad["elf"]["/kanz-lake-snapshot"]["mode"] = 0o755
            if mutation == "owner": bad["elf"]["/kanz-lake-snapshot"]["uid"] = 65532
            if mutation == "go-hash": bad["elf"]["/lake-sink"]["sha256"] = "e" * 64
            if mutation == "library-capability": bad["elf"]["/lib/libc.so"]["paxMetadata"] = {"SCHILY.xattr.security.capability": "a" * 64}
            if mutation == "worker-capability": bad["elf"]["/kanz-lake-snapshot"]["paxMetadata"] = {"SCHILY.xattr.security.capability": "a" * 64}
            with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                gate.validate_hybrid_native(base, bad, "b" * 64, "c" * 64)

    def test_tar_inventory_never_extracts_and_refuses_aliases(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "image.tar"
            for mutation in ("none", "duplicate", "traversal", "hardlink", "device"):
                with tarfile.open(path, "w") as output:
                    entry = tarfile.TarInfo("bin/worker")
                    entry.size, entry.mode = 8, 0o555
                    output.addfile(entry, io.BytesIO(b"\x7fELFtest"))
                    if mutation != "none":
                        other = tarfile.TarInfo("bin/worker" if mutation == "duplicate" else "../escape" if mutation == "traversal" else "other")
                        if mutation == "hardlink":
                            other.type, other.linkname = tarfile.LNKTYPE, "bin/worker"
                        if mutation == "device": other.type = tarfile.CHRTYPE
                        output.addfile(other)
                if mutation == "none":
                    self.assertEqual(set(gate.elf_inventory(path)["elf"]), {"/bin/worker"})
                    self.assertFalse((Path(directory) / "bin").exists())
                else:
                    with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                        gate.elf_inventory(path)

    def test_extended_tar_metadata_is_bounded_before_parser(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "image.tar"
            for kind in (tarfile.XHDTYPE, tarfile.XGLTYPE, tarfile.GNUTYPE_LONGNAME):
                header = tarfile.TarInfo("metadata")
                header.type, header.size = kind, 1024**3
                path.write_bytes(header.tobuf() + bytes(1024))
                with patch.object(gate.tarfile, "open", side_effect=AssertionError("parser entered")):
                    with self.assertRaises(ValueError):
                        gate.elf_inventory(path)
            # Ordinary Docker xattrs/PAX remain supported without extraction.
            with tarfile.open(path, "w", format=tarfile.PAX_FORMAT) as output:
                member = tarfile.TarInfo("worker")
                member.size = 8
                member.pax_headers = {"SCHILY.xattr.security.capability": "fixture", "mtime": "1.5"}
                output.addfile(member, io.BytesIO(b"\x7fELFtest"))
            inventory = gate.elf_inventory(path)
            self.assertEqual(set(inventory["elf"]), {"/worker"})
            self.assertEqual(set(inventory["elf"]["/worker"]["paxMetadata"]), {"SCHILY.xattr.security.capability"})

    def test_go_buildinfo_source_and_spdx_are_independently_bound(self):
        sdk = "github.com/eighred/kanz/kanz-schemas-go"
        deps = [{"Path": "example.org/runtime", "Version": "v1.2.3", "Sum": "h1:fixture"},
                {"Path": sdk, "Version": "v0.0.0-00010101000000-000000000000",
                 "Replace": {"Path": "../kanz-schemas/gen/go", "Version": "(devel)"}}]
        info = {"Path": gate.image_recipe("lake-sink").go_main, "GoVersion": "go1.26.10",
                "Main": {"Path": "github.com/eighred/kanz", "Version": "(devel)"}, "Deps": deps,
                "Settings": [{"Key": k, "Value": v} for k, v in {"GOOS": "linux", "GOARCH": "amd64",
                    "CGO_ENABLED": "0", "-buildmode": "exe", "-trimpath": "true"}.items()]}
        expected = {d["Path"]: copy.deepcopy(d) for d in deps}
        sums = {("example.org/runtime", "v1.2.3"): "h1:fixture"}
        sbom = {"packages": [{"name": n, "versionInfo": v, "externalRefs": [{"referenceType": "purl",
                 "referenceLocator": "pkg:golang/" + n + ("@" + v if v else "")}]} for n, v in
                 (("example.org/runtime", "v1.2.3"), (sdk, ""))]}
        # Actual Syft1.56 SPDX shape from failed65cb: the PURL remains versionless.
        sbom["packages"][1]["versionInfo"] = "UNKNOWN"
        self.assertEqual(gate.validate_go_buildinfo(info, expected, sums, "go1.26.10", sbom), 2)
        for mutation in ("main", "version", "dep", "sdk", "omission", "checksum", "spdx", "settings"):
            bad, catalog, checksums = copy.deepcopy(info), copy.deepcopy(sbom), dict(sums)
            if mutation == "main": bad["Path"] = "other"
            if mutation == "version": bad["GoVersion"] = "go1.26.1"
            if mutation == "dep": bad["Deps"][0]["Version"] = "v0.0.0"
            if mutation == "sdk": bad["Deps"][1]["Replace"]["Path"] = "../evil"
            if mutation == "omission": bad["Deps"].pop()
            if mutation == "checksum": checksums.clear()
            if mutation == "spdx": catalog["packages"].pop()
            if mutation == "settings": bad["Settings"][0]["Value"] = "windows"
            with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                gate.validate_go_buildinfo(bad, expected, checksums, "go1.26.10", catalog)

        versioned = copy.deepcopy(info)
        versioned["Deps"][1] = {"Path":sdk,"Version":"v1.2.3","Sum":"h1:versioned"}
        versioned_graph = {d["Path"]:copy.deepcopy(d) for d in versioned["Deps"]}
        versioned_sums = {**sums,(sdk,"v1.2.3"):"h1:versioned"}
        versioned_sbom = copy.deepcopy(sbom)
        versioned_sbom["packages"][1]["versionInfo"] = "v1.2.3"
        versioned_sbom["packages"][1]["externalRefs"][0]["referenceLocator"] += "@v1.2.3"
        self.assertEqual(gate.validate_go_buildinfo(versioned,versioned_graph,versioned_sums,"go1.26.10",versioned_sbom),2)
        for mutation in ("unknown", "duplicate-identity"):
            bad = copy.deepcopy(versioned_sbom)
            if mutation == "unknown": bad["packages"][1]["versionInfo"] = "UNKNOWN"
            else: bad["packages"].append({**copy.deepcopy(bad["packages"][1]),"name":"example.org/disguised"})
            with self.subTest(versioned_sdk=mutation),self.assertRaises(ValueError):
                gate.validate_go_buildinfo(versioned,versioned_graph,versioned_sums,"go1.26.10",bad)

        for mutation in ("sdk-versioned", "sdk-replacement", "sdk-name", "sdk-purl", "sdk-versioned-purl",
                         "sdk-purl-qualifier", "sdk-duplicate", "sdk-conflict", "sdk-two-purls",
                         "dependency-unknown", "sdk-empty", "sdk-devel", "sdk-disguised-versioned"):
            bad, graph, catalog = copy.deepcopy(info), copy.deepcopy(expected), copy.deepcopy(sbom)
            checksums = dict(sums)
            row = catalog["packages"][1]
            if mutation == "sdk-versioned":
                bad["Deps"][1].pop("Replace"); bad["Deps"][1]["Sum"] = "h1:versioned"
                graph[sdk] = copy.deepcopy(bad["Deps"][1]); checksums[(sdk,bad["Deps"][1]["Version"])] = "h1:versioned"
            if mutation == "sdk-replacement":
                bad["Deps"][1]["Replace"]["Path"] = "../evil"; graph[sdk] = copy.deepcopy(bad["Deps"][1])
            if mutation == "sdk-name": row["name"] = "example.org/other"
            if mutation == "sdk-purl": row["externalRefs"][0]["referenceLocator"] = "pkg:golang/example.org/other"
            if mutation == "sdk-versioned-purl": row["externalRefs"][0]["referenceLocator"] += "@UNKNOWN"
            if mutation == "sdk-purl-qualifier": row["externalRefs"][0]["referenceLocator"] += "?goos=linux"
            if mutation == "sdk-duplicate": catalog["packages"].append(copy.deepcopy(row))
            if mutation == "sdk-conflict": catalog["packages"].append({**copy.deepcopy(row),"versionInfo":"v1.0.0"})
            if mutation == "sdk-two-purls": row["externalRefs"].append(copy.deepcopy(row["externalRefs"][0]))
            if mutation == "dependency-unknown": catalog["packages"][0]["versionInfo"] = "UNKNOWN"
            if mutation == "sdk-empty": row["versionInfo"] = ""
            if mutation == "sdk-devel": row["versionInfo"] = "(devel)"
            if mutation == "sdk-disguised-versioned":
                extra = copy.deepcopy(row); extra["name"] = "example.org/disguised"
                extra["versionInfo"] = "v1.2.3"; extra["externalRefs"][0]["referenceLocator"] += "@v1.2.3"
                catalog["packages"].append(extra)
            with self.subTest(spdx_mutation=mutation), self.assertRaises(ValueError):
                gate.validate_go_buildinfo(bad, graph, checksums, "go1.26.10", catalog)

    def test_go_json_verifies_scan_but_never_substitutes_for_plain_exit(self):
        config = {"protocol_version": "v1.0.0", "scanner_name": "govulncheck", "scanner_version": "v1.6.0",
                  "scan_mode": "binary", "scan_level": "symbol", "db": "https://vuln.go.dev",
                  "db_last_modified": "2026-11-05T23:49:57Z"}
        modules = {("github.com/eighred/kanz", "(devel)"), ("stdlib", "go1.26.10")}
        records = [{"config": config}, {"SBOM": {"go_version": "go1.26.10", "roots": ["github.com/eighred/kanz"],
                   "modules": [{"path": p, "version": v} for p, v in sorted(modules)]}},
                   {"finding": {"osv": "GO-2026-1234"}}]
        encode = lambda r: "\n".join(json.dumps(x) for x in r)
        self.assertEqual(gate.validate_go_audit(encode(records), "go1.26.10", modules)["findingIDs"], ["GO-2026-1234"])
        for mutation in ("empty", "config", "mode", "root", "modules", "db", "duplicate", "truncated"):
            bad = copy.deepcopy(records)
            if mutation == "empty": bad.clear()
            if mutation == "config": bad.pop(0)
            if mutation == "mode": bad[0]["config"]["scan_mode"] = "source"
            if mutation == "root": bad[1]["SBOM"]["roots"] = ["other"]
            if mutation == "modules": bad[1]["SBOM"]["modules"].pop()
            if mutation == "db": del bad[0]["config"]["db_last_modified"]
            if mutation == "duplicate": bad.append(bad[1])
            raw = encode(bad) + ('{' if mutation == "truncated" else '')
            with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                gate.validate_go_audit(raw, "go1.26.10", modules)

    @unittest.skipUnless(sys.platform == "linux", "actual process-group ownership requires Linux")
    def test_actual_bounded_tool_output_and_timeout(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            gate.bounded_command([sys.executable, "-c", "print('complete')"], root / "ok", 64, timeout=5)
            self.assertEqual((root / "ok").read_text(), "complete\n")
            cases = {"stdout": "import sys;sys.stdout.buffer.write(b'x'*131072)",
                     "stderr": "import sys;sys.stderr.buffer.write(b'x'*131072)",
                     "deadline": "import time;time.sleep(30)"}
            for name, source in cases.items():
                with self.subTest(case=name), self.assertRaises(ValueError):
                    gate.bounded_command([sys.executable, "-c", source], root / name, 64, timeout=0.2)
                self.assertLessEqual((root / name).stat().st_size, 64)

    @unittest.skipUnless(sys.platform == "linux", "actual process-group ownership requires Linux")
    def test_existing_destination_refuses_before_spawn(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "existing"
            path.write_bytes(b"preserved")
            with patch.object(gate.subprocess, "Popen", side_effect=AssertionError("must not spawn")):
                with self.assertRaises(FileExistsError):
                    gate.bounded_command([sys.executable, "-c", "pass"], path, 64)
            self.assertEqual(path.read_bytes(), b"preserved")

    @unittest.skipUnless(sys.platform == "linux", "actual process-group ownership requires Linux")
    def test_reader_setup_failure_kills_and_joins_actual_child(self):
        spawned = []
        original = gate.subprocess.Popen
        def acquire(*args, **kwargs):
            child = original(*args, **kwargs)
            spawned.append(child)
            return child
        with tempfile.TemporaryDirectory() as directory, patch.object(gate.subprocess, "Popen", side_effect=acquire), \
                patch.object(gate.threading.Thread, "start", side_effect=RuntimeError("setup refused")):
            with self.assertRaises(RuntimeError):
                gate.bounded_command([sys.executable, "-c", "import time;time.sleep(30)"], Path(directory) / "out", 64)
        self.assertEqual(len(spawned), 1)
        self.assertIsNotNone(spawned[0].poll())
        self.assertTrue(spawned[0].stdout.closed)
        self.assertTrue(spawned[0].stderr.closed)

    def test_native_base_uses_docker_canonical_repository_digest(self):
        expected = gate.CC_RUNTIME.replace(":nonroot@", "@")
        base = {"RepoDigests": [expected], "Os": "linux", "Architecture": "amd64", "Id": "sha256:" + "a" * 64}
        self.assertEqual(gate.validate_native_base_identity(base), expected)
        for key, value in (("RepoDigests", [expected.replace("eighred", "other")]),
                           ("RepoDigests", [expected[:-1] + "0"]), ("Architecture", "arm64"),
                           ("Os", "windows"), ("Id", "missing")):
            bad = copy.deepcopy(base)
            bad[key] = value
            with self.subTest(field=key), self.assertRaises(ValueError):
                gate.validate_native_base_identity(bad)

    def test_both_services_require_actual_runtime_catalog(self):
        for service in gate.SERVICES:
            data, lock, sbom = fixture(service)
            self.assertEqual(gate.validate_inventory(data, lock, service, sbom), (len(data["packages"]), len(data["packages"])))

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

    def test_hybrid_extra_gate_must_complete_before_pass(self):
        self.orchestrate(service="lake-sink", fail_hybrid=True)
        self.orchestrate(service="lake-sink")

    def orchestrate(self, fail_audit=False, fail_copy=False, service="binance-depth", fail_hybrid=False):
        data, _, sbom = fixture(service)
        lock = tomllib.loads((Path(gate.__file__).resolve().parents[2] / "kanz-rs/Cargo.lock").read_text())
        versions = {p["name"]: p["version"] for p in lock["package"]}
        for package, entry in zip(data["packages"], sbom["packages"]):
            version = versions[package["name"]]
            package["version"] = entry["versionInfo"] = version
            entry["externalRefs"][0]["referenceLocator"] = f"pkg:cargo/{package['name']}@{version}"
        calls = []
        image = "ghcr.io/eighred/" + service + "@sha256:" + "a" * 64

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
            argv = ["gate", "--service", service, "--image", image,
                    "--sbom", str(path / "sbom.json"), "--output", str(path / "evidence")]
            with patch("sys.argv", argv), patch.object(gate, "run", side_effect=fake_run), \
                 patch.object(gate, "output", side_effect=fake_output), \
                 patch.object(gate.shutil, "which", return_value="/pinned/cargo-audit"), \
                 patch.dict(gate.os.environ, {"SECRET": "must-not-inherit"}), \
                 patch.object(gate, "verify_hybrid", side_effect=ValueError("hybrid refused") if fail_hybrid else None, return_value={"validated": True}) as hybrid:
                if fail_hybrid:
                    with self.assertRaises(ValueError): gate.main()
                elif fail_copy or fail_audit:
                    with self.assertRaises(subprocess.CalledProcessError): gate.main()
                else:
                    gate.main()
            evidence = json.loads((path / "evidence/evidence.json").read_text())
            self.assertEqual(evidence["passed"], not (fail_copy or fail_audit or fail_hybrid))
            self.assertEqual(hybrid.call_count, int(service == "lake-sink" and not (fail_copy or fail_audit)))
            self.assertIn(["docker", "rm", "container"], calls)
            self.assertFalse(any(c[:2] in (["docker", "run"], ["docker", "start"], ["docker", "exec"]) for c in calls))
            if not (fail_copy or fail_audit):
                audits = [c for c in calls if c[0] == "/pinned/cargo-audit"]
                self.assertEqual(len(audits), 2)
                self.assertIn("--file", audits[0])
                self.assertIn("bin", audits[1])


if __name__ == "__main__":
    unittest.main()
