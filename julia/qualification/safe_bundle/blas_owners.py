"""Bind the safe JuliaC application's native owners to the saved OCI and publisher bytes."""
import argparse
from collections import Counter
import hashlib
import io
import json
from pathlib import Path, PurePosixPath
import sys
import tarfile

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import advisory
import jll_advisory
from inventory import GCC_LIBRARY, GCC_SHA256, LAUNCHER, REMOVED

BASE_MANIFEST = "8e80c746dfbb1aa5891456d3414e5f7fafed1ecc62a6e1579f8ba4311b016d83"
PRIOR_RECEIPT = "2fff5c1128761fc78aeb275d20fb91cff669c28b9eb749d2be24ff8fea071a44"
JULIA_INTERNAL = "lib/julia/libjulia-internal.so.1.13.3"
JULIA_PUBLIC = "lib/libjulia.so.1.13.3"
BLAS = {
    "OpenBLAS_jll": ("4536629a-c528-5b80-bd46-f80d51c5b363", "0.3.30+0",
                     "libopenblas64_.0.3.30.so", "66bea625eceac84062d46f64777149a3d594a9436e91e12f80920e5cafd14aa6"),
    "libblastrampoline_jll": ("8e850b90-86db-534c-a0d3-1478176c7d93", "5.15.0+0",
                             "libblastrampoline.so.5", "76ddd4223122d9664f827fc940f7696bb4225cfd1cf6bb00b3acbeb524bcc609"),
}


def exact_file(stream, key):
    matches = [m for m in stream.getmembers() if m.name == key]
    if len(matches) != 1 or not matches[0].isfile() or matches[0].size > 128 << 20:
        raise ValueError("missing, duplicate or oversized archive member: " + key)
    return stream.extractfile(matches[0]).read()


def blob(oci, descriptor):
    algorithm, digest = descriptor["digest"].split(":", 1)
    data = exact_file(oci, "blobs/sha256/" + digest)
    if algorithm != "sha256" or hashlib.sha256(data).hexdigest() != digest or len(data) != descriptor["size"]:
        raise ValueError("OCI descriptor mismatch")
    return data


def application(archive, base):
    if advisory.digest(base) != BASE_MANIFEST:
        raise ValueError("unexpected distroless linux/amd64 base")
    with tarfile.open(archive) as oci:
        top, = json.loads(exact_file(oci, "index.json"))["manifests"]
        descriptors = json.loads(blob(oci, top))["manifests"]
        manifest, = [d for d in descriptors if d.get("platform") == {"os": "linux", "architecture": "amd64"}]
        image = json.loads(blob(oci, manifest))
        config = json.loads(blob(oci, image["config"]))
        base_layers = json.loads(base.read_bytes())["layers"]
        layers = image["layers"]
        if ([d["digest"] for d in layers[:len(base_layers)]] != [d["digest"] for d in base_layers]
                or config["config"]["Entrypoint"] != ["/opt/kanz/bin/curve-worker"]
                or config["config"]["User"] != "65532:65532"):
            raise ValueError("unexpected base layers or entrypoint")
        for descriptor in layers[:len(base_layers)]:
            blob(oci, descriptor)
        app, *tail = layers[len(base_layers):]
        for descriptor in tail:
            with tarfile.open(fileobj=io.BytesIO(blob(oci, descriptor))) as layer:
                if layer.next() is not None:
                    raise ValueError("unexpected content after application layer")
        native = {}
        seen = set()
        with tarfile.open(fileobj=io.BytesIO(blob(oci, app))) as layer:
            for member in layer:
                path = PurePosixPath(member.name)
                if member.isdir() and path.parts in ((), ("opt",), ("opt", "kanz")):
                    continue
                if path.parts[:2] != ("opt", "kanz") or ".." in path.parts or path.name.startswith(".wh."):
                    raise ValueError("unexpected path in application layer")
                key = path.relative_to("opt/kanz").as_posix()
                if key in seen:
                    raise ValueError("duplicate application layer member")
                seen.add(key)
                if member.isfile():
                    with layer.extractfile(member) as raw:
                        if raw.read(4) == b"\x7fELF":
                            digest = hashlib.sha256(b"\x7fELF")
                            while chunk := raw.read(1024 * 1024):
                                digest.update(chunk)
                            native[key] = digest.hexdigest()
        return manifest["digest"], image["config"]["digest"], native


def prior_receipt(path):
    if advisory.digest(path) != PRIOR_RECEIPT:
        raise ValueError("wrong preceding qualified publisher receipt")
    return json.loads(path.read_bytes())


def retained_packages(previous):
    return {
        name: {k: row[k] for k in ("uuid", "version")}
        for name, row in previous["wrapperMetadata"].items()
        if any(path not in REMOVED for path in row["files"])
    }


