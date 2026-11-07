"""Finite snapshot owner evidence: exact source, binaries, both broker versions, no skips."""
import argparse
from collections import Counter
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
from check_migration_evidence import verify, PREFIX

PACKAGE = "services/lake-sink/internal/snapshot"
VERSIONS = ("2.12.6", "2.14.5")
NAMES = set(['TestCallerAndResourceAuthorization',
 'TestCallerAndResourceAuthorization/foreign',
 'TestCallerAndResourceAuthorization/missing',
 'TestCallerAndResourceAuthorization/own',
 'TestCallerAndResourceAuthorization/portfolio-restricted',
 'TestCallerAndResourceAuthorization/read-is-not-snapshot-grant',
 'TestCallerAndResourceAuthorization/subject',
 'TestCallerAndResourceAuthorization/unknown-owner',
 'TestConvertLandingBoundsForeignRowsBeforeSelection',
 'TestConvertLandingRefusesUntrustedProjection',
 'TestConvertLandingTenantCustodyAndDuplicateOrdinal',
 'TestCreateAcceptanceDoesNotRevealProgress',
 'TestLifecycleRecordsPassCanonicalProducerValidation',
 'TestOperationReservationIsNonqueueingAndRetainsUnjoinedLifetime',
 'TestSnapshotActualBrokerAccountProfileAndRestartRefusal',
 'TestSnapshotActualBrokerProfileCapacityAndAdministrativeRefusal',
 'TestSnapshotActualBrokerProfileCapacityAndAdministrativeRefusal/create',
 'TestSnapshotActualBrokerProfileCapacityAndAdministrativeRefusal/delete',
 'TestSnapshotActualBrokerProfileCapacityAndAdministrativeRefusal/purge',
 'TestSnapshotActualBrokerProfileCapacityAndAdministrativeRefusal/update',
 'TestSnapshotActualCanonicalArchiverFourFactsRestartAndTenantRefusal',
 'TestSnapshotActualOwnerKillReplaysPersistedCut',
 'TestSnapshotActualParkingAndRegisteredArchiverPreflight',
 'TestSnapshotActualRuntimeDurablePublicationQueryAndEstablishedChannelFailure',
 'TestSnapshotActualVerifiedDescriptorSurvivesPathReplacementAndClosesInheritedFD',
 'TestSnapshotActualWorkerConvertAndQuery',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/CreateSnapshot',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/CreateSnapshot/foreign',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/CreateSnapshot/generic',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/CreateSnapshot/missing',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/CreateSnapshot/restricted',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/CreateSnapshot/unknown-wrapper',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/CreateSnapshot/valid',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/CreateSnapshot/wrong-action',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/CreateSnapshot/wrong-method',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/GetSnapshot',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/GetSnapshot/foreign',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/GetSnapshot/generic',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/GetSnapshot/missing',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/GetSnapshot/restricted',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/GetSnapshot/unknown-wrapper',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/GetSnapshot/valid',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/GetSnapshot/wrong-action',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/GetSnapshot/wrong-method',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/QuerySnapshot',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/QuerySnapshot/foreign',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/QuerySnapshot/generic',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/QuerySnapshot/missing',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/QuerySnapshot/restricted',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/QuerySnapshot/unknown-wrapper',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/QuerySnapshot/valid',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/QuerySnapshot/wrong-action',
 'TestSnapshotDelegationRequiresEveryMethodCallerAndExplicitAction//lake.v1.LakeSnapshotService/QuerySnapshot/wrong-method',
 'TestSnapshotGRPCAuthenticatesExactGatewayBeforeDelegation',
 'TestSnapshotGRPCAuthenticatesExactGatewayBeforeDelegation//ns/kanz-services/sa/api-gateway',
 'TestSnapshotGRPCAuthenticatesExactGatewayBeforeDelegation//ns/kanz-services/sa/other',
 'TestSnapshotGRPCStopJoinsCancelledHeldHandler',
 'TestSnapshotImmutablePublicationAndExactRetry',
 'TestSnapshotPostgresAtomicOutboxRefusal',
 'TestSnapshotPostgresAuthorizationIdempotencyAndRLS',
 'TestSnapshotPostgresGenerationExhaustionKeepsOriginalCutAndFailureFact',
 'TestSnapshotPostgresQuotasAndPersistedCut',
 'TestSnapshotPublicationRefusesStagingCollisionAndLinkedMembers',
 'TestSnapshotSelectionAndCursorPresence',
 'TestWorkerResultClosedProjectionAndPaging',
 'TestWorkerResultClosedProjectionAndPaging/exhausted-with-cursor',
 'TestWorkerResultClosedProjectionAndPaging/invalid-time',
 'TestWorkerResultClosedProjectionAndPaging/missing-cursor',
 'TestWorkerResultClosedProjectionAndPaging/unknown-event-wire',
 'TestWorkerResultClosedProjectionAndPaging/unknown-time-wire',
 'TestWorkerResultClosedProjectionAndPaging/wrong-arm'])
