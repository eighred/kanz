"""Refusal controls for the source-bound agent qualification receipt."""
import copy
import hashlib
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest

import check_agent_boundary_evidence as gate

IDENTITY = gate.metadata("a" * 40, "b" * 40, "17", "2")
ACL = hashlib.sha256(b"canonical ACL\n").hexdigest()
ROUTES = {name: "e" * 64 for name in gate.SOURCE_INVENTORY}
REFERENCES = {"runtime": "sha256:" + gate.RUNTIME.rsplit("sha256:", 1)[1],
              "nats": "sha256:" + "e" * 64, "kind_node": "sha256:" + "d" * 64}


def identity():
    return {"schema": 1, "passed": True, **IDENTITY, "sourceACLsha256": ACL,
            "cases": {name: True for name in gate.IDENTITY_CASES},
            "binaries": {"identity_probe": "c" * 64, "copilot_recorder_test": "d" * 64},
            "images": {"fixture": "sha256:" + "f" * 64, **REFERENCES},
            "cleanup": {name: True for name in ("cluster_absent", "owned_containers_absent", "fixture_image_absent", "private_kubeconfig_absent")}}


def builds():
    return {"binaries": {name: hashlib.sha256(name.encode()).hexdigest() for name in gate.BUILD_NAMES},
            "runtime_reference": gate.RUNTIME, "runtime_image_id": "sha256:" + "f" * 64,
            "gateway_overlay_sha256": "d" * 64, "source_inventory": copy.deepcopy(ROUTES)}


def identity_cleanup():
    return {"schema": 1, **IDENTITY, "recorded": True, "cluster": "feed-proof-012345abcdef",
            "cluster_absent": True, "owned_containers_absent": True, "fixture_image_absent": True}


def events(required):
    out = []
    for package, names in required.items():
        full = gate.PREFIX + package
        out.append({"Action": "start", "Package": full})
        for name in sorted(names):
            out.extend([{"Action": "run", "Package": full, "Test": name},
                        {"Action": "pass", "Package": full, "Test": name}])
        out.append({"Action": "pass", "Package": full})
    return out


def raw(rows):
    return ("\n".join(json.dumps(row) for row in rows) + "\n").encode()


def http_events():
    capabilities = {"POST__fixture_" + name: name for name in gate.PROTECTED_CAPABILITIES}
    inventory = {"schema": 1, "source_inventory": ROUTES, "protected": sorted(capabilities), "capabilities": capabilities}
    names = gate.HTTP_TESTS | {"TestRealAgentBoundary/" + group + "/" + name
                               for group in ("GatewayMutationDenials", "GatewayAnonymousDenials") for name in capabilities}
    rows = events({gate.HTTP_PACKAGE: names})
    rows.insert(2, {"Action": "output", "Package": gate.PREFIX + gate.HTTP_PACKAGE, "Test": "TestRealAgentBoundary",
                    "Output": gate.ROUTE_MARKER + json.dumps(inventory) + "\n"})
    return rows


