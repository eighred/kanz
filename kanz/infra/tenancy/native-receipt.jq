# The observer binds the receipt to the sole successful pod of this exact Job.
# Kubernetes may default fields, but must not change the requested executable.
if $phase == "pod" then
  .items as $items |
  ($items | length) == 1 and
  ($items[0] as $p | $created.spec.template.spec as $s |
    $p.metadata.namespace == $created.metadata.namespace and
    ($p.metadata.name | test("^[a-z0-9][-a-z0-9]*$")) and
    ($p.metadata.uid | test("^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$")) and
    ($p.metadata.ownerReferences | length) == 1 and
    $p.metadata.ownerReferences[0].uid == $uid and
    $p.metadata.ownerReferences[0].name == $job and
    $p.metadata.ownerReferences[0].kind == "Job" and
    $p.metadata.ownerReferences[0].controller == true and
    $p.spec.serviceAccountName == $s.serviceAccountName and
    ($p.spec.containers | length) == 1 and
    $p.spec.containers[0].name == "tenant" and
    $p.spec.containers[0].image == $s.containers[0].image and
    $p.spec.containers[0].args == $s.containers[0].args and
    ($p.spec.containers[0].command // []) == ($s.containers[0].command // []) and
    ($p.spec.containers[0].env // []) == ($s.containers[0].env // []) and
    $p.status.phase == "Succeeded" and
    ($p.status.containerStatuses | length) == 1 and
    $p.status.containerStatuses[0].name == "tenant" and
    $p.status.containerStatuses[0].restartCount == 0 and
    $p.status.containerStatuses[0].state.terminated.exitCode == 0)
elif $phase == "receipt" then
  length == 1 and (.[0] |
    (keys | sort) == ["receipt", "version"] and
    (.version | test("^[0-9a-f]{40}$")) and
    (.receipt |
      (keys | sort) == ["acl_attempts", "complete", "durability_profile", "expected_cluster_id", "manifest_sha256", "missing_topics", "mode", "partial_or_unknown", "topic_attempts", "verified_acls", "verified_topics"] and
      .mode == "apply" and .expected_cluster_id == $cluster and
      .manifest_sha256 == $digest and .durability_profile == $profile and
      .complete == true and .partial_or_unknown == false and
      .missing_topics == 0 and .verified_acls == 6 and
      (.verified_topics | type == "number" and . > 0 and . <= 256 and floor == .) and
      (.topic_attempts | type == "number" and . >= 0 and . <= 256 and floor == .) and
      (.acl_attempts | type == "number" and . >= 0 and . <= 6 and floor == .)))
else false end
