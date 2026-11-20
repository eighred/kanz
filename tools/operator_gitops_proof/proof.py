"""Operator identity reconciliation in a new, job-owned Argo CD/Kind fixture."""
import base64
import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import sys
import tempfile
import time
import uuid

import yaml

ROOT = Path(__file__).resolve().parents[2]
NAMESPACE = "kanz-operator"
SOURCE_PATH = "kanz/infra/operator"
APP = "kanz-primary-operator"
REPO = "http://operator-fixture-git.argocd.svc:8080/source.git"
CASES = ("initial-sync", "identity-self-heal", "same-revision-sync", "revision-change", "rollback")
MARKER = "kanz.io/operator-gitops-fixture"


def load_module(name, path):
    spec = importlib.util.spec_from_file_location(name, ROOT / path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def source_documents(path):
    return [item for item in yaml.safe_load_all((ROOT / path).read_text(encoding="utf-8")) if item]


def identity_key(item):
    metadata = item["metadata"]
    return item["kind"], metadata.get("namespace", ""), metadata["name"]


def operator_source():
    appset = source_documents("kanz/infra/gitops/applicationset.yaml")[0]
    generators = appset["spec"]["generators"][0]["matrix"]["generators"]
    elements = [element for generator in generators for element in generator["list"]["elements"]]
    components = [element for element in elements if element.get("path") == SOURCE_PATH]
    environments = [element for element in elements if element.get("env") == "primary"]
    if components != [{"component": "operator", "path": SOURCE_PATH, "namespace": NAMESPACE}] or len(environments) != 1:
        raise ValueError("operator component identity differs")
    template = appset["spec"]["template"]
    values = dict(components[0], **environments[0])

    def render(value):
        if isinstance(value, dict):
            return {key: render(item) for key, item in value.items()}
        if isinstance(value, list):
            return [render(item) for item in value]
        if isinstance(value, str):
            for key, item in values.items():
                value = value.replace("{{." + key + "}}", item)
            if "{{" in value:
                raise ValueError("unresolved operator Application template")
        return value

    application = {"apiVersion": "argoproj.io/v1alpha1", "kind": "Application", **render(template)}
    application["metadata"]["namespace"] = "argocd"
    directory = application["spec"]["source"]["directory"]
    raw_exclude = directory.get("exclude", "")
    exclusions = raw_exclude.strip("{}").split(",")
    if (application["metadata"]["name"] != APP or application["spec"]["destination"] != {
            "server": "https://kubernetes.default.svc", "namespace": NAMESPACE}
            or directory.get("recurse") is not True or directory.get("include")
            or any(any(char in entry for char in "*?[") for entry in exclusions)
            or exclusions.count("halt-job.yaml") != 1
            or application["spec"]["syncPolicy"].get("automated") != {"prune": True, "selfHeal": True}):
        raise ValueError("operator automatic reconciliation boundary differs")
    files, resources, invocation = {}, {}, 0
    for path in sorted((ROOT / SOURCE_PATH).rglob("*")):
        if path.is_dir():
            continue
        if path.suffix == ".jsonnet":
            raise ValueError("Jsonnet cannot establish the static operator identity inventory")
        if path.suffix not in (".yaml", ".yml", ".json"):
            continue
        relative = path.relative_to(ROOT / SOURCE_PATH).as_posix()
        if path.is_symlink():
            raise ValueError("operator source symlink refused")
        canonical = path.read_bytes()
        if path.suffix == ".json":
            objects = [json.loads(canonical)]
        else:
            objects = [item for item in yaml.safe_load_all(canonical) if item]
        revised = canonical
        if relative == "halt-job.yaml":
            if len(objects) != 1 or identity_key(objects[0]) != ("Job", NAMESPACE, "kanz-halt"):
                raise ValueError("manual invocation contains identity resources")
            # Even an exclusion regression can never execute the real brake in
            # this fixture: the source-derived Job is unconditionally suspended.
            objects[0]["spec"]["suspend"] = True
            canonical = revised = yaml.safe_dump_all(objects).encode("utf-8")
            invocation += 1
        else:
            if b"+argocd:skip-file-rendering" in canonical:
                raise ValueError("source directive skips an operator identity")
            if relative in exclusions:
                raise ValueError("ordinary operator identity excluded")
            for item in objects:
                key = identity_key(item)
                if key in resources or key[0] not in ("Namespace", "ServiceAccount", "NetworkPolicy"):
                    raise ValueError("unexpected automatic operator resource")
                if (key[0] == "Namespace" and (key != ("Namespace", "", NAMESPACE)
                        or item["metadata"].get("labels", {}).get("kanz.internal/spiffe") != "enabled")):
                    raise ValueError("operator namespace lost SPIRE issuance")
                if key[0] != "Namespace" and key[1] != NAMESPACE:
                    raise ValueError("operator identity namespace differs")
                resources[key] = item
                if key == ("ServiceAccount", NAMESPACE, "kanz-model"):
                    model = copy.deepcopy(item)
                    model["metadata"].setdefault("annotations", {})[MARKER] = "revised"
                    if path.suffix == ".json":
                        revised = json.dumps(model).encode("utf-8")
                    else:
                        revised = yaml.safe_dump_all(model if doc is item else doc for doc in objects).encode("utf-8")
        files[relative] = (canonical, revised)
    accounts = {key[2] for key in resources if key[0] == "ServiceAccount"}
    policies = {key[2] for key in resources if key[0] == "NetworkPolicy"}
    if (invocation != 1 or len(accounts) != 6 or policies != {name + "-egress" for name in accounts}
            or sum(key[0] == "Namespace" for key in resources) != 1):
        raise ValueError("operator identity inventory differs")
    project = source_documents("kanz/infra/gitops/projects.yaml")[0]
    if not any(item.get("namespace") == NAMESPACE for item in project["spec"]["destinations"]):
        raise ValueError("operator AppProject destination denied")
    return files, resources, application, project


def check_operation(application, revision, token=None, *, automated=False):
    state = application.get("status", {}).get("operationState", {})
    result = state.get("syncResult", {})
    sync = application.get("status", {}).get("sync", {})
    if application.get("spec", {}).get("source", {}).get("targetRevision") != revision:
        return False
    if automated and state.get("operation", {}).get("initiatedBy", {}).get("automated") is not True:
        return False
    if token is not None and state.get("operation", {}).get("info") != token:
        return False
    if (application.get("operation") or not state.get("finishedAt") or
            result.get("revision") != revision or sync.get("revision") != revision):
        return False
    if state.get("phase") != "Succeeded":
        raise ValueError("exact-revision Argo operation refused")
    return sync.get("status") == "Synced"


def operation_observation(application, revision, token, resources):
    """Closed scalar diagnostics; never retain raw Application or resource data."""
    status = application.get("status", {})
    state = status.get("operationState", {})
    result = state.get("syncResult", {})
    sync = status.get("sync", {})
    phase = state.get("phase")
    sync_status = sync.get("status")
    found = status.get("resources", [])
    bounded = type(found) is list and len(found) <= 64
    actual = set()
    if bounded:
        for item in found:
            if type(item) is not dict:
                bounded = False
                break
            key = item.get("kind"), item.get("namespace", ""), item.get("name")
            if any(type(value) is not str or len(value) > 128 for value in key):
                bounded = False
                break
            actual.add(key)
    return {
        "applicationObserved": True,
        "sync": sync_status if sync_status in ("Synced", "OutOfSync") else "UNKNOWN",
        "phase": phase if phase in ("Running", "Terminating", "Succeeded", "Failed", "Error") else "UNKNOWN",
        "targetRevisionMatches": application.get("spec", {}).get("source", {}).get("targetRevision") == revision,
        "operationRevisionMatches": result.get("revision") == revision,
        "comparisonRevisionMatches": sync.get("revision") == revision,
        "manualRequest": token is not None,
        "requestInfoMatches": token is not None and state.get("operation", {}).get("info") == token,
        "automatedOperation": state.get("operation", {}).get("initiatedBy", {}).get("automated") is True,
        "operationPending": bool(application.get("operation")),
        "operationFinished": bool(state.get("finishedAt")),
        "resourceInventoryBounded": bounded,
        "missingResourceCount": len(set(resources) - actual) if bounded else None,
        "unexpectedResourceCount": len(actual - set(resources)) if bounded else None,
        "readbackMatches": None,
    }


def validate_creation_authorization(value, source, run_id, run_attempt):
    """A preflight collision must never manufacture a cleanup entitlement."""
    if (type(value) is not dict or set(value) != {
            "schema", "cluster", "source", "runID", "runAttempt", "creationID", "creationAuthorized"}
            or type(value["schema"]) is not int or value["schema"] != 1
            or value["creationAuthorized"] is not True
            or not re.fullmatch(r"[0-9a-f]{40}", source)
            or not re.fullmatch(r"[1-9][0-9]*", run_id)
            or not re.fullmatch(r"[1-9][0-9]*", run_attempt)
            or value["source"] != source or value["runID"] != run_id or value["runAttempt"] != run_attempt
            or type(value["creationID"]) is not str or not re.fullmatch(r"[0-9a-f]{32}", value["creationID"])
            or value["cluster"] != "messaging-proof-" + value["creationID"][:12]):
        raise ValueError("operator creation authorization refused")
    return value


def check_inventory(resources, accounts, policies, jobs, namespace, marker):
    wanted_accounts = {key[2] for key in resources if key[0] == "ServiceAccount"}
    wanted_policies = {key[2] for key in resources if key[0] == "NetworkPolicy"}
    actual_accounts = {item["metadata"]["name"] for item in accounts}
    if actual_accounts != wanted_accounts | {"default"}:
        raise ValueError("actual operator service-account inventory differs")
    for item in accounts:
        if item["metadata"]["name"] == "default":
            continue
        desired = resources[("ServiceAccount", NAMESPACE, item["metadata"]["name"])]
        if item.get("imagePullSecrets", []) != desired.get("imagePullSecrets", []):
            raise ValueError("actual operator image-pull identity differs")
        if any(item["metadata"].get("labels", {}).get(key) != value
               for key, value in desired["metadata"].get("labels", {}).items()):
            raise ValueError("actual operator service-account labels differ")
    if {item["metadata"]["name"] for item in policies} != wanted_policies:
        raise ValueError("actual operator policy inventory differs")
    for item in policies:
        desired = resources[("NetworkPolicy", NAMESPACE, item["metadata"]["name"])]["spec"]
        # API defaults policyTypes but the canonical manifests specify them.
        if item["spec"] != desired:
            raise ValueError("actual operator egress policy differs")
    if jobs:
        raise ValueError("manual halt Job reached automatic reconciliation")
    if namespace["metadata"].get("labels", {}).get("kanz.internal/spiffe") != "enabled":
        raise ValueError("actual operator namespace lost SPIRE label")
    model = next(item for item in accounts if item["metadata"]["name"] == "kanz-model")
    if model["metadata"].get("annotations", {}).get(MARKER) != marker:
        raise ValueError("actual operator revision marker differs")


class Fixture:
    def __init__(self, scratch, name):
        if not re.fullmatch(r"messaging-proof-[0-9a-f]{12}", name):
            raise ValueError("invalid disposable operator cluster name")
        self.scratch, self.name = scratch, name
        self.config = scratch / "kubeconfig"
        self.environment = {key: os.environ[key] for key in ("PATH", "RUNNER_TEMP")}
        for key, directory in (("HOME", "home"), ("TMPDIR", "tmp")):
            path = scratch / directory
            path.mkdir()
            self.environment[key] = str(path)
        self.environment["KUBECONFIG"] = str(self.config)
        self.process = load_module("operator_bounded_process", "tools/gateway_policy_proof/bounded_process.py")
        self.stage = "initialization"
        self.creation_id = name.removeprefix("messaging-proof-") + uuid.uuid4().hex[12:]
        self.creation_authorization = None
        self.owner = Path(os.environ["RUNNER_TEMP"]) / "operator-gitops-owner.json"

    def command(self, args, data=None, timeout=60):
        return self.process.run(args, cwd=ROOT, env=self.environment, data=data,
                                timeout=timeout, stdout_limit=4 << 20, stderr_limit=65536)

    def k(self, *args, data=None, timeout=30):
        return self.command(["kubectl", "--kubeconfig", str(self.config), "--context", "kind-" + self.name,
                             *args], data=data, timeout=timeout)

    def apply(self, objects):
        self.k("apply", "--server-side", "-f", "-", data=yaml.safe_dump_all(objects))

    def git_source(self, files):
        work = self.scratch / "git"
        work.mkdir()
        operator = work / SOURCE_PATH
        operator.mkdir(parents=True)

        def git(*args):
            return self.command(["git", "-C", str(work), "-c", "user.name=Kanz Fixture",
                                 "-c", "user.email=fixture@example.invalid", *args], timeout=20)

        git("init", "--initial-branch=fixture")
        revisions = []
        for revised in (False, True):
            for name, (original, marked) in files.items():
                target = operator / name
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_bytes(marked if revised else original)
            git("add", ".")
            git("-c", "commit.gpgsign=false", "commit", "-m", "revised" if revised else "canonical")
            revisions.append(git("rev-parse", "HEAD"))
        bare = self.scratch / "source.git"
        git("clone", "--bare", "--no-hardlinks", str(work), str(bare))
        self.command(["git", "--git-dir", str(bare), "update-server-info"], timeout=20)
        data, items = {}, []
        for path in sorted(bare.rglob("*")):
            relative = path.relative_to(bare).as_posix()
            if not path.is_file() or not (relative in ("HEAD", "packed-refs", "info/refs") or relative.startswith(("objects/", "refs/"))):
                continue
            key = "f" + str(len(data))
            data[key] = base64.b64encode(path.read_bytes()).decode("ascii")
            items.append({"key": key, "path": "source.git/" + relative})
        if not data or sum(map(len, data.values())) > 256 << 10:
            raise ValueError("operator fixture Git source exceeds bound")
        return revisions, data, items

    def authorize_creation(self, source):
        if self.creation_authorization is not None:
            raise ValueError("operator creation already authorized")
        if self.name in self.command(["kind", "get", "clusters"]).splitlines():
            raise ValueError("operator fixture refuses cluster adoption")
        identity = {"schema": 1, "cluster": self.name, "source": source,
                    "runID": os.environ["GITHUB_RUN_ID"], "runAttempt": os.environ["GITHUB_RUN_ATTEMPT"],
                    "creationID": self.creation_id, "creationAuthorized": True}
        validate_creation_authorization(identity, source, os.environ["GITHUB_RUN_ID"], os.environ["GITHUB_RUN_ATTEMPT"])
        # This intent record is issued only at the actual create-command edge.
        # It survives a lost create response so only this job can clean up its
        # ambiguous partial creation; an absent-name preflight alone issues none.
        with self.owner.open("x", encoding="utf-8") as output:
            json.dump(identity, output)
            output.flush()
            os.fsync(output.fileno())
        self.creation_authorization = identity

    def install(self, files, application, project, source):
        self.stage = "create-disposable-cluster"
        feed = load_module("operator_feed_cluster", "tools/feed_identity/proof.py")
        if self.name in self.command(["kind", "get", "clusters"]).splitlines():
            raise ValueError("operator fixture refuses cluster adoption")

        def creation_command(args, data=None, timeout=60):
            if args[:3] == ["kind", "create", "cluster"]:
                if (args.count("--name") != 1 or args[args.index("--name") + 1] != self.name
                        or args.count("--kubeconfig") != 1 or args[args.index("--kubeconfig") + 1] != str(self.config)):
                    raise ValueError("operator create command ownership differs")
                self.authorize_creation(source)
            return self.command(args, data=data, timeout=timeout)

        feed.command = creation_command
        feed.create_enforcing_cluster(self.name, self.scratch)
        # Reuse the reviewed Argo version, manifest digest and immutable images;
        # no broker, hook or execution-result receipt code is invoked.
        sys.path.insert(0, str(ROOT / "tools/native_kafka_provision"))
        import argo_bootstrap
        self.stage = "install-pinned-Argo-Core"
        path = self.scratch / "argo-core.yaml"
        self.command(["curl", "--fail", "--silent", "--show-error", "--location", "--max-time", "90",
                      "-o", str(path), f"https://raw.githubusercontent.com/argoproj/argo-cd/{argo_bootstrap.VERSION}/manifests/core-install.yaml"], timeout=95)
        raw = path.read_bytes()
        if hashlib.sha256(raw).hexdigest() != argo_bootstrap.CORE_SHA:
            raise ValueError("Argo Core manifest digest differs")
        core = list(yaml.safe_load_all(raw))
        for obj in core:
            if obj.get("kind") in ("Deployment", "StatefulSet"):
                pod = obj["spec"]["template"]["spec"]
                for container in pod.get("containers", []) + pod.get("initContainers", []):
                    container["image"] = argo_bootstrap.IMAGES[container["image"]]
        self.apply([{"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": "argocd"}}])
        self.k("-n", "argocd", "apply", "--server-side", "-f", "-", data=yaml.safe_dump_all(core))
        revisions, data, items = self.git_source(files)
        labels = {"app": "operator-fixture-git"}
        metadata = {"name": "operator-fixture-git", "namespace": "argocd"}
        self.apply([
            {"apiVersion": "v1", "kind": "ConfigMap", "metadata": metadata, "immutable": True, "binaryData": data},
            {"apiVersion": "apps/v1", "kind": "Deployment", "metadata": metadata, "spec": {
                "replicas": 1, "selector": {"matchLabels": labels}, "template": {
                    "metadata": {"labels": labels}, "spec": {"automountServiceAccountToken": False,
                        "securityContext": {"runAsNonRoot": True, "runAsUser": 65532},
                        "containers": [{"name": "http", "image": argo_bootstrap.BUSYBOX,
                            "command": ["httpd", "-f", "-p", "8080", "-h", "/repo"],
                            "securityContext": {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True,
                                                "capabilities": {"drop": ["ALL"]}},
                            "volumeMounts": [{"name": "source", "mountPath": "/repo", "readOnly": True}]}],
                        "volumes": [{"name": "source", "configMap": {"name": metadata["name"], "items": items}}]}}}},
            {"apiVersion": "v1", "kind": "Service", "metadata": metadata, "spec": {"selector": labels, "ports": [{"port": 8080}]}},
            {"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": metadata, "spec": {
                "podSelector": {"matchLabels": labels}, "policyTypes": ["Ingress", "Egress"], "egress": [],
                "ingress": [{"from": [{"podSelector": {"matchLabels": {"app.kubernetes.io/name": "argocd-repo-server"}}}],
                             "ports": [{"port": 8080, "protocol": "TCP"}]}]}}
        ])
        for resource in ("deploy/operator-fixture-git", "deploy/argocd-redis", "deploy/argocd-repo-server", "statefulset/argocd-application-controller"):
            self.k("-n", "argocd", "rollout", "status", resource, "--timeout=180s", timeout=185)
        self.stage = "register-source-derived-Application"
        project["spec"]["sourceRepos"] = [REPO]
        application["spec"]["source"]["repoURL"] = REPO
        application["spec"]["source"]["targetRevision"] = revisions[0]
        self.apply([project, application])
        return revisions

    def observe(self, resources, revision, marker, *, request_sync=False, automated=False, deadline=None):
        if deadline is None:
            deadline = time.monotonic() + 120
        self.last_observation = {"applicationObserved": False}
        def remaining():
            budget = deadline - time.monotonic()
            if budget <= 0:
                raise TimeoutError("operator identity reconciliation deadline expired")
            return min(10, budget)

        token = None
        if request_sync:
            token = [{"name": "operator-gitops-request", "value": uuid.uuid4().hex}]
            self.k("-n", "argocd", "patch", "application", APP, "--type=merge", "-p", json.dumps({
                "operation": {"info": token, "sync": {"revision": revision, "prune": True}}}), timeout=remaining())

        def observed(*args):
            return json.loads(self.k(*args, timeout=remaining()))

        while time.monotonic() < deadline:
            app = observed("-n", "argocd", "get", "application", APP, "-o", "json")
            self.last_observation = operation_observation(app, revision, token, resources)
            if check_operation(app, revision, token, automated=automated):
                accounts = observed("-n", NAMESPACE, "get", "serviceaccounts", "-o", "json")["items"]
                policies = observed("-n", NAMESPACE, "get", "networkpolicies", "-o", "json")["items"]
                jobs = observed("-n", NAMESPACE, "get", "jobs", "-o", "json")["items"]
                namespace = observed("get", "namespace", NAMESPACE, "-o", "json")
                try:
                    check_inventory(resources, accounts, policies, jobs, namespace, marker)
                except ValueError:
                    self.last_observation["readbackMatches"] = False
                    # A self-heal may be in flight after a resource deletion.
                    # Never admit the dangerous Job, even transiently.
                    if jobs:
                        raise
                else:
                    self.last_observation["readbackMatches"] = True
                    actual = {(item["kind"], item.get("namespace", ""), item["name"])
                              for item in app["status"].get("resources", [])}
                    if actual != set(resources):
                        raise ValueError("Argo resource inventory differs from source")
                    if time.monotonic() > deadline:
                        raise TimeoutError("late operator reconciliation success refused")
                    return
            time.sleep(min(1, max(0, deadline - time.monotonic())))
        raise TimeoutError("operator identity reconciliation deadline expired")

    def target(self, revision):
        self.k("-n", "argocd", "patch", "application", APP, "--type=merge", "-p", json.dumps({
            "spec": {"source": {"targetRevision": revision}}}), timeout=10)

    def rollback(self, resources, revision, report):
        deadline = time.monotonic() + 120
        self.stage = "automatic-rollback"
        self.target(revision)
        self.observe(resources, revision, None, automated=True, deadline=deadline)
        report["automaticRollback"] = self.last_observation.copy()
        self.stage = "rollback-resync"
        self.observe(resources, revision, None, request_sync=True, deadline=deadline)
        report["rollbackResync"] = self.last_observation.copy()

    def cleanup(self):
        if self.creation_authorization is None:
            return False
        identity = self.creation_authorization
        recorded = json.loads(self.owner.read_text(encoding="utf-8"))
        validate_creation_authorization(recorded, identity["source"], identity["runID"], identity["runAttempt"])
        if recorded != identity or recorded["cluster"] != self.name:
            raise ValueError("operator cleanup authorization differs from creation")
        self.command(["kind", "delete", "cluster", "--name", self.name, "--kubeconfig", str(self.config)], timeout=180)
        if self.name in self.command(["kind", "get", "clusters"]).splitlines():
            raise ValueError("owned operator cluster remains")
        self.owner.unlink()
        self.creation_authorization = None
        return True


