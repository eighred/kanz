"""Argo CD Core and Git source owned by the disposable broker fixture."""
import base64
import hashlib
import json
import re
import time

import yaml

VERSION = "v3.6.1"
CORE_SHA = "0b38189d6e1fd76ef5c3db10355026c48a02020d82bf21a1a1b5ab16dffe4f3c"
IMAGES = {
    "quay.io/argoproj/argocd:v3.6.1": "quay.io/argoproj/argocd:v3.6.1@sha256:296e3d74f449b2ce45265613fcb68c18ed7366488360c8ea32f5f2a93d2e5c54",
    "public.ecr.aws/docker/library/redis:8.10.1-alpine": "docker.io/library/redis:8.10.1-alpine@sha256:ef216d85ec44f4fdf7c0e6df2d1757a4734ed32fbb278cabd3181b5a27353b79",
}
BUSYBOX = "busybox:1.36@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662"
REPO = "http://fixture-git.argocd.svc:8080/source.git"
APP = "system-topics"
CASES = ("system-create", "system-repeat", "system-mismatch", "system-verify",
         "system-activate", "system-rollback")


def git_source(command, scratch, variants):
    work = scratch / "argo-source"
    bare = scratch / "source.git"
    work.mkdir()
    def git(*args):
        return command(["git", "-C", str(work), "-c", "user.name=Kanz Fixture",
                        "-c", "user.email=fixture@example.invalid", *args], timeout=20)
    git("init", "--initial-branch=fixture")
    revisions = {}
    for case, objects in variants.items():
        if case in ("system-repeat", "system-rollback"):
            revisions[case] = revisions["system-create"]
            continue
        jobs = [obj for obj in objects if obj["kind"] == "Job"]
        if len(jobs) != 1 or jobs[0]["metadata"]["name"] != "kafka-system-bootstrap":
            raise ValueError("Git revision must contain exactly one system hook")
        (work / "bootstrap.yaml").write_text(yaml.safe_dump_all(objects), encoding="utf-8")
        git("add", "bootstrap.yaml")
        git("-c", "commit.gpgsign=false", "commit", "-m", case)
        revisions[case] = git("rev-parse", "HEAD")
    git("clone", "--bare", "--no-hardlinks", str(work), str(bare))
    command(["git", "--git-dir", str(bare), "update-server-info"], timeout=20)
    files = {}
    items = []
    for path in sorted(bare.rglob("*")):
        relative = path.relative_to(bare).as_posix()
        if not path.is_file() or not (relative in ("HEAD", "packed-refs", "info/refs") or relative.startswith(("objects/", "refs/"))):
            continue
        key = "f" + str(len(files))
        files[key] = base64.b64encode(path.read_bytes()).decode("ascii")
        items.append({"key": key, "path": "source.git/" + relative})
    if not files or sum(map(len, files.values())) > 256 << 10:
        raise ValueError("fixture Git source bound")
    return revisions, files, items


def install(k, apply, command, scratch, variants):
    revisions, files, items = git_source(command, scratch, variants)
    path = scratch / "argo-core.yaml"
    command(["curl", "--fail", "--silent", "--show-error", "--location", "--max-time", "90",
             "-o", str(path), f"https://raw.githubusercontent.com/argoproj/argo-cd/{VERSION}/manifests/core-install.yaml"], timeout=95)
    raw = path.read_bytes()
    if hashlib.sha256(raw).hexdigest() != CORE_SHA:
        raise ValueError("Argo Core manifest digest differs")
    core = list(yaml.safe_load_all(raw))
    for obj in core:
        if obj.get("kind") not in ("StatefulSet", "Deployment"):
            continue
        spec = obj["spec"]["template"]["spec"]
        for container in spec.get("containers", []) + spec.get("initContainers", []):
            container["image"] = IMAGES[container["image"]]
    apply([{"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": "argocd"}}])
    k("-n", "argocd", "apply", "--server-side", "-f", "-", data=yaml.safe_dump_all(core))
    labels = {"app": "fixture-git"}
    meta = {"name": "fixture-git", "namespace": "argocd"}
    apply([
        {"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta,
         "immutable": True, "binaryData": files},
        {"apiVersion": "apps/v1", "kind": "Deployment", "metadata": meta, "spec": {
            "replicas": 1, "selector": {"matchLabels": labels},
            "template": {"metadata": {"labels": labels}, "spec": {
                "automountServiceAccountToken": False,
                "securityContext": {"runAsNonRoot": True, "runAsUser": 65532},
                "containers": [{"name": "http", "image": BUSYBOX,
                                "command": ["httpd", "-f", "-p", "8080", "-h", "/repo"],
                                "securityContext": {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True,
                                                    "capabilities": {"drop": ["ALL"]}},
                                "volumeMounts": [{"name": "git", "mountPath": "/repo", "readOnly": True}]}],
                "volumes": [{"name": "git", "configMap": {"name": "fixture-git", "items": items}}],
            }},
        }},
        {"apiVersion": "v1", "kind": "Service", "metadata": meta, "spec": {
            "selector": labels, "ports": [{"port": 8080}]}},
        {"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": meta, "spec": {
            "podSelector": {"matchLabels": labels}, "policyTypes": ["Ingress", "Egress"], "egress": [],
            "ingress": [{"from": [{"podSelector": {"matchLabels": {"app.kubernetes.io/name": "argocd-repo-server"}}}],
                         "ports": [{"port": 8080, "protocol": "TCP"}]}]}},
    ])
    for resource in ("deploy/fixture-git", "deploy/argocd-redis", "deploy/argocd-repo-server", "statefulset/argocd-application-controller"):
        k("-n", "argocd", "rollout", "status", resource, "--timeout=180s")
    apply([
        {"apiVersion": "argoproj.io/v1alpha1", "kind": "AppProject",
         "metadata": {"name": APP, "namespace": "argocd"},
         "spec": {"sourceRepos": [REPO], "destinations": [{"server": "https://kubernetes.default.svc", "namespace": "kanz-messaging"}],
                  "namespaceResourceWhitelist": [{"group": "", "kind": "ConfigMap"},
                                                 {"group": "", "kind": "ServiceAccount"},
                                                 {"group": "batch", "kind": "Job"}]}},
        {"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
         "metadata": {"name": APP, "namespace": "argocd"},
         "spec": {"project": APP, "source": {"repoURL": REPO, "targetRevision": revisions["system-create"], "path": "."},
                  "destination": {"server": "https://kubernetes.default.svc", "namespace": "kanz-messaging"},
                  "syncPolicy": {"syncOptions": ["ApplyOutOfSyncOnly=true"]}}},
    ])
    return revisions


