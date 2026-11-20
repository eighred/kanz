import copy
import io
import tarfile
import sys
import json
from pathlib import Path
import subprocess
import tempfile
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


class TemporalSourceTests(unittest.TestCase):
    def test_protected_builder_inputs_are_independently_bound(self):
        import temporal_rust_release_evidence as source

        name, version = "arrow-array", "59.3.2"
        relative = f"rust/temporal_sources/generated/{name}-{version}/Cargo.toml"
        # This production writer pin comes from the independently qualified Apache archive.
        apache = {"name": name, "version": version, "manifestPath": relative,
                  "patchSHA256": "5c107560c2cf08797275fbd78000badc64d3c4c44e2d41e5bccb561e27e5b908",
                  "treeSHA256": "1bb87fa6d3991524611c6465453ce8eeff4af49658c389ac552936ff9c7b506f",
                  "upstream": {"kind": "registry-archive", "url": f"https://static.crates.io/crates/{name}/{name}-{version}.crate",
                               "sha256": "72b5b1682924f11ecf6929689c16cbc69efbb6dd141b06ad98ee1cb10d858c43"}}
        fork = "sad-chrono"
        fork_hash = "59944182ae44e540eb6a61059eefb6d437b4bb3a7eff26e75149ed35601b7c5f"
        source_sha, db_sha = "a" * 40, "b" * 40
        worker = source.WORKER
        worker_path = "/src/rust/target-provenance/release/" + worker
        lock = {"package": [{"name": name, "version": version}, {"name": fork, "version": "0.5.1",
                             "checksum": fork_hash, "source": source.REGISTRY}]}
        embedded = {"packages": [{"name": name, "version": version, "source": "local"},
                                 {"name": fork, "version": "0.5.1", "source": "crates.io"}]}
        sbom = {"packages": [{"name": row["name"], "versionInfo": row["version"], "externalRefs": [
                {"referenceType": "purl", "referenceLocator": f"pkg:cargo/{row['name']}@{row['version']}"}]}
                for row in embedded["packages"]]}
        policy = json.dumps({"schema": "kanz.apache-temporal-sources.v1", "packages": [apache]}).encode()
        api_id = "path+file:///src/rust/temporal_sources/generated/arrow-array-59.3.2#arrow-array@59.3.2"
        fork_id = source.REGISTRY + "#sad-chrono@0.5.1"
        package_rows = [{**apache, "packageID": api_id}, {"packageID": fork_id, "name": fork, "version": "0.5.1",
                        "upstream": {"kind": "registry-archive", "url": "https://static.crates.io/crates/sad-chrono/sad-chrono-0.5.1.crate",
                                     "sha256": fork_hash}}]
        def artifact(identifier, manifest, lib, label, exe=None):
            return {"reason": "compiler-artifact", "package_id": identifier, "manifest_path": manifest,
                    "target": {"name": label, "kind": ["lib" if lib else "bin"]}, "profile": {"test": False},
                    "fresh": False, "executable": exe, "filenames": [exe] if exe else []}
        compiler = [artifact(api_id, "/src/" + relative, True, "arrow_array"),
                    artifact(fork_id, "/cargo/registry/src/index/sad-chrono-0.5.1/Cargo.toml", True, "sad_chrono"),
                    artifact("path+file:///src/rust/services/lake_snapshot#kanz-lake-snapshot@0.1.0",
                             "/src/rust/services/lake_snapshot/Cargo.toml", False, worker, worker_path),
                    {"reason": "build-finished", "success": True}]
        snapshot = {"rustsec_commit": db_sha}
        advisory = json.dumps(snapshot).encode()
        receipt = {"schema": source.SCHEMA, "service": "lake-sink", "target": "x86_64-unknown-linux-gnu",
                   "sourceRevision": source_sha, "binarySHA256": "c" * 64, "lockSHA256": "d" * 64,
                   "patchSetSHA256": source.sha256(policy), "advisorySnapshotSHA256": source.sha256(advisory),
                   "packages": package_rows}
        with tempfile.TemporaryDirectory() as temp:
            export = Path(temp)
            folder = export / "kanz-source-evidence"
            folder.mkdir()
            (export / "provenance.json").write_text(json.dumps({"_type": "https://in-toto.io/Statement/v1"}))
            for change in ("valid", "root-only", "source", "binary", "lock", "patch-set", "tree", "missing-compiler", "registry-copy",
                           "cached-library", "debug-worker", "spdx-source", "elf-source", "new-disclosure", "unreviewed",
                           "extra-compiler-origin", "incomplete", "duplicate-lock", "legacy-package", "target-name", "encoded-spdx-alias"):
                with self.subTest(change=change):
                    data, build, elf, spdx, rows = map(copy.deepcopy, (receipt, compiler, embedded, sbom, lock))
                    if change == "source": data["sourceRevision"] = "e" * 40
                    if change == "binary": data["binarySHA256"] = "e" * 64
                    if change == "lock": data["lockSHA256"] = "e" * 64
                    if change == "patch-set": data["patchSetSHA256"] = "e" * 64
                    if change == "tree": data["packages"][0]["treeSHA256"] = "e" * 64
                    if change == "missing-compiler": build.pop(0)
                    if change == "registry-copy": build[0]["package_id"] = source.REGISTRY + "#arrow-array@59.3.2"
                    if change == "cached-library": build[0]["fresh"] = True
                    if change == "debug-worker": build[2]["executable"] = worker_path.replace("/release/", "/debug/")
                    if change == "spdx-source": spdx["packages"][0]["externalRefs"][0]["referenceLocator"] += "?source=elsewhere"
                    if change == "elf-source": elf["packages"][0]["source"] = "crates.io"
                    if change == "unreviewed": data["packages"].pop(0)
                    if change == "extra-compiler-origin": build.insert(0, {**build[0], "package_id": source.REGISTRY + "#arrow-array@59.3.2"})
                    if change == "incomplete": build.pop()
                    if change == "duplicate-lock": rows["package"].append(copy.deepcopy(rows["package"][0]))
                    if change == "legacy-package": rows["package"].append({"name": "chrono", "version": "0.4.45"})
                    if change == "target-name": build[0]["target"]["name"] = "other"
                    if change == "encoded-spdx-alias":
                        spdx["packages"].append({"name": "other", "versionInfo": version, "externalRefs": [
                            {"referenceType": "purl", "referenceLocator": "pkg:cargo/%61rrow-array@59.3.2"}]})
                    (folder / "compiler.jsonl").write_text("\n".join(map(json.dumps, build)))
                    receipt_path = folder / "provenance.json"
                    receipt_path.write_text(json.dumps(data))
                    if change == "root-only":
                        receipt_path.replace(export / "provenance.json")
                    (folder / "chrono-advisories.json").write_bytes(advisory)
                    observed = []
                    def evaluate(s, v, c):
                        if s.get("new-fork-finding"):
                            raise ValueError("new disclosure")
                        observed.append((s, v, c))
                    current = {"rustsec_commit": "f" * 40}
                    if change == "new-disclosure": current["new-fork-finding"] = True
                    def check():
                        return source.verify(export, policy, source_sha, "d" * 64, "c" * 64, rows, elf, spdx,
                                             lambda: current, evaluate)
                    if change == "valid":
                        evidence, saved_snapshot = check()
                        self.assertEqual(evidence["reviewedPackages"], 2)
                        self.assertEqual(evidence["releaseAdvisorySnapshotSHA256"], source.sha256(saved_snapshot))
                        self.assertEqual(json.loads(saved_snapshot), current)
                        self.assertEqual(observed, [(snapshot, "0.5.1", fork_hash), (current, "0.5.1", fork_hash)])
                    else:
                        with self.assertRaises(ValueError):
                            check()


