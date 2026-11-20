"""Collect offline Trivy and Syft evidence for the native-only safe bundle profile."""
import argparse
import datetime as dt
import json
import os
from pathlib import Path
import subprocess

from blas_owners import BLAS, advisory, jll_advisory, prior_receipt, retained_packages


def collect(image, tools, cache, registry, previous, output):
    output.mkdir()
    for name, archive_hash in advisory.ARCHIVES.items():
        advisory.tool_identity(tools / name, archive_hash)
    db = cache / "db" / "trivy.db"
    metadata = json.loads((cache / "db" / "metadata.json").read_bytes())
    advisory.validate_database(metadata, dt.datetime.now(dt.timezone.utc))
    before = advisory.digest(db)
    actual = {name: {"uuid": row[0], "version": row[1]} for name, row in BLAS.items()}
    control = {"OpenBLAS_jll": dict(actual["OpenBLAS_jll"], version="0.3.13+0")}
    http = {"HTTP": {"uuid": advisory.HTTP_UUID, "version": "1.10.15"}}
    retained = retained_packages(prior_receipt(previous))
    raw_registry = registry.read_bytes()
    jll_advisory.registry_identity(raw_registry, actual | retained)
    jll_advisory.registry_identity(raw_registry, jll_advisory.CONTROL)

    environment = {name: os.environ[name] for name in ("PATH", "HOME", "TMPDIR") if name in os.environ}
    environment["SYFT_CHECK_FOR_APP_UPDATE"] = "false"

    def run(args, cwd=output):
        subprocess.run(list(map(str, args)), cwd=cwd, check=True, timeout=540,
                       stdin=subprocess.DEVNULL, env=environment)

    ignored, config = output / "empty.ignore", output / "config.yaml"
    ignored.write_bytes(b"")
    config.write_bytes(b"{}\n")
    flags = ["--cache-dir", cache, "--cache-backend", "memory", "--config", config, "--timeout", "8m",
             "--quiet", "--skip-db-update", "--skip-java-db-update", "--offline-scan", "--scanners", "vuln",
             "--ignore-unfixed=false", "--ignorefile", ignored, "--list-all-pkgs", "--format", "json"]
    trivy = tools / "trivy"
    for label, packages in [("blas", actual), ("retained", retained), ("old-openblas", control),
                            ("openssl", jll_advisory.CONTROL), ("http", http)]:
        directory = output / label
        directory.mkdir()
        (directory / "Manifest.toml").write_bytes(jll_advisory.manifest(packages))
        (directory / "Project.toml").write_bytes(jll_advisory.project(packages))
        run([trivy, "fs", *flags, "--output", output / (label + ".json"), "."], directory)
    run([trivy, "image", *flags, "--input", image, "--output", output / "image.json"])
    run([trivy, "convert", "--format", "spdx-json", "--output", output / "spdx.json", output / "image.json"])
    run([tools / "syft", "scan", "oci-archive:" + str(image), "-o", "syft-json=" + str(output / "syft.json")])
    if before != advisory.digest(db):
        raise ValueError("advisory database changed during scan")
    (output / "scan-inputs.json").write_text(json.dumps({
        "databaseSHA256": before, "metadata": metadata, "imageArchiveSHA256": advisory.digest(image),
    }, indent=2) + "\n", encoding="utf-8")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("image", "tools", "cache", "registry", "previous", "output"):
        parser.add_argument(name, type=Path)
    args = parser.parse_args()
    collect(*(getattr(args, name).resolve() for name in ("image", "tools", "cache", "registry", "previous", "output")))