NAMES |= {"TestSnapshotActualRequestedParkingPreservesOriginalOrRetainsFailure"} | {
    "TestSnapshotActualRequestedParkingPreservesOriginalOrRetainsFailure/" + mode
    for mode in ("terminal-poison", "parking-refused", "transient-retained")
}
TOP_LEVEL = {name for name in NAMES if "/" not in name}
BINARIES = ("worker", "worker-swap", "owner.test", "gateway.test", "archiver.test", "kanz-lake-snapshot", "nats-2.12.6", "nats-2.14.5")
HEX40 = re.compile(r"[0-9a-f]{40}\Z")
HEX64 = re.compile(r"[0-9a-f]{64}\Z")


def strict(pairs):
    result = {}
    for key, value in pairs:
        if key in result: raise ValueError("duplicate evidence key")
        result[key] = value
    return result


def read_file(path, limit):
    if sys.platform != "linux" or not hasattr(os, "O_NOFOLLOW") or not hasattr(os, "O_NONBLOCK"):
        raise ValueError("Linux no-follow/nonblocking reader required")
    with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK), "rb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_size > limit: raise ValueError("invalid bounded proof file")
        raw = stream.read(limit + 1)
        after = os.fstat(stream.fileno())
    identity = lambda item: (item.st_mode, item.st_nlink, item.st_dev, item.st_ino, item.st_size, item.st_mtime_ns, item.st_ctime_ns)
    if not stat.S_ISREG(after.st_mode) or after.st_nlink != 1 or len(raw) != info.st_size or len(raw) > limit or identity(info) != identity(after):
        raise ValueError("proof changed during read")
    return raw


def check_stream(raw, version):
    events = [json.loads(line, object_pairs_hook=strict) for line in raw.decode("utf-8").splitlines() if line]
    verify(events, {PACKAGE: NAMES})
    counts = Counter(e.get("Test", "") for e in events if e.get("Action") == "pass" and e.get("Package") == PREFIX + PACKAGE)
    if counts != Counter({**{name: 1 for name in NAMES}, "": 1}): raise ValueError("unexpected/duplicate/missing outcome")
    started = Counter(e.get("Test") for e in events if e.get("Action") == "run")
    if started != Counter({name: 1 for name in NAMES}): raise ValueError("missing/fake executed test start")
    if any(e.get("Package") != PREFIX + PACKAGE for e in events): raise ValueError("unexpected proof package")
    output = "".join(e.get("Output", "") for e in events)
    if "(cached)" in output: raise ValueError("cached proof refused")
    for name in TOP_LEVEL:
        if name.startswith("TestSnapshotActual") and any(word in name for word in ("Broker", "Archiver", "Runtime", "OwnerKill", "Parking")):
            body = "".join(e.get("Output", "") for e in events if e.get("Test") == name or e.get("Test", "").startswith(name + "/"))
            if "actual snapshot broker version: " + version not in body: raise ValueError("missing exact broker execution")
    if "actual snapshot PostgreSQL role: NOSUPERUSER NOBYPASSRLS; FORCE RLS tables: 2" not in output:
        raise ValueError("missing actual scoped database evidence")


