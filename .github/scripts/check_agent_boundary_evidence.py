"""Exact-source intelligence boundary qualification; no production authority."""
import argparse
import ast
from collections import Counter
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys

PREFIX = "github.com/eighred/kanz/"
HTTP_PACKAGE = "test/agent_boundary"
HTTP_TESTS = {"TestRealAgentBoundary"} | {
    "TestRealAgentBoundary/" + name for name in """
    GatewayMutationDenials GatewayAnonymousDenials GatewayPrivilegedControls GatewayForgedHeaders GatewayUnknownAmend
    MCPToolInventory MCPReadProjection MCPTenantIsolation MCPUnknownOwner MCPUnknownCoverage MCPStaleness
    MCPMutationRefusals RequestBodyBound OptimizationReadOnly OptimizationPortfolioScope OptimizationVerdictRefusal
    GatewayCapabilityMutation GatewayWritesDisabled
    """.split()
}
PROJECTION_TESTS = {
    "services/mcp/internal/server": {
        "TestToolsList_OffersOnlyReads", "TestInitialize_StatesReadOnlyRatherThanImplyingIt",
        "TestToolsCall_CrossTenantIsRefusedAndLeaksNothing", "TestToolsCall_UnknownAndCrossTenantAreIndistinguishable",
        "TestToolsCall_UnresolvedOwnerFailsClosed", "TestToolsCall_EntitledCallerIsServed",
        "TestRPC_AWriteSideMethodIsNotServed", "TestToolsCall_AWithheldMeasureCarriesNoNumber",
    },
    "services/mcp/internal/riskread": {"TestGRPCMCPUnknownQualityFlagWithholdsAndRecovers"},
    "services/copilot/internal/tools": {
        "TestInvoke_CrossTenantDeniedAndLogged", "TestInvoke_PortfolioScopeDenied",
        "TestUnresolvedOwner_EveryToolFailsClosed", "TestRefusal_CrossTenantIsWordForWordTheNotFoundAnswer",
        "TestGRPCCopilotUnknownQualityFlagWithholdsEveryToolAndRecovers",
    },
    "services/copilot/internal/agent": {
        "TestAsk_AuthorizedGroundedAnswer", "TestAsk_CrossTenantProbeYieldsNoData",
        "TestTheRecordCarriesDigestsAndNotText", "TestTheObservationCarriesNoTenantIdentity",
    },
    "services/optimization/internal/server": {
        "TestProposalPortfolioScopeHTTPBoundaries", "TestProposalPortfolioScopeReauthorizesEveryRequest",
        "TestExactReadOnlyProposalCannotEnterLegacyOrders",
    },
}
IDENTITY_CASES = frozenset("""
copilot_svid_issued authz_publish_allowed answer_publish_allowed capital_publish_denied
control_publish_denied jetstream_admin_denied protected_subscription_denied
unregistered_identity_denied wrong_server_identity_denied wrong_server_positive_control grant_widening_detected owned_cleanup
order_submit_denied order_cancel_denied order_amend_denied order_approve_denied
tenant_order_submit_denied tenant_order_cancel_denied tenant_order_amend_denied tenant_order_approve_denied
mandate_policy_denied risk_policy_denied kill_switch_denied jetstream_stream_create_denied
jetstream_stream_delete_denied jetstream_consumer_create_denied order_subscription_denied
risk_subscription_denied mandate_subscription_denied foreign_tenant_subscription_denied
probe_egress_denied probe_egress_positive_control
""".split())
SOURCE_PATHS = frozenset({
    "kanz/test/agent_boundary/**", "tools/agent_identity/**", "tools/feed_identity/**",
    ".github/scripts/check_agent_boundary_evidence.py", ".github/scripts/test_agent_boundary_evidence.py",
    ".github/scripts/test_agent_boundary_workflow.py", ".github/workflows/agent-read-plane.yml",
    ".github/workflows/kanz-ci.yml",
    "kanz/services/api_gateway/**", "kanz/services/mcp/**", "kanz/services/copilot/**",
    "kanz/services/optimization/**", "kanz/services/identity/**", "kanz/internal/agentgate/**",
    "kanz/internal/measureread/**", "kanz/internal/identity/**", "kanz/internal/optimization/**",
    "kanz/pkg/auth/**", "kanz/pkg/authbus/**", "kanz/pkg/bus/**", "kanz/pkg/transport/**",
    "kanz/infra/nats/**", "kanz/infra/security/spire/**", "schemas/**", "kanz/go.mod", "kanz/go.sum",
})
RUNTIME = "ghcr.io/eighred/base/distroless-cc-debian12:nonroot@sha256:9dac0a79194e45a7da0158a9c6da57b217585af0786db3845d1f0ec1a0dd182f"
BUILD_NAMES = frozenset({"gateway", "mutant-gateway", "mcp", "optimization", "http.test"} |
                        {p.replace("/", "_") + ".test" for p in PROJECTION_TESTS})
