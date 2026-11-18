"""Run the actual webhook service against a disposable SPIRE/NATS/Valkey estate."""
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import sys
import tempfile
import uuid

import yaml

from tools.valkey_migration import network, rig_install

messaging = network.messaging
feed = messaging.feed
ROOT = network.ROOT
APP = "webhook-ingest"
APP_NS = network.SERVICES
CACHE_NS = network.MESSAGING


def webhook_config():
    # Preserve the deployed input structure and authorization bindings, replacing
    # every sender credential before the document leaves this process.
    doc, = [d for d in messaging.docs("kanz/infra/deploy/rig-dev-secrets.yaml")
            if d["metadata"]["name"] == "webhook-ingest-config"]
    if doc["metadata"]["namespace"] != APP_NS or set(doc["stringData"]) != {"config.json"}:
        raise ValueError("unexpected webhook config mount")
    cfg = json.loads(doc["stringData"]["config.json"])
    if not cfg["strategies"]:
        raise ValueError("no configured webhook sender")
    for entry in cfg["strategies"].values():
        if set(entry) != {"secret", "funds"} or not entry["funds"]:
            raise ValueError("unexpected webhook sender binding")
        entry["secret"] = secrets.token_hex(32)
    doc["stringData"]["config.json"] = json.dumps(cfg)
    return doc


