"""Qualify the tracked JVM hook and native replacement on owned Apache Kafka."""
import hashlib
import json
import os
from pathlib import Path
import tempfile
import time
import uuid

import yaml

import argo_bootstrap
import qualify
import system_bootstrap
import topology

KAFKA = "apache/kafka:3.9.0@sha256:fbc7d7c428e3755cf36518d4976596002477e4c052d1f80b5b9eafd06d0fff2f"
PEER = "spiffe://kanz.internal/ns/kanz-messaging/sa/kafka"
NS = system_bootstrap.NAMESPACE
OWNER = "kanz-fixture-topic-owner"
PHASES = ("legacy-bootstrap", "native-activate", "legacy-fallback")


def broker_objects():
    docs = topology.retained.shared.manifests("kanz/infra/kafka/kafka.yaml")
    stateful = next(d for d in docs if d["kind"] == "StatefulSet")
    spec = stateful["spec"]["template"]["spec"]
    broker = spec["containers"][0]
    broker["image"] = KAFKA
    topology.retained.shared.pin_images(docs)
    helper = next(d for d in docs if d["kind"] == "ConfigMap")
    helper["data"]["helper.conf"] = "\n".join(
        line for line in helper["data"]["helper.conf"].splitlines()
        if not line.strip().startswith(("cmd ", "cmd_args "))
    )
    # The pinned SPIFFE helper is distroless. Assemble the Kafka PEM as the
    # same unprivileged UID that receives the SVID, in the memory volume.
    spec["securityContext"].update(runAsUser=1000, runAsGroup=1000)
    broker["args"][0] = broker["args"][0].replace(
        "exec /etc/kafka/docker/run",
        "umask 077\ncat /etc/kafka-certs/svid.pem /etc/kafka-certs/svid_key.pem > /tmp/keystore.pem\n"
        "exec /etc/kafka/docker/run",
    )
    next(e for e in broker["env"] if e["name"] == "KAFKA_SSL_KEYSTORE_LOCATION")["value"] = "/tmp/keystore.pem"
    spec["volumes"].append({"name": "keystore", "emptyDir": {"medium": "Memory"}})
    broker["volumeMounts"].append({"name": "keystore", "mountPath": "/tmp"})
    # The upstream image lacks the estate's custom principal builder. Isolate
    # the fixture with listener-specific network policy and retain SPIFFE mTLS.
    excluded = {"KAFKA_PRINCIPAL_BUILDER_CLASS", "KAFKA_AUTHORIZER_CLASS_NAME",
                "KAFKA_SUPER_USERS", "KAFKA_ALLOW_EVERYONE_IF_NO_ACL_FOUND"}
    broker["env"] = [e for e in broker["env"] if e["name"] not in excluded]
    broker["env"].append({"name": "KAFKA_HEAP_OPTS", "value": "-Xms384m -Xmx768m"})
    cluster = next(e["value"] for e in broker["env"] if e["name"] == "CLUSTER_ID")
    stateful["spec"].pop("volumeClaimTemplates")
    spec["volumes"].append({"name": "data", "emptyDir": {"sizeLimit": "2Gi"}})
    return docs, cluster


def listener_policy(owner):
    rules = [{"from": [{"podSelector": {"matchLabels": {"app": "kafka"}}}],
              "ports": [{"port": 9092, "protocol": "TCP"}, {"port": 9093, "protocol": "TCP"}]}]
    if owner:
        rules.append({"from": [{"podSelector": {"matchLabels": {OWNER: owner}}}],
                      "ports": [{"port": 9092 if owner == "legacy" else 9094, "protocol": "TCP"}]})
    return {"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
            "metadata": {"name": "handoff-listeners", "namespace": NS},
            "spec": {"podSelector": {"matchLabels": {"app": "kafka"}},
                     "policyTypes": ["Ingress"], "ingress": rules}}


