"""Exercise the rendered OIDC gateway and shared claims in the disposable cache rig."""
import base64
import hashlib
import hmac
import ipaddress
import json
import re
import secrets
import sys
import time

import yaml

from tools.valkey_migration import network

APP = "api-gateway"
NS = network.SERVICES
PATH = "/v1/permissions"
CLIENT = """import http.client,json,sys
p=json.load(sys.stdin)
h=http.client.HTTPConnection(p["ip"],8080,timeout=10)
h.request(p["method"],"/v1/permissions",headers=p["headers"])
r=h.getresponse()
print(r.status)
h.close()
"""


def signing_secret():
    doc, = [d for d in network.messaging.docs("kanz/infra/deploy/rig-dev-secrets.yaml")
            if d["metadata"]["name"] == "api-gateway-secrets"]
    data = doc.get("stringData") or {}
    if doc["metadata"]["namespace"] != NS or set(data) != {"signing-secret"} or not data["signing-secret"]:
        raise ValueError("unexpected gateway credential mount")
    data["signing-secret"] = secrets.token_hex(32)
    return doc


class Gateway:
    def __init__(self, k, image):
        self.k = k
        self.image = image
        self.key = secrets.token_hex(16)
        self.report = {}

    def start(self):
        def create(*objects):
            self.k("create", "-f", "-", data=yaml.safe_dump_all(objects))

        fixture = json.loads(network.messaging.feed.command(["node", "tools/valkey_migration/issuer.mjs"]))
        self.tokens = fixture["tokens"]
        config = {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "issuer-public", "namespace": NS},
                  "data": {"jwks.json": json.dumps(fixture["jwks"]), "revocations": json.dumps(fixture["revocations"])}}
        issuer = network.messaging.python_pod("fixture-issuer", NS, "identity")
        issuer["spec"]["volumes"] = [{"name": "public", "configMap": {"name": "issuer-public"}}]
        server, = issuer["spec"]["containers"]
        server["command"] = ["python", "-I", "-B", "-m", "http.server", "8087", "--directory", "/public"]
        server["volumeMounts"] = [{"name": "public", "mountPath": "/public", "readOnly": True}]
        identity, = [d for d in network.messaging.docs("kanz/infra/deploy/identity-deploy.yaml") if d["kind"] == "Service"]
        create(config, issuer, identity, network.messaging.python_pod("gateway-reader", NS))
        self.k("-n", NS, "wait", "--for=condition=Ready", "pod/gateway-reader", "--timeout=120s")
        doc = signing_secret()
        self.signing = doc["stringData"]["signing-secret"]
        create(doc)
        raw = network.messaging.feed.command([sys.executable, "tools/rig_dev_patch.py", "--keep-spiffe",
                                              "kanz/infra/deploy/api-gateway-deploy.yaml"])
        resources = list(yaml.safe_load_all(raw))
        deployment, = [d for d in resources if d["kind"] == "Deployment"]
        spec = deployment["spec"]["template"]["spec"]
        container, = spec["containers"]
        env = {e["name"]: e.get("value") for e in container["env"]}
        expected = {"API_GATEWAY_OIDC_ISSUER": "https://identity.kanz.internal", "API_GATEWAY_OIDC_AUDIENCE": "kanz",
                    "API_GATEWAY_OIDC_JWKS_URI": "http://identity.kanz-services.svc:8087/jwks.json",
                    "API_GATEWAY_REVOCATIONS_URI": "http://identity.kanz-services.svc:8087/revocations",
                    "API_GATEWAY_REDIS_URL_FILE": "/run/secrets/redis/redis-url",
                    "API_GATEWAY_SIGNING_SECRET_FILE": "/run/secrets/gateway/signing-secret"}
        forbidden = {"API_GATEWAY_JWT_SECRET", "API_GATEWAY_JWT_SECRET_FILE", "API_GATEWAY_ALLOW_DEV_HS256"}
        if (not expected.items() <= env.items() or forbidden.intersection(env) or not env.get("API_GATEWAY_SPIFFE_SOCKET")
                or spec["serviceAccountName"] != APP or deployment["spec"]["replicas"] != 2):
            raise ValueError("unexpected gateway identity or cache selection")
        container.update(image=self.image, imagePullPolicy="Never")
        create(*resources)
        self.k("-n", NS, "rollout", "status", "deployment/" + APP, "--timeout=210s")
        self.peers = self.processes()
        self.report["processes"] = self.peers

    def processes(self):
        pods = json.loads(self.k("-n", NS, "get", "pods", "-l", "app=" + APP, "-o", "json"))["items"]
        result = []
        for pod in pods:
            status, = pod["status"]["containerStatuses"]
            container, = pod["spec"]["containers"]
            if (not status["ready"] or container["image"] != self.image or container.get("command") or container.get("args")
                    or pod["spec"]["serviceAccountName"] != APP or not re.search(r"sha256:[a-f0-9]{64}$", status["imageID"])):
                raise ValueError("unverified gateway process")
            logs = self.k("-n", NS, "logs", pod["metadata"]["name"])
            if not any(row.get("msg") == "bus transport" and row.get("mtls") is True
                       for row in (json.loads(line) for line in logs.splitlines() if line.startswith("{"))):
                raise ValueError("gateway did not confirm mTLS")
            result.append({"uid": pod["metadata"]["uid"], "containerID": status["containerID"],
                           "imageID": status["imageID"], "ip": str(ipaddress.ip_address(pod["status"]["podIP"])),
                           "restarts": status["restartCount"]})
        if len(result) != 2 or len({p["containerID"] for p in result}) != 2:
            raise ValueError("expected two independent gateway replicas")
        return sorted(result, key=lambda peer: peer["uid"])

    def request(self, pod, tenant, method, expected, key=None):
        signature = hmac.digest(self.signing.encode(), (method + "\n" + PATH + "\n").encode(), "sha256")
        headers = {"X-Signature": base64.urlsafe_b64encode(signature).decode().rstrip("=")}
        if tenant:
            headers["Authorization"] = "Bearer " + self.tokens[tenant]
        if key:
            headers["Idempotency-Key"] = key
        data = json.dumps({"ip": self.peers[pod]["ip"], "method": method, "headers": headers})
        status = self.k("-n", NS, "exec", "-i", "pod/gateway-reader", "--", "python", "-I", "-B", "-c", CLIENT, data=data)
        if status != str(expected):
            raise ValueError(f"gateway {method} replica {pod} expected {expected}, observed {status}")

    def deadlines(self):
        # Key names contain NUL separators. Query only deadlines in the isolated
        # cache without transferring key bytes or credentials to the report.
        rows = self.k("-n", network.MESSAGING, "exec", "redis-0", "--", "sh", "-ec",
                      'export REDISCLI_AUTH="$(cat /run/secrets/redis/password)"; exec redis-cli -e --raw EVAL "$1" 0',
                      "_", "local t={} for _,k in ipairs(redis.call('KEYS','kanz:gw:idem:*')) do "
                      "table.insert(t,redis.call('PEXPIRETIME',k)) end return t")
        return sorted(int(value) for value in rows.splitlines())

    def initial(self):
        self.request(0, None, "GET", 401)
        self.request(1, "revoked", "GET", 401)
        self.request(0, "a", "GET", 200)
        if self.deadlines():
            raise ValueError("gateway claim store was not initially empty")
        # Only GET is registered. A signed POST returns a definite method
        # refusal and acquires the real shared claim without reaching a writer.
        self.request(0, "a", "POST", 405, self.key)
        self.request(1, "a", "POST", 409, self.key)
        self.request(1, "b", "POST", 405, self.key)
        self.request(0, "b", "POST", 409, self.key)
        self.original = self.deadlines()
        if len(self.original) != 2 or min(self.original) <= int(time.time() * 1000):
            raise ValueError("expected two live tenant-scoped claims")
        self.report["initialTenantClaims"] = len(self.original)

    def outage(self):
        self.request(0, "a", "POST", 405, self.key + "-outage")
        self.report["freshDuringOutage"] = True

    def recovered(self):
        if self.processes() != self.peers or self.deadlines() != self.original:
            raise ValueError("gateway restart or retained claim deadlines changed")
        self.request(1, "a", "POST", 409, self.key)
        self.request(0, "b", "POST", 409, self.key)
        remaining = max(self.original) / 1000 - time.time()
        if not 0 < remaining <= 65:
            raise ValueError("gateway claim lifetime outside the declared window")
        time.sleep(remaining + 0.2)
        if self.deadlines():
            raise ValueError("expired gateway claims remained")
        self.request(1, "a", "POST", 405, self.key)
        self.request(0, "b", "POST", 405, self.key)
        if len(self.deadlines()) != 2:
            raise ValueError("both expired tenant claims were not reacquired")
        self.report["retainedThenExpired"] = True
