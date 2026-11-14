import copy
import unittest

from work_session import assess_session as assess

CURRENT_MAIN = dict(sha="a" * 40, verified_checks=True)


def receipt(key, **changes):
    return dict(id=key, merge_sha="a" * 40, verified_on_main=True,
                validation_node="actual-main", all_acceptance_passed=True,
                evidence="merged-main run and full acceptance", **changes)


def finding(key="repair", **changes):
    item = dict(id=key, parent="original", relation="related", disposition="repair",
                evidence="reviewed owner, full scope and callers at source SHA", tracker="issue/2")
    item.update(changes)
    return item


def fixture():
    return dict(requested_count=1, selection_evidence="initial live issue inventory and acceptance",
                targets=[dict(id="original", acceptance_evidence="issue/1 full acceptance")],
                findings=[], main_acceptance=[])


def graph(*keys):
    nodes = {key: dict(state="READY", execution_ready=True, reasons=[]) for key in keys}
    nodes["actual-main"] = dict(state="DELIVERABLE", execution_ready=True, reasons=[])
    return dict(nodes=nodes)


def source_graph():
    return dict(main_revision="a" * 40, nodes=[dict(
        id="actual-main", kind="gate", subject="main", revision="a" * 40)])


def assess_session(session, validation):
    return assess(session, validation, source_graph(), CURRENT_MAIN)