SOURCE_INVENTORY = ("kanz/services/api_gateway/internal/authz/arch_test.go", "kanz/services/api_gateway/internal/proxy/proxy.go")
ARTIFACT_FILES = ("http.jsonl", "projections.jsonl", "builds.json", "http-cleanup.json", "identity/identity.json", "identity/cleanup.json", "proof.json")
SHA = re.compile(r"[0-9a-f]{64}\Z")
MAX_EVENTS = 16 * 1024 * 1024
ROUTE_MARKER = "AGENT_BOUNDARY_ROUTE_INVENTORY:"
PROTECTED_CAPABILITIES = frozenset({"Trade", "Operate", "Fund", "Approve", "Mandate", "Audit"})


def strict(pairs):
    out = {}
    for key, value in pairs:
        if key in out:
            raise ValueError("duplicate evidence field")
        out[key] = value
    return out


def decode(raw):
    def invalid(_):
        raise ValueError("non-finite evidence number")
    return json.loads(raw, object_pairs_hook=strict, parse_constant=invalid)


def metadata(source, tree, run, attempt):
    if (not all(isinstance(v, str) and re.fullmatch(r"[0-9a-f]{40}", v) for v in (source, tree))
            or not all(isinstance(v, str) and re.fullmatch(r"[1-9][0-9]*", v) for v in (run, attempt))):
        raise ValueError("invalid source/tree/run/attempt")
    return dict(source=source, tree=tree, run=run, attempt=attempt)


def parent_descriptor(path):
    """Walk from a trusted root descriptor; never follow linked evidence."""
    if sys.platform != "linux" or not all(hasattr(os, n) for n in ("O_NOFOLLOW", "O_NONBLOCK", "O_DIRECTORY")):
        raise ValueError("Linux no-follow/nonblocking evidence reader required")
    path = Path(os.path.abspath(path))
    directory = os.open(path.anchor, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        for part in path.parts[1:-1]:
            following = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=directory)
            os.close(directory)
            directory = following
        return directory, path.name
    except BaseException:
        os.close(directory)
        raise


def read_bounded(path, limit):
    directory, leaf = parent_descriptor(path)
    try:
        fd = os.open(leaf, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory)
        with os.fdopen(fd, "rb") as stream:
            before = os.fstat(stream.fileno())
            if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1 or before.st_size > limit:
                raise ValueError("evidence must be a bounded single-link regular file")
            raw = stream.read(limit + 1)
            after = os.fstat(stream.fileno())
            if (len(raw) != before.st_size or len(raw) > limit or
                    (before.st_size, before.st_mtime_ns, before.st_ctime_ns) !=
                    (after.st_size, after.st_mtime_ns, after.st_ctime_ns)):
                raise ValueError("evidence changed during read")
            return raw
    finally:
        os.close(directory)


