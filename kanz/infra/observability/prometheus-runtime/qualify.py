"""Compare the pinned upstream and candidate on an isolated Docker loopback.

The supervisor never mounts Docker credentials or a host socket into either
image. Only disposable fixture files, an owned data volume and bounded tmpfs
are writable by target processes. The receipt contains no TLS private material.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import ssl
import subprocess
import tarfile
import tempfile
import time
import urllib.parse
import uuid


HERE = Path(__file__).resolve().parent
SHELLS = {"busybox", "ash", "awk", "sh"}


def command(*args, timeout=60, check=True):
    result = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            timeout=timeout, check=False)
    if check and result.returncode:
        raise RuntimeError(f"{args[0]} {args[1]} failed: {result.stderr.decode()[-2000:]}")
    return result


def digest(path):
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def image_inventory(image, scratch, suffix):
    name = "prometheus-inventory-" + suffix
    command("docker", "create", "--name", name, "--network", "none", image)
    try:
        rootfs = scratch / (suffix + ".tar")
        command("docker", "export", "--output", str(rootfs), name)
        with tarfile.open(rootfs) as archive:
            forbidden = sorted(member.name for member in archive
                               if Path(member.name).name in SHELLS)
        ca = scratch / (suffix + "-ca.crt")
        command("docker", "cp", name + ":/etc/ssl/certs/ca-certificates.crt", str(ca))
        ca_count = ssl.create_default_context(cafile=str(ca)).cert_store_stats()["x509_ca"]
        if ca_count == 0:
            raise ValueError("image has no usable CA certificates")
        binaries = {}
        for binary in ("prometheus", "promtool"):
            path = scratch / (suffix + "-" + binary)
            command("docker", "cp", name + ":/bin/" + binary, str(path))
            binaries[binary] = digest(path)
        identity = json.loads(command("docker", "image", "inspect", image).stdout)[0]
        return {"image_id": identity["Id"], "os": identity["Os"],
                "architecture": identity["Architecture"],
                "user": identity["Config"]["User"], "shell_paths": forbidden,
                "ca_count": ca_count, "binaries_sha256": binaries}
    finally:
        command("docker", "rm", "-f", "-v", name, check=False)


def probe(api, path):
    return command("docker", "exec", api, "python", "/fixture/kube_fixture.py", "get", path,
                   timeout=6).stdout.decode()


def query(api, expression, at=None):
    params = {"query": expression}
    if at is not None:
        params["time"] = str(at)
    result = json.loads(probe(api, "/api/v1/query?" + urllib.parse.urlencode(params)))
    if result.get("status") != "success":
        raise ValueError("query did not succeed")
    return result["data"]["result"]


def wait_for(check):
    deadline = time.monotonic() + 60
    while time.monotonic() < deadline:
        try:
            value = check()
            if value:
                return value
        except (RuntimeError, ValueError, KeyError, subprocess.TimeoutExpired):
            pass
        time.sleep(1)
    raise TimeoutError("isolated Prometheus probe did not converge")


def runtime(image, helper, scratch, suffix):
    name, api, volume, holder = ("prometheus-" + suffix + tail
                                 for tail in ("", "-api", "-data", "-holder"))
    fixture = scratch / suffix
    fixture.mkdir()
    (fixture / "kube_fixture.py").write_bytes((HERE / "kube_fixture.py").read_bytes())
    command("openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
            "-subj", "/CN=qualification", "-addext", "subjectAltName=IP:127.0.0.1",
            "-keyout", str(fixture / "server.key"), "-out", str(fixture / "server.crt"))
    # These are ephemeral dummy fixture keys, never retained in the receipt.
    os.chmod(fixture / "server.key", 0o600)
    (fixture / "prometheus.yml").write_text("""global:
  scrape_interval: 1s
scrape_configs:
  - job_name: kubernetes-pods
    kubernetes_sd_configs:
      - role: pod
        api_server: https://127.0.0.1:8443
        tls_config:
          ca_file: /fixture/server.crt