class WorkSessionTests(unittest.TestCase):
    def test_new_repairs_do_not_replace_the_initial_outcome(self):
        data = fixture()
        data["findings"] = [finding()]
        data["main_acceptance"] = [receipt("repair")]
        result = assess_session(data, graph("repair"))
        self.assertEqual(result["initial_verified_on_main"], [])
        self.assertEqual(result["repairs_verified_on_main"], ["repair"])
        self.assertFalse(result["complete"])

    def test_same_session_repair_requires_its_own_main_acceptance(self):
        data = fixture()
        data["findings"] = [finding()]
        data["main_acceptance"] = [receipt("original")]
        result = assess_session(data, graph("repair"))
        self.assertEqual(result["pending_repairs"], ["repair"])
        self.assertFalse(result["complete"])
        data["main_acceptance"].append(receipt("repair"))
        self.assertTrue(assess_session(data, graph("repair"))["complete"])

    def test_required_deferred_finding_prevents_false_original_closure(self):
        data = fixture()
        data["findings"] = [finding(relation="required", disposition="defer",
                                    reason="explicit runtime hold", reentry="approved runtime input changes")]
        data["main_acceptance"] = [receipt("original")]
        result = assess_session(data, graph())
        self.assertEqual(result["initial_verified_on_main"], [])
        self.assertEqual(result["unresolved_required_findings"], ["repair"])
        self.assertFalse(result["complete"])

        data["main_acceptance"].append(receipt("repair"))
        result = assess_session(data, graph("repair"))
        self.assertIn("repair", result["unverified_receipts"])
        self.assertFalse(result["complete"])

    def test_independent_deferral_does_not_erase_completed_original_work(self):
        data = fixture()
        data["findings"] = [finding(relation="independent", disposition="defer",
                                    reason="different owner and contract", reentry="next scoped work selection")]
        data["main_acceptance"] = [receipt("original")]
        result = assess_session(data, graph())
        self.assertTrue(result["complete"])
        self.assertIn("repair", result["deferred_findings"])

    def test_operator_hold_and_missing_graph_cannot_be_cleared_by_receipt(self):
        data = fixture()
        data["findings"] = [finding()]
        data["main_acceptance"] = [receipt("original"), receipt("repair")]
        for validation in (None, graph(), dict(nodes={"repair": dict(
                state="BLOCKED", execution_ready=True, reasons=["OpenSSL user hold"])})):
            with self.subTest(validation=validation):
                result = assess_session(data, validation)
                self.assertIn("repair", result["blocked_repairs"])
                self.assertEqual(result["repairs_verified_on_main"], [])
                self.assertFalse(result["complete"])

    def test_related_adoption_is_one_wave_but_required_repairs_can_continue(self):
        data = fixture()
        data["findings"] = [finding(), finding("child", parent="repair", relation="related")]
        with self.assertRaisesRegex(ValueError, "direct related repairs"):
            assess_session(data, graph("repair", "child"))
        data["findings"][1]["relation"] = "required"
        data["main_acceptance"] = [receipt("original"), receipt("repair")]
        result = assess_session(data, graph("repair", "child"))
        self.assertEqual(result["initial_verified_on_main"], ["original"])
        self.assertEqual(result["repairs_verified_on_main"], [])
        self.assertFalse(result["complete"])
        data["main_acceptance"].append(receipt("child"))
        self.assertTrue(assess_session(data, graph("repair", "child"))["complete"])

    def test_required_dependency_propagates_through_multiple_repairs(self):
        data = fixture()
        data["findings"] = [finding(relation="required"), finding("child", parent="repair", relation="required")]
        data["main_acceptance"] = [receipt("original"), receipt("repair")]
        result = assess_session(data, graph("repair", "child"))
        self.assertEqual(result["initial_verified_on_main"], [])
        self.assertFalse(result["complete"])

    def test_unfilled_requested_slots_are_not_vacuous_completion(self):
        data = fixture()
        data.update(requested_count=5, targets=[])
        result = assess_session(data, graph())
        self.assertEqual(result["selected_count"], 0)
        self.assertEqual(result["unfilled_count"], 5)
        self.assertFalse(result["complete"])

    def test_pr_or_local_pass_is_not_actual_main_acceptance(self):
        for field in ("verified_on_main", "all_acceptance_passed", "evidence"):
            with self.subTest(field=field):
                data = fixture()
                proof = receipt("original")
                proof[field] = False if field != "evidence" else ""
                data["main_acceptance"] = [proof]
                self.assertFalse(assess_session(data, graph())["complete"])

    def test_rejects_replacement_duplicate_unknown_and_cyclic_identity(self):
        for change in (lambda d: d["findings"].append(finding("original")),
                       lambda d: d["targets"].append(copy.deepcopy(d["targets"][0])),
                       lambda d: d["main_acceptance"].append(receipt("unselected")),
                       lambda d: d["findings"].extend([finding(parent="child"), finding("child", parent="repair")])):
            with self.subTest(change=change):
                data = fixture()
                change(data)
                with self.assertRaises(ValueError):
                    assess_session(data, graph("repair", "child"))

    def test_rejects_unbounded_or_unexplained_dispositions(self):
        for item in (finding(relation="independent"), finding(parent="unknown"),
                     finding(disposition="defer", reason="later", reentry=""),
                     finding(evidence=""), finding(tracker="")):
            with self.subTest(item=item):
                data = fixture()
                data["findings"] = [item]
                with self.assertRaises(ValueError):
                    assess_session(data, graph("repair"))

    def test_count_must_be_positive_integer_and_source_evidence_required(self):
        for count in (0, -1, True, 1.5, "5"):
            with self.subTest(count=count):
                data = fixture()
                data["requested_count"] = count
                with self.assertRaises(ValueError):
                    assess_session(data, graph())
        data = fixture()
        data["selection_evidence"] = ""
        with self.assertRaises(ValueError):
            assess_session(data, graph())

    def test_assessment_does_not_mutate_input_or_validation(self):
        data = fixture()
        data["findings"] = [finding()]
        validation = graph("repair")
        original = copy.deepcopy((data, validation))
        assess_session(data, validation)
        self.assertEqual((data, validation), original)

    def test_current_failed_or_pending_validation_revokes_positive_receipts(self):
        for identity in ("original", "repair"):
            for state in ("BLOCKED", "VALIDATION_FAILED", "VALIDATION_PENDING"):
                with self.subTest(identity=identity, state=state):
                    data = fixture()
                    data["findings"] = [finding()]
                    data["main_acceptance"] = [receipt("original"), receipt("repair")]
                    validation = graph("original", "repair")
                    validation["nodes"][identity].update(
                        state=state, root_blockers=["held-runtime"], reasons=["required validation unresolved"])
                    result = assess_session(data, validation)
                    self.assertFalse(result["complete"])
                    self.assertIn(identity, result["unverified_receipts"])

    def test_main_acceptance_requires_matching_live_main_gate_and_revision(self):
        data = fixture()
        data["main_acceptance"] = [receipt("original")]
        cases = [(None, source_graph()), (dict(nodes={}), source_graph()),
                 (graph(), None), (graph(), dict(main_revision="a" * 40, nodes=[]))]
        for field, value in (("subject", "pr:1"), ("kind", "task"), ("revision", "b" * 40)):
            source = source_graph()
            source["nodes"][0][field] = value
            cases.append((graph(), source))
        wrong_main = source_graph()
        wrong_main["main_revision"] = "b" * 40
        cases.append((graph(), wrong_main))
        for state in ("BLOCKED", "VALIDATION_FAILED", "VALIDATION_PENDING", "READY"):
            validation = graph()
            validation["nodes"]["actual-main"]["state"] = state
            cases.append((validation, source_graph()))
        for validation, source in cases:
            with self.subTest(validation=validation, source=source):
                result = assess(data, validation, source, CURRENT_MAIN)
                self.assertEqual(result["initial_verified_on_main"], [])
                self.assertIn("original", result["unverified_receipts"])
                self.assertFalse(result["complete"])

    def test_required_deferred_receipt_cannot_hide_missing_main_validation(self):
        data = fixture()
        data["findings"] = [finding(relation="required", disposition="defer",
                                    reason="runtime input absent", reentry="approved input supplied")]
        data["main_acceptance"] = [receipt("original"), receipt("repair")]
        result = assess(data, None, None, CURRENT_MAIN)
        self.assertEqual(result["initial_verified_on_main"], [])
        self.assertEqual(result["unresolved_required_findings"], ["repair"])
        self.assertFalse(result["complete"])


if __name__ == "__main__":
    unittest.main()