class PureEvidenceTests(unittest.TestCase):
    def test_metadata_is_an_exact_commit_and_positive_run_identity(self):
        for args in (("A" * 40, "b" * 40, "17", "2"), ("a" * 40, "b" * 39, "17", "2"),
                     ("a" * 40, "b" * 40, "0", "2"), ("a" * 40, "b" * 40, "17", 2)):
            with self.subTest(args=args), self.assertRaises(ValueError):
                gate.metadata(*args)

    def test_complete_nonvacuous_identity(self):
        self.assertGreaterEqual(len(gate.IDENTITY_CASES), 30)
        self.assertEqual(gate.checked_identity(identity(), IDENTITY, ACL, REFERENCES), {"required_cases": len(gate.IDENTITY_CASES)})

    def test_each_identity_case_is_mandatory(self):
        for name in gate.IDENTITY_CASES:
            with self.subTest(case=name):
                value = identity()
                del value["cases"][name]
                with self.assertRaises(ValueError):
                    gate.checked_identity(value, IDENTITY, ACL, REFERENCES)

    def test_identity_never_accepts_skips_or_coercion(self):
        for replacement in (False, 1, "pass", "skip", None):
            with self.subTest(replacement=replacement):
                value = identity()
                value["cases"]["capital_publish_denied"] = replacement
                with self.assertRaises(ValueError):
                    gate.checked_identity(value, IDENTITY, ACL, REFERENCES)

    def test_identity_binds_source_tree_run_attempt(self):
        for key in IDENTITY:
            with self.subTest(key=key):
                value = identity()
                value[key] += "1"
                with self.assertRaises(ValueError):
                    gate.checked_identity(value, IDENTITY, ACL, REFERENCES)

    def test_acl_bytes_include_terminal_newline(self):
        value = identity()
        value["sourceACLsha256"] = hashlib.sha256(b"canonical ACL").hexdigest()
        with self.assertRaises(ValueError):
            gate.checked_identity(value, IDENTITY, ACL, REFERENCES)

    def test_owned_cleanup_receipt_cannot_claim_another_run(self):
        gate.checked_identity_cleanup(identity_cleanup(), IDENTITY)
        for field in IDENTITY:
            value = identity_cleanup()
            value[field] += "1"
            with self.assertRaises(ValueError):
                gate.checked_identity_cleanup(value, IDENTITY)

    def test_missing_or_unrecorded_identity_cleanup_is_not_success(self):
        for field in ("recorded", "cluster_absent", "owned_containers_absent", "fixture_image_absent"):
            value = identity_cleanup()
            value[field] = False
            with self.assertRaises(ValueError):
                gate.checked_identity_cleanup(value, IDENTITY)
        value = identity_cleanup()
        value["cluster"] = "arbitrary-existing-cluster"
        with self.assertRaises(ValueError):
            gate.checked_identity_cleanup(value, IDENTITY)

    def test_binary_image_and_cleanup_receipts_cannot_disappear(self):
        for field in ("binaries", "images", "cleanup"):
            for key in identity()[field]:
                with self.subTest(field=field, key=key):
                    value = identity()
                    del value[field][key]
                    with self.assertRaises(ValueError):
                        gate.checked_identity(value, IDENTITY, ACL, REFERENCES)

    def test_unknown_identity_fields_refuse(self):
        value = identity()
        value["skipped"] = True
        with self.assertRaises(ValueError):
            gate.checked_identity(value, IDENTITY, ACL, REFERENCES)

    def test_explicit_schema_and_positive_control_cannot_coerce(self):
        for field, replacement in (("schema", True), ("passed", 1)):
            value = identity()
            value[field] = replacement
            with self.assertRaises(ValueError):
                gate.checked_identity(value, IDENTITY, ACL, REFERENCES)

    def test_json_duplicates_and_nonfinite_values_refuse(self):
        for value in ('{"passed":true,"passed":false}', '{"value":NaN}', '{"value":Infinity}'):
            with self.assertRaises(ValueError):
                gate.decode(value)

    def test_go_stream_complete_package_and_case_accounting(self):
        required = {gate.HTTP_PACKAGE: gate.HTTP_TESTS}
        result = gate.checked_events(raw(events(required)), required)
        self.assertEqual(result["required_passes"], len(gate.HTTP_TESTS))
        self.assertGreaterEqual(result["required_passes"], 18)

    def test_every_named_http_case_must_execute(self):
        required = {gate.HTTP_PACKAGE: gate.HTTP_TESTS}
        for name in gate.HTTP_TESTS:
            with self.subTest(case=name):
                rows = [row for row in events(required) if row.get("Test") != name]
                with self.assertRaises(ValueError):
                    gate.checked_events(raw(rows), required)

    def test_every_named_projection_case_must_execute(self):
        self.assertGreaterEqual(len(gate.PROJECTION_TESTS), 5)
        for package, names in gate.PROJECTION_TESTS.items():
            for name in names:
                with self.subTest(package=package, case=name):
                    rows = [row for row in events(gate.PROJECTION_TESTS)
                            if (row["Package"], row.get("Test")) != (gate.PREFIX + package, name)]
                    with self.assertRaises(ValueError):
                        gate.checked_events(raw(rows), gate.PROJECTION_TESTS)

    def test_skips_failures_cache_duplicates_and_truncation_refuse(self):
        required = {gate.HTTP_PACKAGE: gate.HTTP_TESTS}
        complete = events(required)
        mutations = [complete[:-1], complete + [complete[-1]], complete[1:], []]
        for action in ("skip", "fail"):
            rows = copy.deepcopy(complete)
            rows[2]["Action"] = action
            mutations.append(rows)
        rows = copy.deepcopy(complete)
        rows.insert(1, {"Action": "output", "Package": gate.PREFIX + gate.HTTP_PACKAGE, "Output": "ok (cached)"})
        mutations.append(rows)
        for rows in mutations:
            with self.assertRaises(ValueError):
                gate.checked_events(raw(rows), required)

    def test_unstarted_case_or_unexpected_package_refuses(self):
        required = {gate.HTTP_PACKAGE: gate.HTTP_TESTS}
        rows = events(required)
        del rows[1]
        with self.assertRaises(ValueError):
            gate.checked_events(raw(rows), required)
        rows = events(required)
        rows[0]["Package"] = gate.PREFIX + "foreign"
        with self.assertRaises(ValueError):
            gate.checked_events(raw(rows), required)

    def test_build_receipt_requires_actual_distinct_mutant_and_all_binaries(self):
        gate.checked_builds(builds(), ROUTES)
        for name in gate.BUILD_NAMES:
            value = builds()
            del value["binaries"][name]
            with self.assertRaises(ValueError):
                gate.checked_builds(value, ROUTES)
        value = builds()
        value["binaries"]["mutant-gateway"] = value["binaries"]["gateway"]
        with self.assertRaises(ValueError):
            gate.checked_builds(value, ROUTES)

    def test_unreviewed_runtime_or_source_route_inventory_refuses(self):
        for key, value in (("runtime_reference", "latest"), ("source_inventory", {name: "f" * 64 for name in gate.SOURCE_INVENTORY})):
            candidate = builds()
            candidate[key] = value
            with self.assertRaises(ValueError):
                gate.checked_builds(candidate, ROUTES)

    def test_every_declared_reference_pin_must_match_reviewed_source(self):
        for name in REFERENCES:
            with self.subTest(pin=name):
                value = identity()
                value["images"][name] = "sha256:" + "0" * 64
                with self.assertRaises(ValueError):
                    gate.checked_identity(value, IDENTITY, ACL, REFERENCES)

    def test_source_pins_are_literal_and_immutable_without_module_execution(self):
        agent = "RUNTIME = " + repr(gate.RUNTIME)
        feed = "NODE = 'kindest/node:fixture@" + REFERENCES["kind_node"] + "'\nPINS = {'nats:2.10-alpine': 'nats:fixture@" + REFERENCES["nats"] + "'}"
        self.assertEqual(gate.pinned_references(agent, feed), REFERENCES)
        for changed in (feed.replace("@sha256:", ":", 1), feed + "\nNODE = 'duplicate'"):
            with self.assertRaises(ValueError):
                gate.pinned_references(agent, changed)

    def test_dynamic_http_route_inventory_requires_every_child_in_both_groups(self):
        rows = http_events()
        self.assertEqual(gate.checked_http(raw(rows), ROUTES)["protected_routes"], 6)
        children = sorted({row["Test"] for row in rows if row.get("Test", "").count("/") == 2})
        for name in children:
            with self.subTest(child=name), self.assertRaises(ValueError):
                gate.checked_http(raw([row for row in rows if row.get("Test") != name]), ROUTES)
        groups_only = [row for row in rows if row.get("Test", "").count("/") < 2]
        with self.assertRaises(ValueError):
            gate.checked_http(raw(groups_only), ROUTES)

    def test_route_inventory_missing_stale_or_duplicate_refuses(self):
        rows = http_events()
        with self.assertRaises(ValueError):
            gate.checked_http(raw([row for row in rows if row.get("Action") != "output"]), ROUTES)
        with self.assertRaises(ValueError):
            gate.checked_http(raw(rows + [copy.deepcopy(rows[2])]), ROUTES)
        changed = {name: "a" * 64 for name in ROUTES}
        with self.assertRaises(ValueError):
            gate.checked_http(raw(rows), changed)