def main():
    if os.name != "posix" or os.environ.get("GITHUB_ACTIONS") != "true":
        raise RuntimeError("operator proof requires its disposable Linux CI job")
    report = {"passed": False, "cleanup": False, "cases": []}
    with tempfile.TemporaryDirectory(prefix="operator-gitops-", dir=os.environ["RUNNER_TEMP"]) as directory:
        fixture = Fixture(Path(directory), "messaging-proof-" + uuid.uuid4().hex[:12])
        source = fixture.command(["git", "rev-parse", "HEAD"])
        if not re.fullmatch(r"[0-9a-f]{40}", source) or source != os.environ["GITHUB_SHA"]:
            raise ValueError("operator source identity differs")
        report.update(source=source, tree=fixture.command(["git", "rev-parse", "HEAD^{tree}"]), cluster=fixture.name)
        try:
            files, resources, application, project = operator_source()
            revisions = fixture.install(files, application, project, source)
            for case in CASES:
                fixture.stage = case
                marker = "revised" if case == "revision-change" else None
                revision = revisions[1] if case == "revision-change" else revisions[0]
                if case == "identity-self-heal":
                    fixture.k("-n", NAMESPACE, "delete", "serviceaccount", "kanz-model", "--wait=true")
                if case == "revision-change":
                    fixture.target(revision)
                if case == "rollback":
                    fixture.rollback(resources, revision, report)
                else:
                    fixture.observe(resources, revision, marker, request_sync=case == "same-revision-sync")
                report["cases"].append(case)
            report["passed"] = report["cases"] == list(CASES)
        except Exception as error:
            report["failedStage"] = fixture.stage
            report["failure"] = ("deadline" if isinstance(error, TimeoutError) else
                                 "boundary-refused" if isinstance(error, ValueError) else "command-or-source-refused")
            if isinstance(error, TimeoutError) and getattr(fixture, "last_observation", None) is not None:
                report["operationObservation"] = fixture.last_observation
            command = fixture.process.command_failure(error)
            if command is not None:
                report["command"] = command
        finally:
            try:
                report["cleanup"] = fixture.cleanup()
            finally:
                print(json.dumps(report, sort_keys=True))
    if not report["passed"] or not report["cleanup"]:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