def write_new(path, raw):
    directory, leaf = parent_descriptor(path)
    try:
        fd = os.open(leaf, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600, dir_fd=directory)
        with os.fdopen(fd, "wb") as stream:
            info = os.fstat(stream.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
                raise ValueError("receipt destination is not a new regular file")
            stream.write(raw)
            stream.flush()
            os.fsync(stream.fileno())
    finally:
        os.close(directory)


def checked_events(raw, required):
    events = [decode(line) for line in raw.decode("utf-8").splitlines() if line]
    if not events:
        raise ValueError("empty Go evidence")
    expected = {PREFIX + p: names for p, names in required.items()}
    starts, ends, active, passes = Counter(), Counter(), set(), Counter()
    for event in events:
        if not isinstance(event, dict) or event.get("Package") not in expected:
            raise ValueError("unexpected Go package")
        package, test, action = event["Package"], event.get("Test"), event.get("Action")
        if action not in {"start", "run", "pause", "cont", "pass", "output", "fail", "skip"}:
            raise ValueError("unknown Go action")
        if action in {"fail", "skip"} or "(cached)" in event.get("Output", ""):
            raise ValueError("failed, skipped or cached agent boundary case")
        if not test and action == "start":
            starts[package] += 1
        if test and action == "run":
            key = (package, test)
            if key in active or passes[key]:
                raise ValueError("duplicate test start")
            active.add(key)
        if test and action == "pass":
            key = (package, test)
            if key not in active:
                raise ValueError("test pass without start")
            active.remove(key)
            passes[key] += 1
        if not test and action == "pass":
            ends[package] += 1
    if active or set(starts) != set(expected) or starts != ends or any(v != 1 for v in starts.values()):
        raise ValueError("incomplete or duplicate Go package/test lifecycle")
    for package, names in expected.items():
        if not names or any(passes[(package, name)] != 1 for name in names):
            raise ValueError("missing required named agent proof: " + package)
    return {"packages": len(expected), "test_passes": sum(passes.values()),
            "required_passes": sum(map(len, required.values()))}


def checked_http(raw, source_inventory):
    events = [decode(line) for line in raw.decode("utf-8").splitlines() if line]
    output = "".join(e.get("Output", "") for e in events if e.get("Action") == "output" and e.get("Test") == "TestRealAgentBoundary")
    lines = [line for line in output.splitlines() if ROUTE_MARKER in line]
    if len(lines) != 1 or not lines[0].startswith(ROUTE_MARKER):
        raise ValueError("missing or duplicate Go AST route inventory")
    inventory = decode(lines[0][len(ROUTE_MARKER):])
    if (not isinstance(inventory, dict) or set(inventory) != {"schema", "source_inventory", "protected", "capabilities"}
            or type(inventory.get("schema")) is not int or inventory["schema"] != 1
            or inventory["source_inventory"] != source_inventory):
        raise ValueError("Go AST route inventory source/schema differs")
    protected, capabilities = inventory["protected"], inventory["capabilities"]
    if (not isinstance(protected, list) or not protected or len(protected) > 1000
            or any(not isinstance(n, str) or not re.fullmatch(r"[A-Za-z0-9_.-]+", n) for n in protected)
            or protected != sorted(set(protected)) or not isinstance(capabilities, dict)
            or set(capabilities) != set(protected) or any(not isinstance(c, str) for c in capabilities.values())
            or set(capabilities.values()) != PROTECTED_CAPABILITIES):
        raise ValueError("Go AST protected route/capability inventory incomplete")
    names = HTTP_TESTS | {"TestRealAgentBoundary/" + group + "/" + name
                          for group in ("GatewayMutationDenials", "GatewayAnonymousDenials") for name in protected}
    result = checked_events(raw, {HTTP_PACKAGE: names})
    actual = Counter(e.get("Test") for e in events if e.get("Action") == "pass" and e.get("Test"))
    if set(actual) != names:
        raise ValueError("unreviewed or missing HTTP route child")
    return {**result, "protected_routes": len(protected)}


def literal_assignment(raw, name):
    source = ast.parse(raw)
    values = [node.value for node in source.body if isinstance(node, ast.Assign) and len(node.targets) == 1
              and isinstance(node.targets[0], ast.Name) and node.targets[0].id == name]
    if len(values) != 1:
        raise ValueError("source pin constant missing or duplicated: " + name)
    return ast.literal_eval(values[0])


def pinned_references(agent_source, feed_source):
    runtime = literal_assignment(agent_source, "RUNTIME")
    node = literal_assignment(feed_source, "NODE")
    pins = literal_assignment(feed_source, "PINS")
    if runtime != RUNTIME or not isinstance(pins, dict) or "nats:2.10-alpine" not in pins:
        raise ValueError("reviewed image source pins changed")
    values = {"runtime": runtime, "nats": pins["nats:2.10-alpine"], "kind_node": node}
    out = {}
    for key, value in values.items():
        if not isinstance(value, str) or not re.fullmatch(r"[^\s]+@sha256:[0-9a-f]{64}", value):
            raise ValueError("nonimmutable source image pin")
        out[key] = "sha256:" + value.rsplit("sha256:", 1)[1]
    return out


def hash_map(value, required, image=False):
    pattern = re.compile(r"sha256:[0-9a-f]{64}\Z") if image else SHA
    if (not isinstance(value, dict) or not required <= set(value)
            or any(not isinstance(k, str) or not isinstance(v, str) or not pattern.fullmatch(v)
                   for k, v in value.items())):
        raise ValueError("missing or invalid binary/image identities")


def checked_identity(value, expected, acl_hash, references):
    if not isinstance(value, dict) or type(value.get("schema")) is not int or value["schema"] != 1 or value.get("passed") is not True:
        raise ValueError("invalid identity qualification")
    if any(value.get(k) != v for k, v in expected.items()):
        raise ValueError("stale identity source/tree/run/attempt")
    if set(value) != {"schema", "passed", "source", "tree", "run", "attempt", "cases", "sourceACLsha256", "binaries", "images", "cleanup"}:
        raise ValueError("unknown identity evidence field")
    cases = value.get("cases")
    if not isinstance(cases, dict) or set(cases) != IDENTITY_CASES or any(v is not True for v in cases.values()):
        raise ValueError("missing, unexpected or unpassed identity case")
    if not SHA.fullmatch(acl_hash) or value.get("sourceACLsha256") != acl_hash:
        raise ValueError("canonical mounted ACL identity differs")
    hash_map(value.get("binaries"), {"identity_probe", "copilot_recorder_test"})
    hash_map(value.get("images"), {"fixture", "runtime", "nats", "kind_node"}, image=True)
    if set(references) != {"runtime", "nats", "kind_node"} or any(value["images"][key] != pin for key, pin in references.items()):
        raise ValueError("declared image reference differs from reviewed source pin")
    cleanup = value.get("cleanup")
    required_cleanup = {"cluster_absent", "owned_containers_absent", "fixture_image_absent", "private_kubeconfig_absent"}
    if not isinstance(cleanup, dict) or set(cleanup) != required_cleanup or any(v is not True for v in cleanup.values()):
        raise ValueError("owned fixture cleanup not proven")
    return {"required_cases": len(cases)}


def checked_builds(value, source_inventory):
    if not isinstance(value, dict) or set(value) != {"binaries", "runtime_reference", "runtime_image_id", "gateway_overlay_sha256", "source_inventory"}:
        raise ValueError("invalid HTTP build receipt")
    hash_map(value["binaries"], BUILD_NAMES)
    if set(value["binaries"]) != BUILD_NAMES or value["runtime_reference"] != RUNTIME:
        raise ValueError("missing HTTP binary or unreviewed runtime")
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", value["runtime_image_id"]) or not SHA.fullmatch(value["gateway_overlay_sha256"]):
        raise ValueError("invalid runtime/mutation identity")
    if value["binaries"]["gateway"] == value["binaries"]["mutant-gateway"]:
        raise ValueError("mutant is identical to baseline")
    hash_map(source_inventory, set(SOURCE_INVENTORY))
    if set(source_inventory) != set(SOURCE_INVENTORY) or value["source_inventory"] != source_inventory:
        raise ValueError("HTTP route inventory differs from source")


def checked_identity_cleanup(value, expected):
    keys = {"schema", "source", "tree", "run", "attempt", "recorded", "cluster",
            "cluster_absent", "owned_containers_absent", "fixture_image_absent"}
    if not isinstance(value, dict) or set(value) != keys or type(value.get("schema")) is not int or value["schema"] != 1:
        raise ValueError("identity cleanup receipt incomplete")
    if any(value[k] != v for k, v in expected.items()):
        raise ValueError("foreign identity cleanup source/run/attempt")
    if not isinstance(value["cluster"], str) or not re.fullmatch(r"feed-proof-[0-9a-f]{12}", value["cluster"]):
        raise ValueError("invalid exact owned fixture name")
    if any(value[k] is not True for k in ("recorded", "cluster_absent", "owned_containers_absent", "fixture_image_absent")):
        raise ValueError("owned identity resource cleanup unproven")
    return {"owned_resources_absent": True}


def qualify(directory, expected, acl_hash, source_inventory, references, create=False):
    streams = {name: read_bounded(directory / name, MAX_EVENTS) for name in ("http.jsonl", "projections.jsonl")}
    raw_identity = read_bounded(directory / "identity" / "identity.json", 256 * 1024)
    raw_builds = read_bounded(directory / "builds.json", 32 * 1024)
    raw_cleanup = read_bounded(directory / "http-cleanup.json", 4096)
    raw_identity_cleanup = read_bounded(directory / "identity/cleanup.json", 4096)
    expected_cleanup = {"schema": 1, "passed": True, **expected, "owned_containers_absent": True}
    if json.dumps(decode(raw_cleanup), sort_keys=True) != json.dumps(expected_cleanup, sort_keys=True):
        raise ValueError("HTTP fixture cleanup/source binding missing")
    result = {
        "http": checked_http(streams["http.jsonl"], source_inventory),
        "projections": checked_events(streams["projections.jsonl"], PROJECTION_TESTS),
        "identity": checked_identity(decode(raw_identity), expected, acl_hash, references),
        "identity_cleanup": checked_identity_cleanup(decode(raw_identity_cleanup), expected),
    }
    checked_builds(decode(raw_builds), source_inventory)
    files = {**streams, "identity/identity.json": raw_identity, "builds.json": raw_builds,
             "http-cleanup.json": raw_cleanup, "identity/cleanup.json": raw_identity_cleanup}
    bound = {"schema": 1, "passed": True, **expected, "sourceACLsha256": acl_hash,
             "files": {name: hashlib.sha256(raw).hexdigest() for name, raw in files.items()}, "result": result}
    path = directory / "proof.json"
    if create:
        if path.exists() or path.is_symlink():
            raise ValueError("proof already exists")
        # Source-checked CI writes a new receipt only after every independent input passed.
        write_new(path, (json.dumps(bound, sort_keys=True) + "\n").encode())
    elif json.dumps(decode(read_bounded(path, 64 * 1024)), sort_keys=True) != json.dumps(bound, sort_keys=True):
        raise ValueError("omitted or stale source-bound proof receipt")
    return result


def mapping(lines, indent):
    """Accept only the explicit block form owned by this workflow, never YAML aliases."""
    out, current = {}, None
    for line in lines:
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        if "\t" in line:
            raise ValueError("workflow tabs refused")
        if line.startswith(" " * (indent + 2)):
            if current is None:
                raise ValueError("workflow body without mapping")
            out[current][1].append(line)
            continue
        match = re.fullmatch(" " * indent + r"([a-z][a-z0-9_-]*):(?: (.*))?", line)
        if not match or match[1] in out:
            raise ValueError("unsupported or duplicate workflow mapping")
        current = match[1]
        out[current] = (match[2] or "", [])
    return out


def workflow_steps(lines):
    groups = []
    for line in lines:
        if line.startswith("      - "):
            groups.append(["        " + line[8:]])
        elif not groups:
            raise ValueError("workflow step missing")
        else:
            groups[-1].append(line)
    return [mapping(group, 8) for group in groups]


def unconditional_scope_policy(raw):
    top = mapping(raw.splitlines(), 0)
    triggers = mapping(top["on"][1], 2)
    if triggers.get("pull_request") != ("", []):
        raise ValueError("core policy must run on every PR")
    scope = mapping(mapping(top["jobs"][1], 2)["scope"][1], 4)
    if set(scope) != {"runs-on", "outputs", "steps"} or scope["runs-on"] != ("ubuntu-latest", []):
        raise ValueError("unconditional Linux policy owner changed")
    steps = workflow_steps(scope["steps"][1])
    selected = [s for s in steps if s.get("name") == ("Verify mandatory agent boundary qualification policy", [])]
    if len(selected) != 1 or selected[0] != {
        "name": ("Verify mandatory agent boundary qualification policy", []),
        "run": ("python3 -B .github/scripts/test_agent_boundary_workflow.py", []),
    }:
        raise ValueError("independent workflow policy invocation disabled or omitted")


def workflow_policy(raw):
    top = mapping(raw.splitlines(), 0)
    if set(top) != {"name", "on", "permissions", "env", "jobs"}:
        raise ValueError("workflow top-level contract changed")
    permissions = mapping(top["permissions"][1], 2)
    if permissions != {"contents": ("read", []), "packages": ("read", [])}:
        raise ValueError("qualification permissions changed")
    if top["env"] != ("", ["  GOFLAGS: -mod=mod", "  AGENT_HTTP_RUNTIME: " + RUNTIME]):
        raise ValueError("unreviewed runtime or inherited environment")
    triggers = mapping(top["on"][1], 2)
    if set(triggers) != {"pull_request", "push", "workflow_dispatch"}:
        raise ValueError("qualification event missing")
    for event in ("pull_request", "push"):
        rule = mapping(triggers[event][1], 4)
        paths = []
        for line in rule.get("paths", ("", []))[1]:
            match = re.fullmatch(r"      - '([^']+)'", line)
            if not match:
                raise ValueError("unsupported source filter")
            paths.append(match[1])
        if set(paths) != SOURCE_PATHS or len(paths) != len(SOURCE_PATHS):
            raise ValueError("agent boundary source trigger omitted")
        if set(rule) != ({"paths", "branches"} if event == "push" else {"paths"}):
            raise ValueError("unexpected event filter")
    if mapping(triggers["push"][1], 4)["branches"] != ("[main]", []):
        raise ValueError("main qualification absent")
    jobs = mapping(top["jobs"][1], 2)
    if set(jobs) != {"agent-boundary"}:
        raise ValueError("agent boundary job missing")
    job = mapping(jobs["agent-boundary"][1], 4)
    if set(job) != {"runs-on", "timeout-minutes", "steps"} or job["runs-on"] != ("ubuntu-latest", []) or job["timeout-minutes"] != ("45", []):
        raise ValueError("mandatory Linux qualification bypassed")
    required = {
        "Verify source-bound evidence refusals": "test_agent_boundary_",
        "Build actual transports and bounded test fixtures": "-overlay",
        "Prepare isolated runtime and bind executable identities": "--network none",
        "Qualify actual isolated HTTP transports": "TestRealAgentBoundary",
        "Reexecute governed tenant projections and revocation": "projections.jsonl",
        "Prove actual SPIRE and NATS agent identity": "tools/agent_identity/proof.py",
        "Seal and independently reread complete qualification": "check_agent_boundary_evidence.py",
    }
    steps = workflow_steps(job["steps"][1])
    for name, token in required.items():
        selected = [step for step in steps if step.get("name") == (name, [])]
        run = "\n".join(selected[0].get("run", ("", []))[1]) if selected else ""
        if len(selected) != 1 or token not in run or any(k in selected[0] for k in ("if", "continue-on-error")):
            raise ValueError("mandatory qualification invocation missing or disabled: " + name)
        if any(re.search(r"(?:^|\s)(?:exit 0|if false|false &&|true \|\|)", line) for line in selected[0].get("run", ("", []))[1]):
            raise ValueError("disabled workflow body")
    final_step = next(s for s in steps if s.get("name") == ("Seal and independently reread complete qualification", []))
    final = "\n".join(final_step["run"][1])
    if final.count("check_agent_boundary_evidence.py") != 2 or final.count("--create") != 1:
        raise ValueError("qualification receipt not independently reread")
    def body(name):
        step = next(s for s in steps if s.get("name") == (name, []))
        if step["run"][0] != "|" or any(not line.startswith("          ") for line in step["run"][1]):
            raise ValueError("qualification run layout changed")
        return "\n".join(line[10:] for line in step["run"][1])
    if body("Prove actual SPIRE and NATS agent identity") != "python3 -B tools/agent_identity/proof.py":
        raise ValueError("identity qualification not unconditionally invoked")
    evidence_args = ' --directory "$AGENT_BOUNDARY_EVIDENCE_DIR"{create} --source "$GITHUB_SHA" --tree "$(git rev-parse HEAD^{tree})" --run "$GITHUB_RUN_ID" --attempt "$GITHUB_RUN_ATTEMPT"'
    expected_final = "set -euo pipefail\n" + "\n".join(
        "python3 -B .github/scripts/check_agent_boundary_evidence.py" + evidence_args.replace("{create}", create)
        for create in (" --create", ""))
    if body("Seal and independently reread complete qualification") != expected_final:
        raise ValueError("source-bound qualification invocation disabled")
    sandbox = "\n".join(next(s for s in steps if s.get("name") == ("Prepare isolated runtime and bind executable identities", []))["run"][1])
    for token in ("timeout --kill-after=10s 7m docker run --rm --network none --read-only",
                  "--cap-drop ALL --security-opt no-new-privileges --user 65532:65532",
                  "--pids-limit 128 --cpus 2 --memory 1536m --memory-swap 1536m",
                  "--ulimit nofile=512:512 --ulimit fsize=67108864:67108864",
                  "--tmpfs /scratch:rw,nosuid,nodev,noexec,size=134217728,mode=1777",
                  "--mount \"type=bind,source=$RUNNER_TEMP/agent-boundary-bin,destination=/proof,readonly\"",
                  "--env HOME=/scratch --env TMPDIR=/scratch --env PATH=/nonexistent"):
        if token not in sandbox:
            raise ValueError("target execution sandbox widened")
    if sandbox.count("--mount") != 1 or sandbox.count("--network") != 1 or any(
            token in sandbox for token in ("--privileged", "--cap-add", "--env-file", "--volume", "--device", "unconfined", "--pid=host", "--ipc=host")):
        raise ValueError("ambient target authority introduced")
    cleanup = [s for s in steps if s.get("name") == ("Prove owned HTTP container cleanup", [])]
    if len(cleanup) != 1 or cleanup[0].get("if") != ("always()", []) or "continue-on-error" in cleanup[0]:
        raise ValueError("owned HTTP cleanup is not mandatory")
    cleanup_body = body("Prove owned HTTP container cleanup")
    if ('remaining=$(docker ps -aq --filter "label=$owner")' not in cleanup_body or 'test -z "$remaining"' not in cleanup_body or
            'owner="io.kanz.agent-boundary.owner=$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT"' not in cleanup_body):
        raise ValueError("owned HTTP absence check omitted")
    identity_cleanup = [s for s in steps if s.get("name") == ("Prove owned identity cleanup after interruption", [])]
    if (len(identity_cleanup) != 1 or identity_cleanup[0].get("if") != ("always()", []) or
            "continue-on-error" in identity_cleanup[0] or
            body("Prove owned identity cleanup after interruption") != "python3 -B tools/agent_identity/proof.py --cleanup-owned"):
        raise ValueError("exact resource-owner interruption cleanup omitted")
    artifacts = [s for s in steps if s.get("name") == ("Preserve source-bound sanitized qualification evidence", [])]
    if len(artifacts) != 1:
        raise ValueError("qualification evidence upload missing")
    inputs = mapping(artifacts[0].get("with", ("", []))[1], 10)
    expected_paths = ["            ${{ runner.temp }}/agent-boundary-evidence/" + name for name in ARTIFACT_FILES]
    if inputs.get("path") != ("|", expected_paths) or inputs.get("if-no-files-found") != ("error", []):
        raise ValueError("evidence upload must retain only the sanitized allowlist")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", required=True, type=Path)
    parser.add_argument("--create", action="store_true")
    for name in ("source", "tree", "run", "attempt"):
        parser.add_argument("--" + name, required=True)
    args = parser.parse_args()
    expected = metadata(args.source, args.tree, args.run, args.attempt)
    head = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
    tree = subprocess.check_output(["git", "rev-parse", "HEAD^{tree}"], text=True).strip()
    if ((head, tree) != (args.source, args.tree) or
            (os.environ.get("GITHUB_RUN_ID"), os.environ.get("GITHUB_RUN_ATTEMPT")) != (args.run, args.attempt)):
        raise ValueError("actual checkout/runner identity differs")
    if subprocess.run(["git", "diff", "--quiet", "HEAD", "--"], check=False).returncode != 0:
        raise ValueError("tracked source changed after checkout")
    root = Path(__file__).resolve().parents[2]
    workflow_policy(read_bounded(root / ".github/workflows/agent-read-plane.yml", 64 * 1024).decode())
    import yaml
    tenancy = yaml.safe_load(read_bounded(root / "kanz/infra/nats/tenancy.yaml", 1024 * 1024))
    acl_hash = hashlib.sha256(tenancy["data"]["tenants.conf"].encode("utf-8")).hexdigest()
    source_inventory = {name: hashlib.sha256(read_bounded(root / name, 1024 * 1024)).hexdigest() for name in SOURCE_INVENTORY}
    references = pinned_references(read_bounded(root / "tools/agent_identity/proof.py", 1024 * 1024).decode(),
                                  read_bounded(root / "tools/feed_identity/proof.py", 1024 * 1024).decode())
    print(json.dumps(qualify(args.directory, expected, acl_hash, source_inventory, references, args.create), sort_keys=True))


if __name__ == "__main__":
    main()
