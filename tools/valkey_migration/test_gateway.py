import unittest

import yaml

from tools.valkey_migration import gateway, network


class GatewayInputsTest(unittest.TestCase):
    def test_generic_rig_preserves_oidc_and_the_request_signing_mount(self):
        raw = network.messaging.feed.command(["bash", "tools/rig-apply.sh", "--render-workload", "api-gateway-deploy.yaml"])
        deployment, = [d for d in yaml.safe_load_all(raw) if d["kind"] == "Deployment"]
        spec = deployment["spec"]["template"]["spec"]
        container, = spec["containers"]
        env = {entry["name"]: entry.get("value") for entry in container["env"]}
        self.assertEqual(env["API_GATEWAY_OIDC_ISSUER"], "https://identity.kanz.internal")
        self.assertFalse({"API_GATEWAY_JWT_SECRET", "API_GATEWAY_JWT_SECRET_FILE", "API_GATEWAY_ALLOW_DEV_HS256"} & env.keys())
        self.assertFalse({"API_GATEWAY_OPERATOR_ADDR", "API_GATEWAY_OPERATOR_ROLE", "API_GATEWAY_SPIFFE_SOCKET"} & env.keys())
        doc = gateway.signing_secret()
        self.assertEqual(set(doc["stringData"]), {"signing-secret"})
        volume, = [v for v in spec["volumes"] if v.get("secret", {}).get("secretName") == doc["metadata"]["name"]]
        mount, = [v for v in container["volumeMounts"] if v["name"] == volume["name"]]
        self.assertEqual(env["API_GATEWAY_SIGNING_SECRET_FILE"], mount["mountPath"] + "/signing-secret")