class EvidenceTests(unittest.TestCase):
    def test_inventory_create_never_starts_and_cleans_owned_container(self):
        image = gate.CC_RUNTIME
        container = "a" * 64
        for failure in (None, "export", "remove", "identity"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as directory:
                temp = Path(directory); calls = []
                def create(args):
                    calls.append(args)
                    self.assertEqual(args,["docker","create","--entrypoint","/__kanz_inventory_not_executed__",image])
                    return "invalid" if failure == "identity" else container
                def export(args, destination, limit):
                    calls.append(args)
                    self.assertEqual(args,["docker","export",container])
                    self.assertEqual(destination,temp/"base.tar")
                    self.assertEqual(limit,2*1024**3)
                    if failure == "export": raise ValueError("synthetic export refusal")
                    destination.write_bytes(b"bounded synthetic archive")
                def remove(args):
                    calls.append(args)
                    self.assertEqual(args,["docker","rm","-f",container])
                    if failure == "remove": raise ValueError("synthetic removal refusal")
                with patch.object(gate,"output",side_effect=create), patch.object(gate,"bounded_command",side_effect=export), \
                     patch.object(gate,"run",side_effect=remove), patch.object(gate,"elf_inventory",return_value={"verified":"fixture"}) as inventory:
                    if failure is None:
                        self.assertEqual(gate.export_inventory(image,temp,"base"),{"verified":"fixture"})
                        inventory.assert_called_once_with(temp/"base.tar")
                        self.assertFalse((temp/"base.tar").exists())
                    else:
                        with self.assertRaises(ValueError):gate.export_inventory(image,temp,"base")
                        inventory.assert_not_called()
                self.assertEqual([c[1] for c in calls],["create"] if failure=="identity" else ["create","export","rm"])

    def test_closed_hybrid_recipe_requires_exact_paths(self):
        recipe = gate.image_recipe("lake-sink")
        self.assertEqual(recipe.rust_path, "/kanz-lake-snapshot")
        self.assertEqual(recipe.rust_root, "kanz-lake-snapshot")
        self.assertEqual(recipe.go_path, "/lake-sink")
        self.assertEqual(recipe.go_main, "github.com/eighred/kanz/services/lake_sink/cmd/lake_sink")
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
                 "Replace": {"Path": "../schemas/gen/go", "Version": "(devel)"}}]
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

    def test_workspace_and_extracted_binary_have_independent_results(self):
        for lock in ("clean", "finding"):
            for binary in ("clean", "finding"):
                with self.subTest(lock=lock, binary=binary):
                    self.orchestrate(audits=(lock, binary))

    def test_failed_scanner_and_invalid_evidence_refuse_both_release_paths(self):
        for defect in ("empty", "malformed", "no-fields", "wrong-db", "duplicate", "other-input",
                       "ignored", "bad-count", "bad-type", "zero-with-warning",
                       "zero-with-vulnerability", "nonzero-clean", "timeout"):
            for audits in ((defect, "clean"), ("clean", defect)):
                with self.subTest(audits=audits):
                    self.orchestrate(audits=audits, service="lake-sink")

    def test_extraction_failure_removes_container(self):
        self.orchestrate(fail_copy=True)

    def test_hybrid_extra_gate_must_complete_before_pass(self):
        self.orchestrate(service="lake-sink", fail_hybrid=True)
        self.orchestrate(service="lake-sink")

    def test_selected_fork_requires_builder_evidence_in_release_entrypoint(self):
        self.orchestrate(service="lake-sink", missing_temporal=True)

    def orchestrate(self, audits=("clean", "clean"), fail_copy=False, service="binance-depth", fail_hybrid=False,
                    missing_temporal=False):
        data, lock, sbom = fixture(service)
        lock["package"].append({"name": "other-workspace-member", "version": "1.1.0"})
        if missing_temporal:
            lock["package"].append({"name": "sad-chrono", "version": "0.5.1"})
        fail_audit = any(outcome != "clean" for outcome in audits)
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
                self.assertIs(kwargs["stderr"], subprocess.DEVNULL)
                self.assertEqual(args[args.index("--deny") + 1], "warnings")
                binary = "bin" in args
                outcome = audits[1 if binary else 0]
                package_counts = (len(lock["package"]), len(data["packages"]))
                report = {"database": {"last-commit": "c" * 40}, "settings": {"ignore": []},
                          "lockfile": {"dependency-count": package_counts[binary]},
                          "vulnerabilities": {"found": False, "count": 0, "list": []}, "warnings": {}}
                if outcome in ("finding", "zero-with-warning"):
                    report["warnings"] = {"unmaintained": [{"kind": "unmaintained"}]}
                elif outcome == "zero-with-vulnerability":
                    report["vulnerabilities"] = {"found": True, "count": 1, "list": [{}]}
                elif outcome == "wrong-db":
                    report["database"]["last-commit"] = "d" * 40
                elif outcome == "other-input":
                    report["lockfile"]["dependency-count"] = package_counts[not binary]
                elif outcome == "ignored":
                    report["settings"]["ignore"] = ["RUSTSEC-fixture"]
                elif outcome == "bad-count":
                    report["vulnerabilities"]["count"] = 1
                elif outcome == "bad-type":
                    report["vulnerabilities"]["found"] = 0
                elif outcome == "timeout":
                    raise subprocess.TimeoutExpired(args, 900)
                payload = {"empty": "", "malformed": "incomplete JSON", "no-fields": "{}"}.get(outcome)
                if outcome == "duplicate":
                    payload = '{"warnings":{"unmaintained":[{}]},' + json.dumps(report)[1:]
                kwargs["stdout"].write(json.dumps(report) if payload is None else payload)
                if outcome in ("finding", "nonzero-clean"):
                    raise subprocess.CalledProcessError(1, args)

        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            (path / "rust").mkdir()
            (path / "rust/Cargo.lock").write_text("".join(
                f'[[package]]\nname = "{entry["name"]}"\nversion = "{entry["version"]}"\n'
                for entry in lock["package"]))
            (path / "sbom.json").write_text(json.dumps(sbom))
            argv = ["gate", "--service", service, "--image", image,
                    "--sbom", str(path / "sbom.json"), "--output", str(path / "evidence")]
            with patch("sys.argv", argv), patch.object(gate, "run", side_effect=fake_run), \
                 patch.object(gate, "__file__", str(path / ".github/scripts/rust_release_evidence.py")), \
                 patch.object(gate, "output", side_effect=fake_output), \
                 patch.object(gate.shutil, "which", return_value="/pinned/cargo-audit"), \
                 patch.dict(gate.os.environ, {"SECRET": "must-not-inherit"}), \
                 patch.object(gate, "verify_hybrid", side_effect=ValueError("hybrid refused") if fail_hybrid else None, return_value={"validated": True}) as hybrid:
                if missing_temporal:
                    with self.assertRaisesRegex(ValueError, "requires protected builder evidence"):
                        gate.main()
                elif fail_hybrid or fail_audit:
                    with self.assertRaises(ValueError): gate.main()
                elif fail_copy:
                    with self.assertRaises(subprocess.CalledProcessError): gate.main()
                else:
                    gate.main()
            evidence = json.loads((path / "evidence/evidence.json").read_text())
            self.assertEqual(evidence["passed"], not (fail_copy or fail_audit or fail_hybrid or missing_temporal))
            self.assertEqual(hybrid.call_count, int(service == "lake-sink" and not (fail_copy or fail_audit or missing_temporal)))
            self.assertIn(["docker", "rm", "container"], calls)
            self.assertFalse(any(c[:2] in (["docker", "run"], ["docker", "start"], ["docker", "exec"]) for c in calls))
            scans = [c for c in calls if c[0] == "/pinned/cargo-audit"]
            if fail_copy:
                self.assertEqual(scans, [])
                self.assertNotIn("audits", evidence)
                return
            self.assertEqual(len(scans), 2)
            self.assertEqual(scans[0][-2:], ["--file", str(path / "rust/Cargo.lock")])
            self.assertEqual(scans[1][-2], "bin")
            self.assertEqual(Path(scans[1][-1]).name, gate.image_recipe(service).rust_root)
            for name, outcome in zip(("lock", "binary"), audits):
                receipt = evidence["audits"][name]
                self.assertEqual(receipt["passed"], outcome == "clean")
                expected_exit = {"finding": 1, "nonzero-clean": 1, "timeout": None}.get(outcome, 0)
                self.assertEqual(receipt["exitCode"], expected_exit)
                retained = outcome in ("clean", "finding", "zero-with-warning", "zero-with-vulnerability", "nonzero-clean")
                artifact = path / "evidence" / f"audit-{name}.json"
                self.assertEqual(artifact.exists(), retained)
                if retained:
                    self.assertEqual(receipt["reportSHA256"], gate.digest(artifact))
                    self.assertEqual(evidence[name + "AdvisoryDBCommit"], "c" * 40)
                else:
                    self.assertNotIn("reportSHA256", receipt)
                    self.assertNotIn(name + "AdvisoryDBCommit", evidence)


if __name__ == "__main__":
    unittest.main()