def check_manifest(proof, sha, tree, run, attempt, hashes):
    if set(proof) != {"schema", "source", "tree", "run", "attempt", "versions", "binaries", "streams", "cleanup", "dependencies"}:
        raise ValueError("unexpected manifest shape")
    if proof["schema"] != 1 or type(proof["schema"]) is not int or proof["source"] != sha or proof["tree"] != tree or proof["run"] != run or proof["attempt"] != attempt or proof["versions"] != list(VERSIONS):
        raise ValueError("source/run/attempt/version mismatch")
    if not HEX40.fullmatch(sha) or not HEX40.fullmatch(tree) or not re.fullmatch(r"[1-9][0-9]*",run) or not re.fullmatch(r"[1-9][0-9]*",attempt): raise ValueError("invalid identity")
    if set(proof["binaries"]) != set(BINARIES) or any(not isinstance(v,str) or not HEX64.fullmatch(v) for v in proof["binaries"].values()): raise ValueError("missing binary identity")
    if proof["binaries"]["worker"] != proof["binaries"]["worker-swap"]: raise ValueError("different swap executable")
    if set(proof["dependencies"]) != {"postgres", "redpanda"} or any(not isinstance(v,str) or not re.fullmatch(r"sha256:[0-9a-f]{64}",v) for v in proof["dependencies"].values()): raise ValueError("missing actual dependency image identity")
    if proof["streams"] != hashes or proof["cleanup"] != {"run":run,"attempt":attempt,"postgres_absent":True,"compose_absent":True}:
        raise ValueError("stream/cleanup mismatch")
    if any(type(proof["cleanup"][k]) is not bool for k in ("postgres_absent","compose_absent")): raise ValueError("invalid cleanup verdict")


def check_source_inventory(root):
    found = set()
    for path in (root / "kanz" / PACKAGE).glob("*_test.go"):
        found.update(re.findall(r"^func (Test\w+)\(", path.read_text(), re.MULTILINE))
    if found - {"TestSnapshotOwnerSubprocess"} != TOP_LEVEL: raise ValueError("source test inventory differs")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--regex", action="store_true")
    parser.add_argument("--seal", action="store_true")
    parser.add_argument("--binary-inventory", action="store_true")
    parser.add_argument("--root", type=Path, default=Path("."))
    parser.add_argument("--proof", type=Path)
    parser.add_argument("--binaries", type=Path)
    parser.add_argument("--sha")
    parser.add_argument("--tree")
    parser.add_argument("--run")
    parser.add_argument("--attempt")
    args = parser.parse_args()
    check_source_inventory(args.root)
    if args.binary_inventory:
        if args.binaries is None or args.proof is None: raise ValueError("binary inventory requires owned paths")
        hashes = {name: hashlib.sha256(read_file(args.binaries/name,256<<20)).hexdigest() for name in BINARIES}
        (args.proof/"binaries-before.json").write_text(json.dumps(hashes,sort_keys=True)+"\n")
        return
    if args.regex:
        print("^(" + "|".join(sorted(TOP_LEVEL)) + ")$")
        return
    if not all((args.proof,args.sha,args.tree,args.run,args.attempt)): raise ValueError("incomplete exact source identity")
    hashes = {}
    for version in VERSIONS:
        raw = read_file(args.proof / (version + ".jsonl"), 64 << 20)
        check_stream(raw, version)
        hashes[version + ".jsonl"] = hashlib.sha256(raw).hexdigest()
    cleanup = json.loads(read_file(args.proof / "cleanup.json", 4096), object_pairs_hook=strict)
    dependencies = {name:read_file(args.proof/(name+"-image.txt"),128).decode("ascii").strip() for name in ("postgres","redpanda")}
    before = json.loads(read_file(args.proof/"binaries-before.json",4096),object_pairs_hook=strict)
    if args.seal:
        if args.binaries is None: raise ValueError("tested binaries required")
        actual = subprocess.check_output(["git","-C",str(args.root),"rev-parse","HEAD","HEAD^{tree}"], timeout=10, text=True).splitlines()
        if actual != [args.sha,args.tree]: raise ValueError("checkout source differs")
        binary_hashes = {name: hashlib.sha256(read_file(args.binaries/name, 256<<20)).hexdigest() for name in BINARIES}
        if binary_hashes != before: raise ValueError("tested binary changed")
        proof = {"schema":1,"source":args.sha,"tree":args.tree,"run":args.run,"attempt":args.attempt,"versions":list(VERSIONS),"binaries":binary_hashes,"streams":hashes,"cleanup":cleanup,"dependencies":dependencies}
        check_manifest(proof,args.sha,args.tree,args.run,args.attempt,hashes)
        (args.proof/"proof.json").write_text(json.dumps(proof,sort_keys=True)+"\n")
    else:
        proof = json.loads(read_file(args.proof/"proof.json",64<<10),object_pairs_hook=strict)
        check_manifest(proof,args.sha,args.tree,args.run,args.attempt,hashes)
        if cleanup != proof["cleanup"] or dependencies != proof["dependencies"] or before != proof["binaries"]: raise ValueError("retained receipt differs")
    print("Finite snapshot owner: exact inventories, both broker versions and cleanup verified")


if __name__ == "__main__":
    main()