def sync(k, case, revision, hook_name):
    token = [{"name": "fixture-case", "value": case}]
    # Let the controller compare the new target before requesting the sync.
    # This separates a target change from the operation and lets us distinguish
    # Git comparison failures from a PostSync refusal.
    def get_app():
        return json.loads(k("-n", "argocd", "get", "application", APP, "-o", "json", timeout=10))

    def patch(value):
        k("-n", "argocd", "patch", "application", APP, "--type=merge", "-p", json.dumps(value))

    patch({"metadata": {"annotations": {"argocd.argoproj.io/refresh": "normal"}},
           "spec": {"source": {"targetRevision": revision}}})
    comparison_deadline = time.monotonic() + 90
    while time.monotonic() < comparison_deadline:
        app = get_app()
        sync_status = app.get("status", {}).get("sync", {})
        if (sync_status.get("revision") == revision and sync_status.get("status") in ("Synced", "OutOfSync")
                and not app.get("operation") and not app["metadata"].get("annotations", {}).get("argocd.argoproj.io/refresh")):
            break
        time.sleep(1)
    else:
        raise TimeoutError("Argo target comparison did not converge")
    patch({"operation": {"info": token, "sync": {"revision": revision}}})
    deadline = time.monotonic() + 300
    while time.monotonic() < deadline:
        app = get_app()
        state = app.get("status", {}).get("operationState", {})
        if state.get("operation", {}).get("info") == token and state.get("finishedAt") and not app.get("operation"):
            result = state.get("syncResult", {})
            expected = "Failed" if case == "system-mismatch" else "Succeeded"
            hooks = [x for x in result.get("resources", []) if x.get("kind") == "Job" and x.get("name") == hook_name]
            if (state.get("phase") != expected or result.get("revision") != revision or len(hooks) != 1 or
                    hooks[0].get("namespace") != "kanz-messaging" or hooks[0].get("syncPhase") != "PostSync" or
                    hooks[0].get("hookPhase") != expected):
                phases = {"Succeeded", "Failed", "Error", "Running", "Terminating"}
                phase = state.get("phase")
                hook_phases = [h.get("hookPhase") if h.get("hookPhase") in phases else "UNKNOWN" for h in hooks[:4]]
                print(json.dumps({"case": case, "phase": phase if phase in phases else "UNKNOWN",
                                  "revisionMatches": result.get("revision") == revision,
                                  "hookCount": len(hooks), "hookPhases": hook_phases}), flush=True)
                raise ValueError("Argo PostSync result differs")
            return {"case": case, "revision": revision, "phase": expected, "hookPhase": expected}
        time.sleep(1)
    raise TimeoutError("Argo sync observation expired")


def observe_owner(k, hook_name):
    apps = json.loads(k("get", "applications.argoproj.io", "-A", "-o", "json", timeout=10))["items"]
    owners = [app["metadata"]["name"] for app in apps
              if app["spec"]["destination"].get("namespace") == "kanz-messaging"]
    jobs = json.loads(k("-n", "kanz-messaging", "get", "jobs", "-o", "json", timeout=10))["items"]
    hooks = []
    for job in jobs:
        name = job["metadata"]["name"]
        containers = job["spec"]["template"]["spec"]["containers"]
        if name in (hook_name, "kafka-topics") or any(
            "-system-bootstrap" in c.get("args", []) or "/scripts/topics.sh" in c.get("command", [])
            for c in containers
        ):
            hooks.append(name)
    if owners != [APP] or hooks != [hook_name]:
        raise ValueError("system topic owner inventory differs")
    return {"application": APP, "hook": hook_name}


def validate(observations):
    if not isinstance(observations, list) or tuple(x.get("case") for x in observations) != CASES:
        raise ValueError("Argo sync inventory differs")
    for row in observations:
        expected = "Failed" if row["case"] == "system-mismatch" else "Succeeded"
        if (set(row) != {"case", "revision", "phase", "hookPhase", "jobUID", "owner"} or
                not re.fullmatch("[0-9a-f]{40}", row["revision"]) or
                not re.fullmatch("[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}", row["jobUID"]) or
                row["phase"] != expected or row["hookPhase"] != expected or
                row["owner"] != {"application": APP, "hook": "kafka-system-bootstrap"}):
            raise ValueError("Argo sync evidence differs")
    revisions = [x["revision"] for x in observations]
    if (revisions[0] != revisions[1] or revisions[0] != revisions[-1] or len(set(revisions)) != 4
            or len({x["jobUID"] for x in observations}) != len(CASES)):
        raise ValueError("Argo revision or Job continuity differs")
