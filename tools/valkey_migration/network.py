"""Qualify the fresh-rig cache ingress in a new disposable kind/Cilium cluster."""
import copy
import hashlib
import json
import os
from pathlib import Path
import secrets
import re
import sys
import time
import tempfile
from urllib.parse import urlsplit
import uuid

import yaml

from tools.tokyo_messaging_proof import proof as messaging
from tools.valkey_migration import proof as migration
from tools.valkey_migration import rig_install

ROOT = Path(__file__).resolve().parents[2]
POLICY = ROOT / "kanz/infra/messaging/valkey-clients.yaml"
CLIENTS = ("api-gateway", "webhook-ingest")
SERVICES = "kanz-services"
MESSAGING = "kanz-messaging"
FOREIGN = "fixture-foreign"

# Open exactly the fixture cache using the mounted DSN and the standard
# consumer environment variable. No credential, server reply or DSN is logged.
AUTH = """import os,pathlib,socket,sys
from urllib.parse import urlsplit
url=urlsplit(pathlib.Path(os.environ[sys.argv[1]]).read_text().strip())
assert (url.scheme,url.hostname,url.port)==('redis','redis.kanz-messaging.svc',6379)
assert url.username in (None,'') and url.password
def frame(*parts):
 return b'*'+str(len(parts)).encode()+b'\\r\\n'+b''.join(b'$'+str(len(p)).encode()+b'\\r\\n'+p+b'\\r\\n' for p in parts)
with socket.create_connection((url.hostname,url.port),timeout=3) as sock:
 f=sock.makefile('rb')
 def ask(*parts):
  sock.sendall(frame(*parts))
  return f.readline()
 assert ask(b'PING').startswith(b'-NOAUTH ')
 assert ask(b'AUTH',url.password.encode())==b'+OK\\r\\n'
 assert ask(b'PING')==b'+PONG\\r\\n'
print('AUTH_PASS')
"""


def rendered(path):
    return list(yaml.safe_load_all(migration.command([sys.executable, str(ROOT / "tools/rig_dev_patch.py"), str(ROOT / path)])))


def client_pod(name):
    deployment, = [d for d in rendered(f"kanz/infra/deploy/{name}-deploy.yaml") if d["kind"] == "Deployment"]
    template = deployment["spec"]["template"]
    service = template["spec"]
    container, = [c for c in service["containers"] if c["name"] == name]
    env, = [item for item in container["env"] if item["name"] == name.upper().replace("-", "_") + "_REDIS_URL_FILE"]
    mount, = [m for m in container["volumeMounts"] if m["mountPath"] + "/redis-url" == env["value"]]
    volume, = [v for v in service["volumes"] if v["name"] == mount["name"]]
    if (deployment["metadata"]["namespace"] != SERVICES or service["serviceAccountName"] != name or
            template["metadata"]["labels"]["app"] != name or volume["secret"]["secretName"] != name + "-redis" or
            not mount["readOnly"]):
        raise ValueError("unexpected cache consumer scope")
    pod = messaging.python_pod(name, SERVICES)
    pod["metadata"]["labels"] = copy.deepcopy(template["metadata"]["labels"])
    pod["spec"].update(serviceAccountName=service["serviceAccountName"], volumes=[volume])
    pod["spec"]["containers"][0].update(env=[env], volumeMounts=[mount])
    return pod, env["name"]


def fixture_secrets():
    records = list(yaml.safe_load_all((ROOT / "kanz/infra/deploy/rig-dev-secrets.yaml").read_text(encoding="utf-8")))
    wanted = {"redis-auth", *(name + "-redis" for name in CLIENTS)}
    selected = {v["metadata"]["name"]: v for v in records if v and v["kind"] == "Secret" and v["metadata"]["name"] in wanted}
    backend = selected["redis-auth"]
    if backend["metadata"]["namespace"] != MESSAGING:
        raise ValueError("unexpected server secret scope")
    previous = backend["stringData"]["password"]
    if not previous:
        raise ValueError("empty server password")
    password = secrets.token_hex(24)
    backend["stringData"]["password"] = password
    for name in CLIENTS:
        secret = selected[name + "-redis"]
        dsn = urlsplit(secret["stringData"]["redis-url"])
        if (secret["metadata"]["namespace"] != SERVICES or dsn.scheme != "redis" or not dsn.password or
                dsn.password != previous or dsn.username not in (None, "") or
                dsn.hostname != "redis.kanz-messaging.svc" or dsn.port != 6379):
            raise ValueError("unexpected mounted cache DSN")
        secret["stringData"]["redis-url"] = dsn._replace(netloc=f":{password}@{dsn.hostname}:{dsn.port}").geturl()
    return list(selected.values())