def main():
    name = "cni-proof-" + uuid.uuid4().hex[:12]
    source = feed.command(["git", "rev-parse", "HEAD"])
    feed.command(["git", "diff", "--quiet", "HEAD", "--"])
    runner_temp = Path(os.environ["RUNNER_TEMP"])
    build = runner_temp / "valkey-webhook-build"
    binary = build / APP
    if set(build.iterdir()) != {binary} or not binary.is_file():
        raise ValueError("expected a single compiled application executable")
    image = "kanz-webhook-fixture:" + name
    report = {"source": source, "executableSHA256": hashlib.sha256(binary.read_bytes()).hexdigest(),
              "policySHA256": hashlib.sha256(network.POLICY.read_bytes()).hexdigest(),
              "passed": False, "cleanupPassed": False}
    artifact = runner_temp / "valkey-webhook.json"
    if name in feed.command(["kind", "get", "clusters"]).splitlines():
        raise ValueError("fixture already exists")
    with tempfile.TemporaryDirectory(prefix="kanz-webhook-") as temp:
        scratch = Path(temp)
        cfg = str(scratch / "kubeconfig")

        def k(*args, data=None):
            return feed.command(["kubectl", "--kubeconfig", cfg, "--context", "kind-" + name, *args], data=data)

        def apply(resources):
            k("apply", "-f", "-", data=yaml.safe_dump_all(resources))

        def cache(*args, data=None):
            return k("-n", CACHE_NS, *args, data=data)

        def pods():
            return json.loads(k("-n", APP_NS, "get", "pods", "-l", "app=" + APP, "-o", "json"))["items"]

        def authenticated_clients():
            # CLIENT LIST contains no passwords. Only the bound pod identities
            # reach the report; raw server rows and workload logs stay private.
            rows = cache("exec", "redis-0", "--", "sh", "-ec",
                         'export REDISCLI_AUTH="$(cat /run/secrets/redis/password)"; exec redis-cli -e --raw CLIENT LIST TYPE NORMAL')
            peers = set()
            for line in rows.splitlines():
                fields = dict(v.split("=", 1) for v in line.split() if "=" in v)
                if fields.get("user") == "default":
                    peers.add(fields["addr"].rsplit(":", 1)[0])
            items = pods()
            if len(items) != 2:
                raise ValueError("expected both webhook replicas")
            result = []
            for pod in items:
                c, = pod["spec"]["containers"]
                status, = pod["status"]["containerStatuses"]
                ip = str(ipaddress.ip_address(pod["status"]["podIP"]))
                if (pod["spec"]["serviceAccountName"] != APP or pod["metadata"]["labels"]["app"] != APP
                        or c["image"] != image or c.get("command") or c.get("args") or not status["ready"]
                        or ip not in peers or not re.search(r"sha256:[a-f0-9]{64}$", status["imageID"])):
                    raise ValueError("actual webhook process or authenticated cache peer not verified")
                logs = k("-n", APP_NS, "logs", pod["metadata"]["name"])
                if not any(row.get("msg") == "bus transport" and row.get("mtls") is True
                           for row in (json.loads(line) for line in logs.splitlines() if line.startswith("{"))):
                    raise ValueError("webhook did not confirm mTLS")
                result.append({"podUID": pod["metadata"]["uid"], "containerID": status["containerID"],
                               "imageID": status["imageID"], "restarts": status["restartCount"]})
            if len({r["containerID"] for r in result}) != 2:
                raise ValueError("duplicate application process")
            return sorted(result, key=lambda r: r["podUID"])

        try:
            report["stage"] = "package compiled webhook"
            feed.command(["docker", "build", "-t", image, "-f", "-", str(build)],
                         data='FROM scratch\nCOPY webhook-ingest /webhook-ingest\nUSER 65532:65532\nENTRYPOINT ["/webhook-ingest"]\n')
            report["fixtureImageID"] = feed.command(["docker", "image", "inspect", "--format", "{{.Id}}", image])
            report["stage"] = "enforcing cluster and workload identities"
            feed.create_enforcing_cluster(name, scratch)
            feed.command(["kind", "load", "docker-image", image, "--name", name])
            crds = [d for path in sorted((ROOT / "kanz/infra/security/spire/crds").glob("*.yaml"))
                    for d in messaging.docs(str(path.relative_to(ROOT)))]
            apply(crds)
            for crd in crds:
                k("wait", "--for=condition=Established", "crd/" + crd["metadata"]["name"], "--timeout=90s")
            apply(feed.spire_objects())
            k("-n", "spire-system", "rollout", "status", "statefulset/spire-server", "--timeout=240s")
            k("-n", "spire-system", "rollout", "status", "daemonset/spire-agent", "--timeout=240s")
            apply(messaging.docs("kanz/infra/security/spire/registration.yaml"))
            report["stage"] = "authenticated broker and canonical streams"
            apply(messaging.broker_objects())
            cache("rollout", "status", "statefulset/nats", "--timeout=240s")
            apply(messaging.bootstrap_objects())
            cache("wait", "--for=condition=complete", "job/nats-bootstrap", "--timeout=220s")
            report["stage"] = "guarded fresh Valkey and policy"
            apply([doc for doc in network.fixture_secrets() if doc["metadata"]["name"] in ("redis-auth", APP + "-redis")])
            apply([webhook_config(), yaml.safe_load(network.POLICY.read_text(encoding="utf-8"))])
            policy = json.loads(cache("get", "networkpolicy", "-o", "json"))["items"]
            if [p["spec"] for p in policy] != [yaml.safe_load(network.POLICY.read_text(encoding="utf-8"))["spec"]]:
                raise ValueError("unexpected effective cache policy")
            rig_install.install(cache, network.rendered("kanz/infra/messaging/redis.yaml"))
            cache("rollout", "status", "statefulset/redis", "--timeout=180s")
            report["cacheImageID"] = cache("get", "pod/redis-0", "-o", "jsonpath={.status.containerStatuses[0].imageID}")
            claim_uid = cache("get", "pvc/data-redis-0", "-o", "jsonpath={.metadata.uid}")
            if not report["cacheImageID"].endswith(network.migration.VALKEY.split("@")[1]) or not claim_uid:
                raise ValueError("unexpected cache runtime")
            report["stage"] = "real webhook readiness and authenticated client connections"
            raw = feed.command([sys.executable, "tools/rig_dev_patch.py", "--keep-spiffe",
                                "kanz/infra/deploy/webhook-ingest-deploy.yaml"])
            resources = list(yaml.safe_load_all(raw))
            deployment, = [doc for doc in resources if doc["kind"] == "Deployment"]
            container, = deployment["spec"]["template"]["spec"]["containers"]
            env = {e["name"]: e.get("value") for e in container["env"]}
            if (deployment["spec"]["replicas"] != 2 or not env["SPIFFE_ENDPOINT_SOCKET"]
                    or env["WEBHOOK_INGEST_REDIS_URL_FILE"] != "/run/secrets/redis/redis-url"):
                raise ValueError("unexpected webhook composition")
            container["image"] = image
            container["imagePullPolicy"] = "Never"
            apply(resources)
            k("-n", APP_NS, "rollout", "status", "deployment/" + APP, "--timeout=210s")
            report["initialClients"] = authenticated_clients()
            report["stage"] = "real cache outage"
            cache("scale", "statefulset/redis", "--replicas=0")
            cache("wait", "--for=delete", "pod/redis-0", "--timeout=120s")
            k("-n", APP_NS, "wait", "--for=condition=Ready=False", "pod", "-l", "app=" + APP, "--timeout=100s")
            report["unreadyDuringOutage"] = sorted(p["metadata"]["uid"] for p in pods())
            if report["unreadyDuringOutage"] != [r["podUID"] for r in report["initialClients"]]:
                raise ValueError("webhook identity changed during cache loss")
            report["stage"] = "same-claim restart and original webhook recovery"
            cache("scale", "statefulset/redis", "--replicas=1")
            cache("rollout", "status", "statefulset/redis", "--timeout=180s")
            k("-n", APP_NS, "rollout", "status", "deployment/" + APP, "--timeout=120s")
            report["recoveredClients"] = authenticated_clients()
            if (report["initialClients"] != report["recoveredClients"]
                    or cache("get", "pvc/data-redis-0", "-o", "jsonpath={.metadata.uid}") != claim_uid):
                raise ValueError("recovery changed webhook processes or original claim")
            report["passed"] = True
        finally:
            feed.command(["kind", "delete", "cluster", "--name", name, "--kubeconfig", cfg], timeout=180)
            if report.get("fixtureImageID"):
                feed.command(["docker", "image", "rm", image])
            report["cleanupPassed"] = name not in feed.command(["kind", "get", "clusters"]).splitlines()
            report["passed"] &= report["cleanupPassed"]
            artifact.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    if not report["passed"]:
        raise ValueError("webhook cache qualification incomplete")


if __name__ == "__main__":
    main()