@unittest.skipUnless(sys.platform == "linux", "protected no-follow/nonblocking filesystem checks require Linux")
class ProtectedReceiptTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        (self.root / "identity").mkdir()
        (self.root / "identity/identity.json").write_text(json.dumps(identity()))
        (self.root / "builds.json").write_text(json.dumps(builds()))
        (self.root / "http-cleanup.json").write_text(json.dumps({"schema": 1, "passed": True, **IDENTITY, "owned_containers_absent": True}))
        (self.root / "identity/cleanup.json").write_text(json.dumps(identity_cleanup()))
        (self.root / "http.jsonl").write_bytes(raw(http_events()))
        (self.root / "projections.jsonl").write_bytes(raw(events(gate.PROJECTION_TESTS)))

    def test_roundtrip_and_omitted_receipt_refusal(self):
        with self.assertRaises(FileNotFoundError):
            gate.qualify(self.root, IDENTITY, ACL, ROUTES, REFERENCES)
        gate.qualify(self.root, IDENTITY, ACL, ROUTES, REFERENCES, create=True)
        gate.qualify(self.root, IDENTITY, ACL, ROUTES, REFERENCES)
        with self.assertRaises(ValueError):
            gate.qualify(self.root, IDENTITY, ACL, ROUTES, REFERENCES, create=True)

    def test_stale_or_changed_stream_receipt_refuses(self):
        gate.qualify(self.root, IDENTITY, ACL, ROUTES, REFERENCES, create=True)
        value = json.loads((self.root / "proof.json").read_text())
        value["source"] = "c" * 40
        (self.root / "proof.json").write_text(json.dumps(value))
        with self.assertRaises(ValueError):
            gate.qualify(self.root, IDENTITY, ACL, ROUTES, REFERENCES)

    def test_final_receipt_does_not_coerce_boolean_or_integer_types(self):
        gate.qualify(self.root, IDENTITY, ACL, ROUTES, REFERENCES, create=True)
        original = json.loads((self.root / "proof.json").read_text())
        for field, replacement in (("schema", 1.0), ("passed", 1)):
            value = copy.deepcopy(original)
            value[field] = replacement
            (self.root / "proof.json").write_text(json.dumps(value))
            with self.assertRaises(ValueError):
                gate.qualify(self.root, IDENTITY, ACL, ROUTES, REFERENCES)

    def test_symlink_hardlink_fifo_and_bounds_refuse(self):
        leaf = self.root / "leaf"
        leaf.write_bytes(b"12")
        with self.assertRaises(ValueError):
            gate.read_bounded(leaf, 1)
        link = self.root / "link"
        link.symlink_to(leaf)
        with self.assertRaises(OSError):
            gate.read_bounded(link, 10)
        link.unlink()
        os.link(leaf, link)
        with self.assertRaises(ValueError):
            gate.read_bounded(link, 10)
        fifo = self.root / "fifo"
        os.mkfifo(fifo)
        with self.assertRaises(ValueError):
            gate.read_bounded(fifo, 10)
        parent_link = self.root / "parent_link"
        parent_link.symlink_to(self.root / "identity", target_is_directory=True)
        with self.assertRaises(OSError):
            gate.read_bounded(parent_link / "identity.json", 32768)


if __name__ == "__main__":
    unittest.main()
