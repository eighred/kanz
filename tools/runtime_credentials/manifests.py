"""Render the opt-in #1747 phase-1 transaction from existing component inputs.

The active overlays are not edited. Migration prerequisites and runtime cutover
are separate artifacts so an operator can wait for the authorized Jobs before
switching runtime policies/controllers. No credential value is an input here.
"""

import argparse
import copy
import hashlib
import json
from pathlib import Path
import re

import yaml


SERVICES = ("accounting", "audit", "identity")
PIN = re.compile(r"[^\s@]+@sha256:[0-9a-f]{64}\Z")


def identity(document):
    metadata = document.get("metadata", {})
    return document.get("kind"), metadata.get("namespace"), metadata.get("name")


def phase_one(documents, plane="base"):
    """Return prerequisites, cutover and a non-secret resource/digest receipt."""
    if plane not in ("base", "tokyo"):
        raise ValueError("unknown database plane")
    indexed = {}
    for document in documents:
        if not isinstance(document, dict):
            raise ValueError("manifest document must be an object")
        key = identity(document)
        if key in indexed:
            raise ValueError("duplicate manifest identity")
        indexed[key] = document
    prerequisites, cutover = [], []
    for service in SERVICES:
        namespace = "kanz-services"
        deployment = copy.deepcopy(indexed[("Deployment", namespace, service)])
        account = copy.deepcopy(indexed[("ServiceAccount", namespace, service)])
        provider = copy.deepcopy(indexed[("SecretProviderClass", namespace, service + "-db")])
        pod = deployment["spec"]["template"]["spec"]
        if pod.get("serviceAccountName") != service:
            raise ValueError("runtime identity changed outside the credential transaction")
        initializers = pod.get("initContainers", [])
        if len(initializers) != 1 or initializers[0].get("name") != "migrate":
            raise ValueError("expected exactly the existing migration initializer")
        migration = copy.deepcopy(initializers[0])
        if not PIN.fullmatch(migration.get("image", "")):
            raise ValueError("migration image must retain its approved immutable input")
        if migration.get("args") != ["--dir", "/migrations/" + service]:
            raise ValueError("migration owner/directory mismatch")
        if migration.get("env") != [{"name": "KANZ_MIGRATE_DATABASE_URL_FILE", "value": "/run/secrets/db/migrate-database-url"}]:
            raise ValueError("migration credential input mismatch")
        parameters = provider["spec"]["parameters"]
        original_objects = yaml.safe_load(parameters["objects"])
        expected = {("database-url", "dsn"), ("migrate-database-url", "migrate_dsn")}
        if parameters.get("roleName") != service or {(item["objectName"], item["secretKey"]) for item in original_objects} != expected:
            raise ValueError("canonical database projection changed")
        if len(original_objects) != 2 or any(item["secretPath"] != "kv/data/kanz/" + service for item in original_objects):
            raise ValueError("canonical database record changed")
        migration_provider = copy.deepcopy(provider)
        migration_provider["metadata"]["name"] = service + "-migration-db"
        migration_parameters = migration_provider["spec"]["parameters"]
        migration_parameters["roleName"] = service + "-migrate"
        migration_parameters["objects"] = yaml.safe_dump([{
            "objectName": "migrate-database-url",
            "secretPath": "kv/data/kanz/" + service + "-migration",
            "secretKey": "migrate_dsn",
        }], sort_keys=False)
        parameters["objects"] = yaml.safe_dump([{
            "objectName": "database-url",
            "secretPath": "kv/data/kanz/" + service + "-runtime",
            "secretKey": "dsn",
        }], sort_keys=False)
        del pod["initContainers"]
        migration_account = copy.deepcopy(account)
        migration_account["metadata"] = {"name": service + "-migrate", "namespace": namespace}
        migration_pod = {
            "serviceAccountName": service + "-migrate",
            "restartPolicy": "Never",
            "securityContext": copy.deepcopy(pod["securityContext"]),
            "containers": [migration],
            "volumes": [{"name": "db", "csi": {
                "driver": "secrets-store.csi.k8s.io", "readOnly": True,
                "volumeAttributes": {"secretProviderClass": service + "-migration-db"},
            }}],
        }
        if migration.get("volumeMounts") != [{"name": "db", "mountPath": "/run/secrets/db", "readOnly": True}]:
            raise ValueError("migration initializer acquired an unreviewed mount")
        migration.setdefault("resources", {"requests": {"cpu": "25m", "memory": "32Mi"}, "limits": {"cpu": "250m", "memory": "128Mi"}})
        job = {
            "apiVersion": "batch/v1", "kind": "Job",
            "metadata": {"name": service + "-credential-migration", "namespace": namespace},
            "spec": {"backoffLimit": 1, "activeDeadlineSeconds": 300, "template": {
                "metadata": {"labels": {"app": service + "-migrate", "app.kubernetes.io/part-of": "kanz"}},
                "spec": migration_pod,
            }},
        }
        provenance = deployment.get("metadata", {}).get("annotations", {}).get("kanz.io/release-commit")
        if provenance:
            job["metadata"]["annotations"] = {"kanz.io/release-commit": provenance}
            job["spec"]["template"]["metadata"]["annotations"] = {"kanz.io/release-commit": provenance}
        database_peer = {"podSelector": {"matchLabels": {"app": "postgres"}}}
        if plane == "tokyo":
            database_peer = {
                "namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "kanz-data"}},
                "podSelector": {"matchLabels": {"cnpg.io/cluster": "kanz-testnet-postgres"}},
            }
        network = {
            "apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
            "metadata": {"name": service + "-credential-migration", "namespace": namespace},
            "spec": {"podSelector": {"matchLabels": {"app": service + "-migrate"}}, "policyTypes": ["Ingress", "Egress"], "ingress": [], "egress": [
                {"to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "kube-system"}}, "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}}}], "ports": [{"protocol": "UDP", "port": 53}, {"protocol": "TCP", "port": 53}]},
                {"to": [database_peer], "ports": [{"protocol": "TCP", "port": 5432}]},
            ]},
        }
        prerequisites.extend((migration_account, migration_provider, network, job))
        cutover.extend((provider, account, deployment))
        for kind in ("Service", "PodDisruptionBudget"):
            if (kind, namespace, service) in indexed:
                cutover.append(copy.deepcopy(indexed[(kind, namespace, service)]))
    signing = copy.deepcopy(indexed[("SecretProviderClass", "kanz-services", "identity-signing-key")])
    signing_objects = yaml.safe_load(signing["spec"]["parameters"]["objects"])
    if signing["spec"]["parameters"].get("roleName") != "identity" or signing_objects != [{
        "objectName": "signing-key", "secretPath": "kv/data/kanz/identity", "secretKey": "signing_key_pem",
    }]:
        raise ValueError("identity signing projection changed outside the selected contract")
    signing_objects[0]["secretPath"] = "kv/data/kanz/identity-runtime"
    signing["spec"]["parameters"]["objects"] = yaml.safe_dump(signing_objects, sort_keys=False)
    cutover.append(signing)
    receipt = {
        "schema": 1, "phase": "preparation-only", "database_plane": plane, "services": list(SERVICES),
        "prerequisites": [list(identity(item)) for item in prerequisites],
        "cutover": [list(identity(item)) for item in cutover],
        "runtime_policies_require_separate_operator_authorization": True,
    }
    return prerequisites, cutover, receipt