def main():
    name = "cni-proof-" + uuid.uuid4().hex[:12]
    command = messaging.feed.command
    source = command(["git", "rev-parse", "HEAD"])
    command(["git", "diff", "--quiet", "HEAD", "--"])
    report = {"source": source, "image": migration.VALKEY, "cases": [], "passed": False,
              "policySHA256": hashlib.sha256(POLICY.read_bytes()).hexdigest(), "cleanupPassed": False}
    artifact = Path(os.environ["RUNNER_TEMP"]) / "rig-cache-ingress.json"
    with tempfile.TemporaryDirectory(prefix="kanz-rig-ingress-") as temp:
        scratch = Path(temp)
        cfg = str(scratch / "kubeconfig")

        def k(*args, data=None):
            return command(["kubectl", "--kubeconfig", cfg, "--context", "kind-" + name, *args], data=data)

        def create(resources):
            k("create", "-f", "-", data=yaml.safe_dump_all(resources))

        def auth(app, phase):
            ns = SERVICES
            pod = json.loads(k("-n", ns, "get", "pod", app, "-o", "json"))
            if pod["spec"]["serviceAccountName"] != app or pod["metadata"]["labels"]["app"] != app:
                raise ValueError("unexpected running consumer identity")
            marker = k("-n", ns, "exec", app, "--", "python", "-I", "-B", "-c", AUTH, env_names[app])
            if marker != "AUTH_PASS":
                raise ValueError("missing authenticated consumer response")
            report["cases"].append(f"{phase}:{app}:authenticated")

        def tcp(ns, pod, expected):
            marker = k("-n", ns, "exec", pod, "--", "python", "-I", "-B", "-c", messaging.TCP, address, "6379", expected)
            if marker != "TCP_PASS":
                raise ValueError("missing bounded connection result")
            report["cases"].append(f"{ns}/{pod}:{expected}")

        if name in command(["kind", "get", "clusters"]).splitlines():
            raise ValueError("fixture cluster already exists")
        try:
            report["stage"] = "create enforcing cluster"
            messaging.feed.create_enforcing_cluster(name, scratch)
            report["stage"] = "create first-install workload"
            create([{"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": ns}} for ns in (SERVICES, MESSAGING, FOREIGN)])
            create(fixture_secrets())
            cache = rendered("kanz/infra/messaging/redis.yaml")
            sts, = [d for d in cache if d["kind"] == "StatefulSet"]
            if sts["spec"]["template"]["spec"]["containers"][0]["image"] != migration.VALKEY:
                raise ValueError("unexpected rig image")
            def cache_api(*args, data=None):
                return k("-n", MESSAGING, *args, data=data)

            rig_install.install(cache_api, cache, preflight=True)
            rig_install.install(cache_api, cache)
            k("-n", MESSAGING, "rollout", "status", "statefulset/redis", "--timeout=210s")
            server = json.loads(k("-n", MESSAGING, "get", "pod/redis-0", "-o", "json"))
            runtime, = server["status"]["containerStatuses"]
            if (server["spec"]["containers"][0]["image"] != migration.VALKEY or not runtime["ready"] or
                    not re.search(r"sha256:[0-9a-f]{64}$", runtime["imageID"])):
                raise ValueError("unverified running cache image")
            report["runtimeImageID"] = runtime["imageID"]
            original_uid = server["metadata"]["uid"]
            rig_install.install(cache_api, rendered("kanz/infra/messaging/redis.yaml"))
            cache_api("rollout", "status", "statefulset/redis", "--timeout=120s")
            current_uid = cache_api("get", "pod/redis-0", "-o", "jsonpath={.metadata.uid}")
            if current_uid != original_uid:
                raise ValueError("reapply replaced cache pod")
            report["originalClaimReapply"] = True
            env_names = {}
            for app in CLIENTS:
                pod, env_names[app] = client_pod(app)
                create([{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": {"name": app, "namespace": SERVICES}}, pod])
            create([messaging.python_pod("unapproved", SERVICES), messaging.python_pod("other-namespace", FOREIGN, CLIENTS[0])])
            for ns, pod in [(SERVICES, app) for app in CLIENTS] + [(SERVICES, "unapproved"), (FOREIGN, "other-namespace")]:
                k("-n", ns, "wait", "--for=condition=Ready", "pod/" + pod, "--timeout=120s")
            address = k("-n", MESSAGING, "get", "svc/redis", "-o", "jsonpath={.spec.clusterIP}")
            report["stage"] = "baseline"
            for app in CLIENTS:
                auth(app, "before")
            for ns, pod in ((SERVICES, "unapproved"), (FOREIGN, "other-namespace")):
                tcp(ns, pod, "allowed")
            report["stage"] = "enforce cache ingress"
            policy = yaml.safe_load(POLICY.read_text())
            create([policy])
            actual = json.loads(k("-n", MESSAGING, "get", "networkpolicies", "-o", "json"))["items"]
            if len(actual) != 1 or actual[0]["metadata"]["name"] != policy["metadata"]["name"] or actual[0]["spec"] != policy["spec"]:
                raise ValueError("effective cache policy differs")
            for ns, pod in ((SERVICES, "unapproved"), (FOREIGN, "other-namespace")):
                # Cilium installs a new Kubernetes policy asynchronously. A
                # successful check still requires three actual TCP timeouts.
                deadline = time.monotonic() + 45
                while True:
                    try:
                        tcp(ns, pod, "blocked")
                        break
                    except RuntimeError:
                        if time.monotonic() >= deadline:
                            raise
                        time.sleep(1)
            for app in CLIENTS:
                auth(app, "after")
            report["passed"] = True
        finally:
            command(["kind", "delete", "cluster", "--name", name, "--kubeconfig", cfg], timeout=180)
            report["cleanupPassed"] = name not in command(["kind", "get", "clusters"]).splitlines()
            report["passed"] &= report["cleanupPassed"]
            artifact.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    if not report["passed"] or len(report["cases"]) != 8:
        raise ValueError("incomplete cache ingress qualification")


if __name__ == "__main__":
    main()
