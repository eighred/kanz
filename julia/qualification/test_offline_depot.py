"""Closed input and scratch-write regressions; run only in the bounded sandbox."""

import copy
import hashlib
import io
import os
import json
from pathlib import Path
import stat
import tarfile
import tempfile
import unittest

import offline_depot as depot


PACKAGE_UUID = "12345678-1234-1234-1234-123456789abc"
PROJECT = (b'name = "Demo"\nuuid = "12345678-1234-1234-1234-123456789abc"\n'
           b'version = "1.0.0"\n')
FILES = {"Project.toml": ("100644", PROJECT),
         "src/Demo.jl": ("100644", b"module Demo\nend\n"),
         "bin/demo": ("100755", b"#!/bin/sh\nexit 0\n")}
# Independently computed using Git's hash-object blob/tree commands, including
# the executable entry and two nested trees. No Julia/production helper ran.
TREE = "42bf87e53c649f22bae049661e39529644a0ec2b"
MANIFEST = (b'julia_version = "1.13.3"\nmanifest_format = "2.1"\n'
            b'[[deps.Demo]]\nuuid = "12345678-1234-1234-1234-123456789abc"\n'
            b'version = "1.0.0"\ngit-tree-sha1 = "' + TREE.encode() + b'"\n')


def archive(files=None, extras=()):
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w:gz", format=tarfile.GNU_FORMAT) as tar:
        for name, (mode, raw) in (FILES if files is None else files).items():
            member = tarfile.TarInfo(name)
            member.mode = 0o755 if mode == "100755" else 0o644
            member.size = len(raw)
            tar.addfile(member, io.BytesIO(raw))
        for member, raw in extras:
            tar.addfile(member, io.BytesIO(raw) if member.isfile() else None)
    return output.getvalue()


def pin(raw, files=None, tree=TREE, entries=None):
    files = FILES if files is None else files
    return {"uuid": PACKAGE_UUID, "version": "1.0.0", "treeSHA1": tree,
            "archiveSHA256": hashlib.sha256(raw).hexdigest(),
            "archiveBytes": len(raw), "entries": len(files) if entries is None else entries,
            "expandedBytes": sum(len(v[1]) for v in files.values())}


def contract(raw):
    return {"schema": "kanz-julia-package-input-v1", "juliaVersion": "1.13.3",
            "manifestSHA256": hashlib.sha256(MANIFEST).hexdigest(),
            "packages": {"Demo": pin(raw)}}


class InputIdentity(unittest.TestCase):
    def test_git_tree_matches_independent_nested_executable_fixture(self):
        self.assertEqual(depot.git_tree(FILES), TREE)
        changed = dict(FILES, **{"bin/demo": ("100644", FILES["bin/demo"][1])})
        self.assertNotEqual(depot.git_tree(changed), TREE)

    def test_crc32c_uses_published_vector_and_chained_input(self):
        self.assertEqual(depot.crc32c(b"123456789"), 0xE3069283)
        self.assertEqual(depot.crc32c(b"456789", depot.crc32c(b"123")), 0xE3069283)
        self.assertEqual(depot.crc32c(b""), 0)

    def test_valid_closed_plan_preserves_declared_identity(self):
        raw = archive()
        planned = depot.package_plan(MANIFEST, contract(raw))
        self.assertEqual(set(planned), {"Demo"})
        self.assertEqual(planned["Demo"]["treeSHA1"], TREE)
        self.assertRegex(planned["Demo"]["slug"], r"^[A-Za-z0-9]{5}$")
        self.assertEqual(depot.package_files(raw, "Demo", planned["Demo"]), FILES)

    def test_locked_input_drift_refuses(self):
        raw = archive()
        cases = {
            "manifest-byte": lambda c: c.update(manifestSHA256="0" * 64),
            "runtime": lambda c: c.update(juliaVersion="1.13.4"),
            "schema": lambda c: c.update(schema="unknown"),
            "missing-source": lambda c: c["packages"].clear(),
            "extra-source": lambda c: c["packages"].update(Other=c["packages"]["Demo"]),
            "uuid": lambda c: c["packages"]["Demo"].update(uuid="00000000-0000-0000-0000-000000000000"),
            "version": lambda c: c["packages"]["Demo"].update(version="1.0.1"),
            "tree": lambda c: c["packages"]["Demo"].update(treeSHA1="0" * 40),
            "boolean-bound": lambda c: c["packages"]["Demo"].update(archiveBytes=True),
            "oversized-bound": lambda c: c["packages"]["Demo"].update(archiveBytes=depot.MAX_ARCHIVE + 1),
            "extra-pin-field": lambda c: c["packages"]["Demo"].update(override="unreviewed"),
        }
        for name, mutate in cases.items():
            with self.subTest(name=name):
                value = copy.deepcopy(contract(raw))
                mutate(value)
                with self.assertRaises(ValueError):
                    depot.package_plan(MANIFEST, value)

    def test_duplicate_manifest_entry_refuses_even_with_matching_bytes(self):
        raw = archive()
        manifest = MANIFEST + MANIFEST[MANIFEST.index(b"[[deps.Demo]]"):]
        value = contract(raw)
        value["manifestSHA256"] = hashlib.sha256(manifest).hexdigest()
        with self.assertRaisesRegex(ValueError, "ambiguous manifest"):
            depot.package_plan(manifest, value)

    def test_unreviewed_source_and_manifest_format_refuse(self):
        raw = archive()
        cases = [MANIFEST.replace(b'"2.1"', b'"2.0"')]
        for field in [b'path = "/outside"', b'repo-url = "https://example.invalid"',
                      b'repo-rev = "unreviewed"']:
            cases.append(MANIFEST + field + b'\n')
        for manifest in cases:
            with self.subTest(manifest=manifest):
                value = contract(raw)
                value["manifestSHA256"] = hashlib.sha256(manifest).hexdigest()
                with self.assertRaises(ValueError):
                    depot.package_plan(manifest, value)