def map_native(native, previous, publishers):
    prior = prior_receipt(previous)
    mapped = {}
    for owner, row in prior["wrapperMetadata"].items():
        for path in row["files"]:
            if path in REMOVED:
                continue
            if path == GCC_LIBRARY:
                expected = GCC_SHA256
            else:
                expected = row["publisherFiles"]["lib/" + PurePosixPath(path).name]
            if native.get(path) != expected:
                raise ValueError("changed native publisher bytes: " + path)
            mapped[path] = {"owner": owner, "version": row["version"], "sha256": expected,
                            "evidence": "rebuilt-gcc" if path == GCC_LIBRARY else "prior-publisher"}
    for (owner, (_, version, filename, archive_hash)), package in zip(BLAS.items(), publishers, strict=True):
        if advisory.digest(package) != archive_hash:
            raise ValueError("wrong publisher archive: " + owner)
        with tarfile.open(package) as archive:
            expected = hashlib.sha256(exact_file(archive, "lib/" + filename)).hexdigest()
        path = "lib/julia/" + filename
        if native.get(path) != expected:
            raise ValueError("BLAS publisher and shipped ELF differ: " + path)
        mapped[path] = {"owner": owner, "version": version, "sha256": expected, "evidence": "publisher"}
    if native.get(JULIA_PUBLIC) != prior["native"][JULIA_PUBLIC]["sha256"]:
        raise ValueError("unexpected Julia public runtime")
    for path, owner in [(JULIA_PUBLIC, "Julia release"), (JULIA_INTERNAL, "Julia internal runtime"), (LAUNCHER, "Kanz JuliaC application")]:
        mapped[path] = {"owner": owner, "sha256": native[path], "evidence": "separate runtime qualification"}
    if mapped.keys() != native.keys():
        raise ValueError("unmapped application ELF")
    return mapped


def scan_results(directory, manifest, config, retained):
    def read(name):
        return json.loads((directory / (name + ".json")).read_bytes())
    advertised = {n: {"uuid": r[0], "version": r[1]} for n, r in BLAS.items()}
    def julia(name, expected):
        result = advisory.manifest_result(read(name), "Manifest.toml")
        advisory.validate_packages(result, expected)
        return result
    real = julia("blas", advertised)
    retained_result = julia("retained", retained)
    old = {"OpenBLAS_jll": dict(advertised["OpenBLAS_jll"], version="0.3.13+0")}
    control = julia("old-openblas", old)
    if not any(f.get("PkgName") == "OpenBLAS_jll" and f.get("PkgID") == old["OpenBLAS_jll"]["uuid"]
               and f.get("InstalledVersion") == "0.3.13+0" and f.get("VulnerabilityID") == "CVE-2021-4048"
               for f in control.get("Vulnerabilities", [])):
        raise ValueError("version-specific OpenBLAS positive control missing")
    advisory.validate_control(read("http"))
    jll_advisory.validate_control(julia("openssl", jll_advisory.CONTROL))
    actual, sbom = read("image"), read("syft")
    if (actual.get("Trivy", {}).get("Version") != "0.72.0" or actual["Metadata"]["ImageID"] != config
            or sbom["descriptor"]["version"] != "1.56.0" or sbom["source"]["type"] != "image"
            or sbom["source"]["metadata"]["imageID"] != config or sbom["source"]["metadata"]["manifestDigest"] != manifest):
        raise ValueError("scanner identity differs from OCI")
    results = actual["Results"]
    if (not any(r.get("Class") == "os-pkgs" and r.get("Type") == "debian" for r in results)
            or any(r.get("ExperimentalModifiedFindings") for r in results)):
        raise ValueError("missing OS scan or modified findings")
    severity = Counter(f["Severity"] for r in results for f in r.get("Vulnerabilities", []))
    if severity["CRITICAL"] or real.get("Vulnerabilities") or retained_result.get("Vulnerabilities"):
        raise ValueError("critical image finding or BLAS JLL advisory present")
    return {"imageFindings": dict(severity), "jllFindings": [], "reviewedWrappers": sorted(retained | advertised), "openblasControl": "CVE-2021-4048"}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("image", "base_manifest", "previous_receipt", "openblas_archive", "trampoline_archive", "scans"):
        parser.add_argument(name, type=Path)
    a = parser.parse_args()
    manifest, config, native = application(a.image, a.base_manifest)
    owners = map_native(native, a.previous_receipt, [a.openblas_archive, a.trampoline_archive])
    report = scan_results(a.scans, manifest, config, retained_packages(prior_receipt(a.previous_receipt)))
    scan_inputs = json.loads((a.scans / "scan-inputs.json").read_bytes())
    archive_hash = advisory.digest(a.image)
    if scan_inputs["imageArchiveSHA256"] != archive_hash:
        raise ValueError("scanned archive differs from native owner input")
    report.update({"imageArchiveSHA256": archive_hash, "manifest": manifest, "config": config,
                   "databaseSHA256": scan_inputs["databaseSHA256"], "databaseUpdatedAt": scan_inputs["metadata"]["UpdatedAt"],
                   "owners": owners, "releaseQualified": False})
    print(json.dumps(report, sort_keys=True, indent=2))

