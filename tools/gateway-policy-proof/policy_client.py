"""Bounded policy API adapter; callers own verified cluster/workload scope."""
import json
import transaction as tx


class KubernetesClient:
    def __init__(self, kubectl, cluster_uid, check_workloads=lambda: None):
        self.k = kubectl
        self.cluster_uid = cluster_uid
        self.check_workloads = check_workloads

    def check_scope(self):
        obj = json.loads(self.k("get", "namespace", "kube-system", "-o", "json"))
        if obj["metadata"]["uid"] != self.cluster_uid:
            raise tx.Refusal("cluster identity changed")
        self.check_workloads()

    def list_policies(self):
        return json.loads(self.k("-n", tx.NAMESPACE, "get", "networkpolicies", "-o", "json"))["items"]

    def write(self, obj, replacing, dry_run=False):
        # Ordinary kubectl replace rewrites an existing last-applied annotation.
        # Raw requests preserve user metadata and still use API UID/RV CAS.
        objects = tx.inventory([obj])
        key = next(iter(objects))
        if key[0] != tx.NAMESPACE or key[1] not in tx.NAMES:
            raise tx.Refusal("write outside owned policies")
        if replacing:
            tx.version(obj)
        elif any(field in obj["metadata"] for field in ("uid", "resourceVersion")):
            raise tx.Refusal("create contains server identity")
        path = "/apis/networking.k8s.io/v1/namespaces/" + tx.NAMESPACE + "/networkpolicies"
        if replacing:
            path += "/" + key[1]
        # --dry-run is not consulted by kubectl's raw HTTP path.
        if dry_run:
            path += "?dryRun=All"
        result = self.k("replace" if replacing else "create", "--raw", path, "-f", "-", value=obj)
        if dry_run:
            actual = json.loads(result)
            tx.inventory([actual])
            if tx.semantic(actual) != tx.semantic(obj):
                raise tx.Refusal("server dry-run changed intended policy")

    def dry_run(self, obj, replacing):
        self.write(obj, replacing, dry_run=True)

    def create(self, obj):
        self.write(obj, False)

    def replace(self, obj):
        self.write(obj, True)

    def delete(self, key, uid, rv):
        if key[0] != tx.NAMESPACE or key[1] not in tx.NAMES:
            raise tx.Refusal("delete outside owned policies")
        self.k("delete", "--raw", "/apis/networking.k8s.io/v1/namespaces/" + key[0] + "/networkpolicies/" + key[1],
               "-f", "-", value={"apiVersion": "v1", "kind": "DeleteOptions",
                                 "preconditions": {"uid": uid, "resourceVersion": rv}})