def base_inputs(root):
    paths = [root / "kanz/infra/security/secrets/secretproviderclass.yaml"]
    paths.extend(root / ("kanz/infra/deploy/" + service + "-deploy.yaml") for service in SERVICES)
    return [document for path in paths for document in yaml.safe_load_all(path.read_text(encoding="utf-8")) if document]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", type=Path, help="previously rendered exact-source Tokyo manifest; omit for base component inputs")
    parser.add_argument("--output", type=Path, required=True, help="new external staging directory; contains no credential values")
    parser.add_argument("--source-sha", required=True, help="reviewed source commit, independently checked by the caller")
    parser.add_argument("--source-tree", required=True, help="reviewed source tree, independently checked by the caller")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[2]
    documents = list(yaml.safe_load_all(args.input.read_text(encoding="utf-8"))) if args.input else base_inputs(root)
    if not all(re.fullmatch(r"[0-9a-f]{40}", value) for value in (args.source_sha, args.source_tree)):
        parser.error("source commit/tree must be full lowercase Git object identities")
    prerequisites, cutover, receipt = phase_one(documents, "tokyo" if args.input else "base")
    receipt.update({"source_sha": args.source_sha, "source_tree": args.source_tree,
                    "input_sha256": hashlib.sha256(yaml.safe_dump_all(documents, sort_keys=True).encode()).hexdigest()})
    args.output.mkdir(parents=True, exist_ok=False)
    for name, data in (("migration-prerequisites.yaml", prerequisites), ("runtime-cutover.yaml", cutover)):
        content = yaml.safe_dump_all(data, sort_keys=False)
        (args.output / name).write_text(content, encoding="utf-8")
        receipt[name + "_sha256"] = hashlib.sha256(content.encode()).hexdigest()
    (args.output / "receipt.json").write_text(json.dumps(receipt, indent=2) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