class ArchiveBoundary(unittest.TestCase):
    def test_archive_size_digest_tree_and_census_each_refuse(self):
        raw = archive()
        for field, value in [("archiveBytes", len(raw) - 1),
                             ("archiveSHA256", "0" * 64),
                             ("treeSHA1", "0" * 40), ("entries", 2),
                             ("expandedBytes", pin(raw)["expandedBytes"] - 1)]:
            with self.subTest(field=field):
                value_pin = pin(raw)
                value_pin[field] = value
                with self.assertRaises(ValueError):
                    depot.package_files(raw, "Demo", value_pin)

    def test_unsafe_paths_links_special_modes_and_duplicates_refuse(self):
        cases = []
        for path in ["../escape", "/escape", "src//escape", "src/./escape", "src\\escape", ""]:
            member = tarfile.TarInfo(path)
            member.size = 1
            cases.append((path, member, b"x"))
        for name, kind in [("symlink", tarfile.SYMTYPE), ("hardlink", tarfile.LNKTYPE),
                           ("fifo", tarfile.FIFOTYPE), ("device", tarfile.CHRTYPE)]:
            member = tarfile.TarInfo("bad-" + name)
            member.type = kind
            member.linkname = "/outside"
            cases.append((name, member, b""))
        mode = tarfile.TarInfo("setuid")
        mode.mode = 0o4755
        cases.append(("setuid", mode, b""))
        duplicate = tarfile.TarInfo("Project.toml")
        duplicate.size = len(PROJECT)
        cases.append(("duplicate", duplicate, PROJECT))
        for name, member, data in cases:
            with self.subTest(name=name):
                raw = archive(extras=[(member, data)])
                p = pin(raw, entries=4)
                p["expandedBytes"] += member.size
                with self.assertRaisesRegex(ValueError, "unsafe or ambiguous"):
                    depot.package_files(raw, "Demo", p)

    def test_invalid_output_component_refuses_before_materialization(self):
        for name in ["src/line\nbreak", "src/" + "x" * 256]:
            files = dict(FILES, **{name: ("100644", b"x")})
            raw = archive(files)
            with self.subTest(name=name), self.assertRaisesRegex(ValueError, "path component"):
                depot.package_files(raw, "Demo", pin(raw, files))

    def test_package_file_cannot_be_an_ancestor(self):
        files = dict(FILES, src=("100644", b"not a directory"))
        raw = archive(files)
        with self.assertRaisesRegex(ValueError, "also an ancestor"):
            depot.package_files(raw, "Demo", pin(raw, files))

    def test_project_metadata_must_name_the_admitted_package(self):
        raw = archive()
        with self.assertRaisesRegex(ValueError, "project metadata"):
            depot.package_files(raw, "Other", pin(raw))

    def test_valid_tree_with_additional_build_or_artifact_input_refuses(self):
        cases = [("Artifacts.toml", b"[dummy]\n", "a76e9d936cc5ad65ec4e5e7e762713e86499f903"),
                 ("deps/build.jl", b'error("must not execute")\n', "f39e1b1a66a33d4e9886b6847745e66dd263acb3")]
        for name, data, tree in cases:
            with self.subTest(name=name):
                files = dict(FILES, **{name: ("100644", data)})
                raw = archive(files)
                with self.assertRaisesRegex(ValueError, "separately qualified"):
                    depot.package_files(raw, "Demo", pin(raw, files, tree))