def variants(raw, admin_image, built, cluster):
    legacy = list(yaml.safe_load_all(raw))
    config, job = legacy
    config["immutable"] = True
    job["spec"].update(backoffLimit=0, activeDeadlineSeconds=660)
    template = job["spec"]["template"]
    template["metadata"] = {"labels": {OWNER: "legacy"}}
    template["spec"].update(restartPolicy="Never", serviceAccountName="default", automountServiceAccountToken=False)
    container = template["spec"]["containers"][0]
    container["image"] = KAFKA
    container["env"].append({"name": "KAFKA_HEAP_OPTS", "value": "-Xmx192m"})
    account, config, native = system_bootstrap.render(
        raw, "ghcr.io/eighred/kanz-kafka-provision@" + built, "kafka.kanz-messaging.svc:9094",
        PEER, cluster, "kafka-min-isr")
    template = native["spec"]["template"]
    template["metadata"] = {"labels": {OWNER: "native"}}
    template["spec"]["containers"][0].update(image=admin_image, imagePullPolicy="IfNotPresent")
    return {"legacy-bootstrap": legacy, "native-activate": [account, config, native], "legacy-fallback": legacy}


def writer_jobs(k):
    return json.loads(k("-n", NS, "get", "jobs", "-o", "json"))["items"]


def require_empty(k):
    jobs = writer_jobs(k)
    pods = json.loads(k("-n", NS, "get", "pods", "-l", OWNER, "-o", "json"))["items"]
    if jobs or pods:
        raise ValueError("writer handoff not quiescent")


def terminal_job(k, desired, owner, prior_uids):
    name = desired["metadata"]["name"]
    jobs = writer_jobs(k)
    if len(jobs) != 1 or jobs[0]["metadata"]["name"] != name:
        raise ValueError("expected exactly one topic Job")
    job = jobs[0]
    uid = qualify.job_custody(job, desired)
    pods = json.loads(k("-n", NS, "get", "pods", "-l", OWNER, "-o", "json"))["items"]
    pod = qualify.pod_custody(pods, desired, uid)
    status = qualify.pod_diagnostic(pods, desired, uid)
    if (uid in prior_uids or pod["metadata"]["labels"].get(OWNER) != owner
            or pod["status"]["phase"] != "Succeeded" or status["exitCode"] != 0
            or not any(c["type"] == "Complete" and c["status"] == "True" for c in job["status"]["conditions"])):
        raise ValueError("topic Job did not complete as sole writer")
    return job, pod


def drain(k, apply, job):
    apply([listener_policy(None)])
    k("-n", NS, "delete", "job", job["metadata"]["name"], "--cascade=foreground", "--wait=true", "--timeout=60s")
    require_empty(k)


def broker_uids(k):
    pods = json.loads(k("-n", NS, "get", "pods", "-l", "app=kafka", "-o", "json"))["items"]
    if ({p["metadata"]["name"] for p in pods} != {"kafka-0", "kafka-1", "kafka-2"}
            or any(len(p["status"].get("containerStatuses", [])) != 2
                   or any(c["restartCount"] or not c["ready"] for c in p["status"]["containerStatuses"]) for p in pods)):
        raise ValueError("broker continuity unavailable")
    return {p["metadata"]["name"]: p["metadata"]["uid"] for p in pods}


def history(k, records, append=False):
    binary = "/opt/kafka/bin/kafka-console-"
    broker = ["--bootstrap-server", "kafka-0.kafka-headless.kanz-messaging.svc:9092", "--topic", "order.order"]
    if append:
        k("-n", NS, "exec", "-i", "kafka-0", "-c", "kafka", "--",
          binary+"producer.sh", *broker, "--producer-property", "acks=all", "--property", "parse.key=true",
          # Kafka's default keyed partitioner maps UTF-8 w to partition 0 of 6.
          data="w\t"+records[-1]+"\n", timeout=40)
    output = k("-n", NS, "exec", "kafka-0", "-c", "kafka", "--",
               binary+"consumer.sh", *broker, "--partition", "0", "--offset", "earliest",
               "--max-messages", str(len(records)), "--timeout-ms", "15000",
               "--property", "print.offset=true", timeout=40)
    expected = [f"Offset:{offset}\t{value}" for offset, value in enumerate(records)]
    if output.splitlines() != expected:
        raise ValueError("retained topic records or offsets differ")
    return hashlib.sha256(output.encode()).hexdigest()


