import unittest

from tools.valkey_migration import gateway, network


class GatewayInputsTest(unittest.TestCase):
    def test_generic_rig_preserves_oidc_and_the_request_signing_mount(self):
        deployment, = [d for d in network.rendered("kanz/infra/deploy/api-gateway-deploy.yaml")
                       if d["kind"] == "Deployment"]
        spec = deployment["spec"]["template"]["spec"]
        container, = spec["containers"]
        env = {entry["name"]: entry.get("value") for entry in container["env"]}
        self.assertEqual(env["API_GATEWAY_OIDC_ISSUER"], "https://identity.kanz.internal")
        self.assertFalse({"API_GATEWAY_JWT_SECRET", "API_GATEWAY_JWT_SECRET_FILE", "API_GATEWAY_ALLOW_DEV_HS256"} & env.keys())
        doc = gateway.signing_secret()
        self.assertEqual(set(doc["stringData"]), {"signing-secret"})
        volume, = [v for v in spec["volumes"] if v.get("secret", {}).get("secretName") == doc["metadata"]["name"]]
        mount, = [v for v in container["volumeMounts"] if v["name"] == volume["name"]]
        self.assertEqual(env["API_GATEWAY_SIGNING_SECRET_FILE"], mount["mountPath"] + "/signing-secret")
