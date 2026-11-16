"""Render an opt-in system topic PostSync hook for an estate-specific GitOps source."""
import argparse
import hashlib
from pathlib import Path
import re

import yaml

ROOT = Path(__file__).resolve().parents[2]
NAMESPACE = "kanz-messaging"
JOB_NAME = "kafka-system-bootstrap"
MANIFEST = ROOT / "kanz/infra/kafka/topics-job.yaml"


def render(raw, image, broker, peer, cluster, profile):
    if not re.fullmatch(r"ghcr\.io/eighred/kanz-kafka-provision@sha256:[0-9a-f]{64}", image):
        raise ValueError("pinned native provisioner image required")
    if not re.fullmatch(r"[a-z0-9][a-z0-9.-]*:[1-9][0-9]{0,4}", broker) or int(broker.rsplit(":", 1)[1]) > 65535:
        raise ValueError("explicit broker TLS endpoint required")
    if not re.fullmatch(r"(?:redpanda\.)?[A-Za-z0-9_-]+", cluster) or len(cluster) > 128 or cluster in ("redpanda.unknown", "redpanda.initializing"):
        raise ValueError("exact broker cluster identity required")
    peers = {
        "kafka-min-isr": "spiffe://kanz.internal/ns/kanz-messaging/sa/kafka",
        "redpanda-raft": "spiffe://kanz.internal/ns/kanz-messaging/sa/redpanda",
    }
    if peers.get(profile) != peer:
        raise ValueError("broker identity and durability profile differ")
    digest = hashlib.sha256(raw).hexdigest()
    config_name = "kafka-system-topics-" + digest
    config = {
        "apiVersion": "v1", "kind": "ConfigMap",
        "metadata": {"name": config_name, "namespace": NAMESPACE},
        "immutable": True, "data": {"topics-job.yaml": raw.decode("utf-8")},
    }
    account = {"apiVersion": "v1", "kind": "ServiceAccount",
               "metadata": {"name": "kafka-provisioner", "namespace": NAMESPACE}}
    job = {
        "apiVersion": "batch/v1", "kind": "Job",
        "metadata": {
            "generateName": JOB_NAME + "-", "namespace": NAMESPACE,
            "annotations": {"argocd.argoproj.io/hook": "PostSync",
                            "argocd.argoproj.io/hook-delete-policy": "BeforeHookCreation"},
        },
        "spec": {
            "backoffLimit": 0, "activeDeadlineSeconds": 240, "ttlSecondsAfterFinished": 900,
            "template": {"spec": {
                "serviceAccountName": "kafka-provisioner", "restartPolicy": "Never",
                "automountServiceAccountToken": False,
                "securityContext": {"runAsNonRoot": True, "runAsUser": 65532,
                                    "seccompProfile": {"type": "RuntimeDefault"}},
                "containers": [{
                    "name": "provisioner", "image": image,
                    "resources": {"requests": {"cpu": "50m", "memory": "64Mi"},
                                  "limits": {"cpu": "1", "memory": "256Mi"}},
                    "securityContext": {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True,
                                        "capabilities": {"drop": ["ALL"]}},
                    "args": ["-system-bootstrap", "-mode=apply", "-manifest=/manifest/topics-job.yaml",
                             "-manifest-sha256=" + digest, "-broker=" + broker,
                             "-broker-spiffe-id=" + peer, "-expected-cluster-id=" + cluster,
                             "-durability-profile=" + profile,
                             "-workload-socket=unix:///run/spiffe/spire-agent.sock", "-timeout=210s"],
                    "volumeMounts": [{"name": "table", "mountPath": "/manifest/topics-job.yaml",
                                      "subPath": "topics-job.yaml", "readOnly": True},
                                     {"name": "spiffe", "mountPath": "/run/spiffe", "readOnly": True}],
                }],
                "volumes": [{"name": "table", "configMap": {"name": config_name}},
                            {"name": "spiffe", "csi": {"driver": "csi.spiffe.io", "readOnly": True}}],
            }},
        },
    }
    return account, config, job


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("image", "broker", "peer", "cluster", "profile"):
        parser.add_argument("--" + name, required=True)
    args = parser.parse_args()
    print(yaml.safe_dump_all(render(MANIFEST.read_bytes(), **vars(args))), end="")


if __name__ == "__main__":
    main()