def native_receipt(k, desired, pod, built, name, source, cluster, raw, mode):
    graph = qualify.observe_image(pod, desired, built, name, time.monotonic()+60, {})
    if not graph["verified"]:
        raise ValueError("native image custody differs")
    result = json.loads(k("-n", NS, "logs", pod["metadata"]["name"], "-c", "provisioner", "--limit-bytes=8192"))
    digest, count, _ = qualify.canonical_inventory(raw, True)
    expected = {"mode": mode, "expected_cluster_id": cluster, "manifest_sha256": digest,
                "durability_profile": "kafka-min-isr", "verified_topics": count, "missing_topics": 0,
                "verified_acls": 0, "topic_attempts": 0, "acl_attempts": 0,
                "partial_or_unknown": False, "complete": True}
    if result != {"version": source, "receipt": expected}:
        raise ValueError("native readback differs")
    return expected


def main():
    command = qualify.command
    source, tree = command(["git", "rev-parse", "HEAD"]), command(["git", "rev-parse", "HEAD^{tree}"])
    run, attempt = os.environ["GITHUB_RUN_ID"], os.environ["GITHUB_RUN_ATTEMPT"]
    qualify.source_identity(source, tree, run, attempt, os.environ["GITHUB_SHA"])
    runner = Path(os.environ["RUNNER_TEMP"])
    claim = runner / "native-kafka-claim.json"
    artifact = runner / "legacy-kafka-handoff.json"
    name = "redpanda-proof-" + uuid.uuid4().hex[:12]
    raw = system_bootstrap.MANIFEST.read_bytes()
    evidence = {"source": source, "tree": tree, "run": run, "attempt": attempt,
                "kafkaImage": KAFKA, "stages": [], "passed": False, "cleanup": False}
    with tempfile.TemporaryDirectory(prefix="legacy-kafka-handoff-") as directory:
        scratch = Path(directory)
        kubeconfig = str(scratch / "kubeconfig")
        def k(*args, data=None, timeout=300):
            return command(["kubectl", "--kubeconfig", kubeconfig, "--context", "kind-"+name, *args], data, timeout)

        def apply(docs):
            k("apply", "-f", "-", data=yaml.safe_dump_all(docs))

        owned = False
        try:
            evidence["stage"] = "build"
            image = "kanz-native-admin:"+source
            command(["docker", "build", "--build-arg", "VERSION="+source, "-f",
                     "kanz/cmd/kanz-kafka-provision/Dockerfile", "-t", image, "."], timeout=1200)
            built = command(["docker", "image", "inspect", "--format", "{{.Id}}", image])
            if name in command(["kind", "get", "clusters"]).splitlines() or Path(kubeconfig).exists():
                raise ValueError("fixture name collision")
            with claim.open("x", encoding="utf-8") as file:
                json.dump({"source": source, "run": run, "attempt": attempt, "cluster": name}, file)
            owned = True
            evidence["stage"] = "cluster"
            topology.retained.shared.create_enforcing_cluster(name, scratch)
            command(["kind", "load", "docker-image", image, "--name", name])
            crds = []
            for path in sorted((qualify.ROOT/"kanz/infra/security/spire/crds").glob("*.yaml")):
                crds += topology.retained.shared.manifests(str(path.relative_to(qualify.ROOT)))
            apply(crds)
            for crd in crds:
                k("wait", "--for=condition=Established", "crd/"+crd["metadata"]["name"], "--timeout=90s")
            apply(topology.retained.spire_objects())
            k("-n", "spire-system", "rollout", "status", "statefulset/spire-server", "--timeout=240s")
            k("-n", "spire-system", "rollout", "status", "daemonset/spire-agent", "--timeout=240s")
            apply(topology.retained.shared.manifests("kanz/infra/security/spire/registration.yaml"))
            evidence["stage"] = "brokers"
            apply(topology.retained.shared.manifests("kanz/infra/nats/namespace.yaml"))
            docs, cluster = broker_objects()
            apply([listener_policy(None)])
            apply(docs)
            k("-n", NS, "rollout", "status", "statefulset/kafka", "--timeout=240s")
            evidence["brokersBefore"] = broker_uids(k)
            desired = variants(raw, image, built, cluster)
            evidence["stage"] = "argo-install"
            revisions = argo_bootstrap.install(k, apply, command, scratch, desired, legacy=True)
            markers = ["before-native:"+source, "during-native:"+source]
            uids = set()
            for phase in PHASES:
                evidence["stage"] = phase
                owner = "native" if phase == "native-activate" else "legacy"
                require_empty(k)
                apply([listener_policy(owner)])
                diagnostic = {}
                evidence["syncDiagnostic"] = diagnostic
                sync = argo_bootstrap.sync(k, phase, revisions[phase], diagnostic, legacy=owner == "legacy")
                expected = desired[phase][-1]
                expected["metadata"]["name"] = sync["jobName"]
                if not diagnostic["resultMatches"]:
                    raise ValueError("Argo handoff sync differs")
                job, pod = terminal_job(k, expected, owner, uids)
                uid = job["metadata"]["uid"]
                uids.add(uid)
                sync.update(owner=owner, jobUID=uid, exclusive=True)
                if owner == "legacy":
                    config = json.loads(k("-n", NS, "get", "configmap", "kafka-topics", "-o", "json"))
                    if config.get("immutable") is not True or config["data"] != desired[phase][0]["data"]:
                        raise ValueError("legacy table differs")
                else:
                    custody = {}
                    if argo_bootstrap.observe_hook(k, expected, None, custody) != uid:
                        raise ValueError("native hook UID differs")
                    sync["receipt"] = native_receipt(k, expected, pod, built, name, source, cluster, raw, "apply")
                drain(k, apply, job)
                if phase == "legacy-bootstrap":
                    sync["historyDigest"] = history(k, markers[:1], append=True)
                elif phase == "native-activate":
                    sync["historyDigest"] = history(k, markers, append=True)
                else:
                    sync["historyDigest"] = history(k, markers)
                    if sync["historyDigest"] != evidence["stages"][-1]["historyDigest"]:
                        raise ValueError("fallback history differs")
                sync["drained"] = True
                evidence["stages"].append(sync)
            evidence["stage"] = "fallback-readback"
            # Legacy is drained and fenced. This final native observer runs
            # verify only, so it cannot change topic configuration or ACLs.
            verify = desired["native-activate"][-1]
            verify["metadata"].pop("generateName", None)
            verify["metadata"]["name"] = "handoff-verify"
            verify["metadata"].pop("annotations", None)
            verify["spec"]["template"]["spec"]["containers"][0]["args"][1] = "-mode=verify"
            apply([listener_policy("native"), verify])
            k("-n", NS, "wait", "--for=condition=complete", "job/handoff-verify", "--timeout=250s")
            job, pod = terminal_job(k, verify, "native", uids)
            evidence["fallbackReadback"] = native_receipt(k, verify, pod, built, name, source, cluster, raw, "verify")
            drain(k, apply, job)
            evidence["brokersAfter"] = broker_uids(k)
            if evidence["brokersBefore"] != evidence["brokersAfter"] or revisions[PHASES[0]] != revisions[PHASES[2]]:
                raise ValueError("fallback revision or broker identity differs")
            evidence["passed"] = True
        except Exception as error:
            evidence["errorCategory"] = type(error).__name__
            print(json.dumps({"stage": evidence["stage"], "errorCategory": evidence["errorCategory"]}), flush=True)
            raise RuntimeError("legacy handoff qualification failed") from None
        finally:
            if owned:
                try:
                    command(["kind", "delete", "cluster", "--name", name, "--kubeconfig", kubeconfig], timeout=180)
                    evidence["cleanup"] = name not in command(["kind", "get", "clusters"]).splitlines()
                except Exception:
                    pass
            evidence["passed"] &= evidence["cleanup"]
            artifact.write_text(json.dumps(evidence, sort_keys=True)+"\n", encoding="utf-8")
    if not evidence["passed"]:
        raise RuntimeError("owned cleanup unconfirmed")


if __name__ == "__main__":
    main()
