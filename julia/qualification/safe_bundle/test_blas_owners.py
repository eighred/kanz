import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest

from blas_owners import blob, exact_file, map_native, scan_results, BLAS, JULIA_INTERNAL
import advisory
import jll_advisory


def packed(files):
    out = io.BytesIO()
    with tarfile.open(fileobj=out, mode="w") as archive:
        for name, data in files:
            entry = tarfile.TarInfo(name)
            entry.size = len(data)
            archive.addfile(entry, io.BytesIO(data))
    out.seek(0)
    return tarfile.open(fileobj=out)


class OwnerEvidence(unittest.TestCase):
    def test_legacy_private_runtime_cannot_be_attributed_to_the_rebuilt_image(self):
        previous = json.loads(Path(__file__).with_name("previous_application.json").read_bytes())
        with self.assertRaisesRegex(ValueError, "private Julia runtime differs"):
            map_native({JULIA_INTERNAL: previous["native"][JULIA_INTERNAL]}, None, [])

    def test_verified_blob_rejects_wrong_size_and_bytes(self):
        raw = b"benign metadata"
        sha = hashlib.sha256(raw).hexdigest()
        descriptor = {"digest": "sha256:" + sha, "size": len(raw)}
        with packed([("blobs/sha256/" + sha, raw)]) as archive:
            self.assertEqual(blob(archive, descriptor), raw)
            with self.assertRaises(ValueError):
                blob(archive, dict(descriptor, size=1))
        with packed([("blobs/sha256/" + sha, b"x" * len(raw))]) as archive, self.assertRaises(ValueError):
            blob(archive, descriptor)

    def test_duplicate_publisher_member_is_rejected(self):
        with packed([("lib/fixture.so", b"one"), ("lib/fixture.so", b"two")]) as archive, self.assertRaises(ValueError):
            exact_file(archive, "lib/fixture.so")

    def test_control_requires_correct_owner_and_version_and_image(self):
        def manifest(rows, findings=()):
            return {"Trivy": {"Version": "0.72.0"}, "Results": [
                {"Type": "julia", "Target": "Manifest.toml", "Packages": rows, "Vulnerabilities": list(findings)}]}

        def row(name, uuid, version):
            return {"Name": name, "ID": uuid, "Version": version}

        def control(entry, cve):
            return {"PkgName": entry["Name"], "PkgID": entry["ID"], "InstalledVersion": entry["Version"], "VulnerabilityID": cve}

        actual = [row(n, v[0], v[1]) for n, v in BLAS.items()]
        old = dict(actual[0], Version="0.3.13+0")
        http = row("HTTP", advisory.HTTP_UUID, "1.10.15")
        ssl = row("OpenSSL_jll", jll_advisory.OPENSSL_UUID, "3.0.12+0")
        finding = control(old, "CVE-2021-4048")
        docs = {
            "retained": manifest([]), "blas": manifest(actual), "old-openblas": manifest([old], [finding]),
            "http": manifest([http], [control(http, "CVE-2025-52479")]),
            "openssl": manifest([ssl], [control(ssl, "CVE-2024-5535")]),
            "image": {"Trivy": {"Version": "0.72.0"}, "Metadata": {"ImageID": "cfg"},
                      "Results": [{"Class": "os-pkgs", "Type": "debian"}]},
            "syft": {"descriptor": {"version": "1.56.0"}, "source": {"type": "image",
                     "metadata": {"imageID": "cfg", "manifestDigest": "manifest"}}},
        }
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp)
            for name, data in docs.items():
                (out / (name + ".json")).write_text(json.dumps(data))
            self.assertEqual(scan_results(out, "manifest", "cfg", {})["jllFindings"], [])
            with self.assertRaises(ValueError):
                scan_results(out, "different manifest", "cfg", {})
            for field, replacement in [("PkgID", "unrelated UUID"), ("InstalledVersion", "0.3.30+0"),
                                       ("VulnerabilityID", "unrelated finding")]:
                with self.subTest(field=field), self.assertRaisesRegex(ValueError, "positive control"):
                    (out / "old-openblas.json").write_text(json.dumps(manifest([old], [dict(finding, **{field: replacement})])))
                    scan_results(out, "manifest", "cfg", {})
            (out / "old-openblas.json").write_text(json.dumps(docs["old-openblas"]))
            (out / "blas.json").write_text(json.dumps(manifest(actual, [control(actual[0], "fixture finding")])))
            with self.assertRaisesRegex(ValueError, "advisory present"):
                scan_results(out, "manifest", "cfg", {})