""", encoding="utf-8")
    sandbox = ["--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
               "--pids-limit=64", "--memory=512m", "--cpus=1",
               "--ulimit=fsize=268435456:268435456",
               "--log-opt=max-size=1m", "--log-opt=max-file=1",
               "--tmpfs=/tmp:rw,noexec,nosuid,size=16m", "--env=HOME=/tmp",
               "--mount", f"type=bind,source={fixture},target=/fixture,readonly"]

    def start_api():
        command("docker", "run", "-d", "--name", api, "--network=container:" + name,
                *sandbox, f"--user={os.getuid()}:{os.getgid()}",
                helper, "python", "/fixture/kube_fixture.py", "serve")

    try:
        command("docker", "volume", "create", "--driver=local", "--opt=type=tmpfs",
                "--opt=device=tmpfs", "--opt=o=size=256m,uid=65534,gid=65534,mode=0700", volume)
        # Keep the bounded filesystem mounted across the Prometheus restart.
        # This proves process/WAL recovery, not node loss or a production PVC.
        data_mount = f"type=volume,source={volume},target=/prometheus,volume-nocopy"
        command("docker", "run", "-d", "--name", holder, "--network=none", *sandbox,
                "--user=65534:65534", "--mount", data_mount,
                helper, "python", "-c", "import time; time.sleep(480)")
        command("docker", "run", "-d", "--name", name, "--network=none", *sandbox,
                "--user=65534:65534", "--mount", data_mount, image,
                "--config.file=/fixture/prometheus.yml", "--storage.tsdb.path=/prometheus",
                "--web.listen-address=127.0.0.1:9090")
        start_api()
        wait_for(lambda: json.loads(command("docker", "exec", api, "python",
                                            "/fixture/kube_fixture.py", "status").stdout))
        wait_for(lambda: "Ready" in probe(api, "/-/ready"))
        value = query(api, "vector(1)")
        if len(value) != 1 or value[0]["value"][1] != "1":
            raise ValueError("constant query differs from upstream contract")
        def discovered():
            values = query(api, 'up{job="kubernetes-pods"}')
            return values if len(values) == 1 and values[0]["value"][1] == "1" else None

        try:
            observed = wait_for(discovered)
        except TimeoutError as error:
            # These are only the owned dummy fixture's logs, with no real
            # account credentials or externally supplied configuration.
            logs = command("docker", "logs", "--tail=15", name, check=False)
            raise TimeoutError("TLS discovery failed: " + logs.stderr.decode()[-4000:]) from error
        if len(observed) != 1 or observed[0]["value"][1] != "1":
            raise ValueError("TLS pod discovery did not scrape its target")
        at = observed[0]["value"][0]
        discovery_status = json.loads(command("docker", "exec", api, "python",
                                              "/fixture/kube_fixture.py", "status").stdout)
        command("docker", "rm", "-f", api)
        command("docker", "stop", "--time=15", name)
        command("docker", "start", name)
        start_api()
        wait_for(lambda: "Ready" in probe(api, "/-/ready"))
        retained = query(api, 'up{job="kubernetes-pods"}', at)
        if len(retained) != 1 or retained[0]["value"][1] != "1":
            raise ValueError("pre-restart observation was lost from the owned volume")
        return {"readiness": True, "constant_query": True,
                "tls_pod_discovery": True, "watch_protocol": discovery_status,
                "retained_history_after_restart": True}
    finally:
        survivors = []
        for container in (api, name, holder):
            try:
                command("docker", "rm", "-f", "-v", container, check=False)
                if command("docker", "inspect", container, check=False).returncode == 0:
                    survivors.append(container)
            except subprocess.TimeoutExpired:
                survivors.append(container)
        try:
            command("docker", "volume", "rm", volume, check=False)
            if command("docker", "volume", "inspect", volume, check=False).returncode == 0:
                survivors.append(volume)
        except subprocess.TimeoutExpired:
            survivors.append(volume)
        if survivors:
            raise RuntimeError("owned qualification resources survived cleanup: " + ", ".join(survivors))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--candidate", required=True)
    parser.add_argument("--helper", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if os.getuid() == 0:
        raise ValueError("qualification requires a non-root Linux supervisor")
    recipe = (HERE / "Dockerfile").read_text()
    upstream = re.search(r"^FROM (\S+) AS upstream$", recipe, re.MULTILINE)[1]
    if "@sha256:" not in upstream:
        raise ValueError("upstream must be pinned")
    suffix = uuid.uuid4().hex[:12]
    with tempfile.TemporaryDirectory(prefix="prometheus-qualification-") as directory:
        scratch = Path(directory)
        baseline = image_inventory(upstream, scratch, suffix + "-upstream")
        candidate = image_inventory(args.candidate, scratch, suffix + "-candidate")
        if not any(Path(path).name == "busybox" for path in baseline["shell_paths"]):
            raise ValueError("affected upstream control did not expose BusyBox")
        if candidate["shell_paths"]:
            raise ValueError("candidate retains shell tooling")
        if candidate["user"] != "65534:65534":
            raise ValueError("candidate default identity changed")
        if candidate["binaries_sha256"] != baseline["binaries_sha256"]:
            raise ValueError("candidate binaries differ from the pinned upstream")
        baseline["runtime"] = runtime(upstream, args.helper, scratch, suffix + "-upstream")
        candidate["runtime"] = runtime(args.candidate, args.helper, scratch, suffix + "-candidate")
        receipt = {"source": command("git", "rev-parse", "HEAD").stdout.decode().strip(),
                   "tree": command("git", "rev-parse", "HEAD^{tree}").stdout.decode().strip(),
                   "recipe_sha256": digest(HERE / "Dockerfile"), "upstream": upstream,
                   "runtime_base": re.findall(r"^FROM (\S+)$", recipe, re.MULTILINE)[-1],
                   "baseline": baseline, "candidate": candidate,
                   "helper_image_id": json.loads(command("docker", "image", "inspect", args.helper).stdout)[0]["Id"]}
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(receipt, sort_keys=True) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
