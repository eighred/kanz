"""Identify reusable compilation inputs; unknown host metadata disables caching only.

Go revalidates source, compiler and option hashes, but not changed C libraries.
Bind prefix reuse to the exact hosted image, native packages and Go environment;
refuse to save if native inputs changed while the complete gate was running.
"""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys


GO_ENV = "GOVERSION GOOS GOARCH GOAMD64 GOARM GOARM64 CGO_ENABLED CC CXX CGO_CFLAGS CGO_CPPFLAGS CGO_CXXFLAGS CGO_LDFLAGS GOEXPERIMENT GOMODCACHE GOCACHE".split()


def identify(env, go, packages):
    if env.get("RUNNER_OS") != "Linux":
        raise ValueError("unsupported runner")
    image = "-".join(env.get(key, "") for key in ("ImageOS", "ImageVersion"))
    if any(not re.fullmatch(r"[A-Za-z0-9_.-]{1,80}", env.get(key, "")) for key in ("ImageOS", "ImageVersion")):
        raise ValueError("missing hosted image identity")
    version = go["GOVERSION"]
    if not re.fullmatch(r"go[0-9]+\.[0-9]+\.[0-9]+", version):
        raise ValueError("unknown exact Go version")
    if go["GOOS"] != "linux" or not packages.strip() or any(key not in go for key in GO_ENV):
        raise ValueError("incomplete compiler environment")
    paths = [go[key] for key in ("GOMODCACHE", "GOCACHE")]
    workspace = Path(env["GITHUB_WORKSPACE"]).resolve()
    approved = [Path.home() / "go/pkg/mod", Path.home() / ".cache/go-build"]
    for value, expected in zip(paths, approved):
        path = Path(value)
        if not path.is_absolute() or any(c in value for c in "\r\n") or path.resolve() != expected or path.resolve().is_relative_to(workspace):
            raise ValueError("cache path is not an isolated Go cache")
    # Keep package order immaterial, while retaining every native package/version.
    native = "\n".join(sorted(packages.splitlines()))
    fingerprint = hashlib.sha256(json.dumps(go, sort_keys=True).encode() + b"\0" + native.encode()).hexdigest()
    return {"version": version, "image": image, "fingerprint": fingerprint, "paths": "\n".join(paths)}


def main():
    output = Path(os.environ["GITHUB_OUTPUT"])
    try:
        go = json.loads(subprocess.check_output(["go", "env", "-json", *GO_ENV], timeout=30))
        packages = subprocess.check_output(["dpkg-query", "-W", "-f", "${binary:Package}=${Version}\n"], text=True, timeout=30)
        data = identify(os.environ, go, packages)
    except (KeyError, ValueError, OSError, subprocess.SubprocessError):
        print("::notice::Compiler cache disabled: host identity could not be established")
        with output.open("a", encoding="utf-8") as stream:
            stream.write("enabled=false\nstable=false\n")
        return
    with output.open("a", encoding="utf-8") as stream:
        if sys.argv[1:] == ["check"]:
            stable = data['fingerprint'] == os.environ.get('EXPECTED_FINGERPRINT')
            stream.write(f"stable={str(stable).lower()}\n")
            print("Compiler cache native inputs unchanged" if stable else "::notice::Native compiler inputs changed; cache save disabled")
        elif not sys.argv[1:]:
            stream.write("enabled=true\n")
            for key in ("version", "image", "fingerprint"):
                stream.write(f"{key}={data[key]}\n")
            stream.write(f"paths<<KANZ_GO_CACHE_PATHS\n{data['paths']}\nKANZ_GO_CACHE_PATHS\n")
        else:
            raise ValueError("unknown cache operation")


if __name__ == "__main__":
    main()
