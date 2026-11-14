"""Offline build proof: exactly one fixed Debian package, identical Python graph."""
import hashlib
import importlib.metadata
import json
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent


def inventory():
    result = subprocess.run(["dpkg-query", "-W", "-f=${Package}:${Architecture}\t${Version}\n"], check=True, stdout=subprocess.PIPE, timeout=30)
    if len(result.stdout) > 1 << 20:
        raise ValueError("package inventory exceeded bound")
    packages = dict(line.split("\t", 1) for line in result.stdout.decode().splitlines())
    python = {item.metadata["Name"]: item.version for item in importlib.metadata.distributions()}
    return {"packages": packages, "python": python, "pythonVersion": sys.version.split()[0]}


def compare(before, after, lock):
    name = lock["package"] + ":" + lock["architecture"]
    if before["packages"].get(name) != lock["before"] or after["packages"].get(name) != lock["after"]:
        raise ValueError("fixed package versions differ")
    expected = dict(before["packages"], **{name: lock["after"]})
    if after["packages"] != expected or before["python"] != after["python"] or before["pythonVersion"] != after["pythonVersion"] or after["python"].get("acryl-datahub") != "1.8.0":
        raise ValueError("single-package/Python compatibility proof refused")
    digest = lambda value: hashlib.sha256(json.dumps(value, sort_keys=True).encode()).hexdigest()
    return {"baseReference": lock["base"], "packageSHA256": lock["sha256"], "package": name,
            "before": lock["before"], "after": lock["after"], "onlyFixedPackageChanged": True,
            "pythonUnchanged": True, "pythonVersion": after["pythonVersion"], "datahubVersion": after["python"]["acryl-datahub"],
            "beforePackagesSHA256": digest(before["packages"]), "afterPackagesSHA256": digest(after["packages"]), "pythonPackagesSHA256": digest(after["python"])}


if __name__ == "__main__":
    lock = json.loads((HERE / "package-lock.json").read_text())
    if sys.argv[1:] == ["before"]:
        body = (HERE / "linux-libc-dev.deb").read_bytes()
        if len(body) != lock["bytes"] or hashlib.sha256(body).hexdigest() != lock["sha256"]:
            raise ValueError("fixed package bytes differ")
        (HERE / "before.json").write_text(json.dumps(inventory()))
    elif sys.argv[1:] == ["after"]:
        before = json.loads((HERE / "before.json").read_text())
        proof = compare(before, inventory(), lock)
        Path("/opt/kanz-catalog-patch.json").write_text(json.dumps(proof, sort_keys=True))
    else:
        raise ValueError("unknown patch proof phase")
