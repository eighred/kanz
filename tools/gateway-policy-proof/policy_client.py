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

    def dry_run(self, obj, replacing):
        self.k("replace" if replacing else "create", "--dry-run=server", "-f", "-", value=obj)

    def create(self, obj):
        self.k("create", "-f", "-", value=obj)

    def replace(self, obj):
        self.k("replace", "-f", "-", value=obj)

    def delete(self, key, uid, rv):
        if key[0] != tx.NAMESPACE or key[1] not in tx.NAMES:
            raise tx.Refusal("delete outside owned policies")
        self.k("delete", "--raw", "/apis/networking.k8s.io/v1/namespaces/" + key[0] + "/networkpolicies/" + key[1],
               "-f", "-", value={"apiVersion": "v1", "kind": "DeleteOptions",
                                 "preconditions": {"uid": uid, "resourceVersion": rv}})