@unittest.skipUnless(os.name == "posix", "descriptor-relative staging requires Linux")
class ScratchBoundary(unittest.TestCase):
    def test_private_scratch_is_required(self):
        raw = archive()
        plan = depot.package_plan(MANIFEST, contract(raw))
        with tempfile.TemporaryDirectory() as directory:
            os.chmod(directory, 0o755)
            fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                with self.assertRaises(ValueError):
                    depot.stage_packages(plan, {"Demo": raw}, fd)
                self.assertEqual(os.listdir(fd), [])
            finally:
                os.close(fd)

    def test_input_links_special_files_and_oversized_files_refuse(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "regular").write_bytes(b"source")
            (root / "link").symlink_to("regular")
            os.link(root / "regular", root / "hardlink")
            os.mkfifo(root / "fifo")
            fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                for name in ["link", "hardlink", "regular", "fifo"]:
                    with self.subTest(name=name), self.assertRaises((OSError, ValueError)):
                        depot.read_input(fd, name, 8)
                (root / "large").write_bytes(b"123456789")
                with self.assertRaises(ValueError):
                    depot.read_input(fd, "large", 8)
                (root / "valid").write_bytes(b"exact")
                self.assertEqual(depot.read_input(fd, "valid", 8), b"exact")
            finally:
                os.close(fd)

    def test_unexpected_input_inventory_and_duplicate_pin_keys_refuse(self):
        raw = archive()
        with tempfile.TemporaryDirectory() as source, tempfile.TemporaryDirectory() as output:
            root = Path(source)
            (root / "offline_packages.json").write_text(json.dumps(contract(raw)))
            (root / "Manifest.toml").write_bytes(MANIFEST)
            (root / "Demo.tar.gz").write_bytes(raw)
            (root / "extra").write_bytes(b"not admitted")
            input_fd = os.open(source, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            output_fd = os.open(output, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                with self.assertRaisesRegex(ValueError, "inventory"):
                    depot.prepare(input_fd, output_fd)
                self.assertEqual(os.listdir(output_fd), [])
                (root / "extra").unlink()
                (root / "offline_packages.json").write_bytes(b'{"schema":1,"schema":2}')
                with self.assertRaisesRegex(ValueError, "duplicate"):
                    depot.prepare(input_fd, output_fd)
                self.assertEqual(os.listdir(output_fd), [])
            finally:
                os.close(input_fd)
                os.close(output_fd)

    def test_staging_copies_exact_bytes_with_no_consumer_acceptance(self):
        raw = archive()
        plan = depot.package_plan(MANIFEST, contract(raw))
        with tempfile.TemporaryDirectory() as source, tempfile.TemporaryDirectory() as directory:
            inputs = Path(source)
            (inputs / "Manifest.toml").write_bytes(MANIFEST)
            (inputs / "offline_packages.json").write_text(json.dumps(contract(raw)))
            (inputs / "Demo.tar.gz").write_bytes(raw)
            root = Path(directory)
            input_fd = os.open(source, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                result = depot.prepare(input_fd, fd)
                self.assertIs(result["consumerAccepted"], False)
                self.assertEqual(result["scope"], "external-source-packages-only")
                package = root / "packages" / "Demo" / plan["Demo"]["slug"]
                for path, (mode, expected) in FILES.items():
                    actual = package / path
                    self.assertEqual(actual.read_bytes(), expected)
                    self.assertEqual(stat.S_IMODE(actual.stat().st_mode), 0o555 if mode == "100755" else 0o444)
                self.assertEqual({p.relative_to(package).as_posix() for p in package.rglob("*") if p.is_file()}, set(FILES))
            finally:
                os.close(input_fd)
                os.close(fd)
                # Only the known fixture directories are made removable. No
                # target output is retained or copied outside sandbox scratch.
                for path in [root / "packages", root / "packages/Demo",
                             root / "packages/Demo" / plan["Demo"]["slug"]]:
                    if path.exists():
                        path.chmod(0o700)
                for name in ["src", "bin"]:
                    path = root / "packages/Demo" / plan["Demo"]["slug"] / name
                    if path.exists():
                        path.chmod(0o700)

    def test_all_archive_validation_precedes_any_output(self):
        raw = archive()
        plan = depot.package_plan(MANIFEST, contract(raw))
        with tempfile.TemporaryDirectory() as directory:
            fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                with self.assertRaises(ValueError):
                    depot.stage_packages(plan, {"Demo": raw + b"changed"}, fd)
                self.assertEqual(os.listdir(fd), [])
            finally:
                os.close(fd)

    def test_existing_foreign_output_refuses_without_mutation(self):
        raw = archive()
        plan = depot.package_plan(MANIFEST, contract(raw))
        with tempfile.TemporaryDirectory() as directory:
            marker = Path(directory) / "foreign"
            marker.write_bytes(b"retained")
            fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                with self.assertRaises(ValueError):
                    depot.stage_packages(plan, {"Demo": raw}, fd)
                self.assertEqual(marker.read_bytes(), b"retained")
                self.assertEqual(os.listdir(fd), ["foreign"])
            finally:
                os.close(fd)

    def test_path_components_and_existing_symlink_never_escape(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            victim = root / "victim"
            victim.write_bytes(b"retained")
            (root / "link").symlink_to(victim)
            fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                for name in ["..", "../escape", "/escape", "a/b", "a\\b", "\x00", ""]:
                    with self.subTest(name=name), self.assertRaises(ValueError):
                        depot.new_directory(fd, name)
                with self.assertRaises(FileExistsError):
                    depot.write_file(fd, "link", b"changed", False)
                self.assertEqual(victim.read_bytes(), b"retained")
            finally:
                os.close(fd)


if __name__ == "__main__":
    unittest.main()
