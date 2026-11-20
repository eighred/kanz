import copy
import unittest
from unittest.mock import patch

from chrono_advisories import CHECKSUM, FIXED_IN_047, FIXED_IN_051, REVIEWED_RUSTSEC, UPSTREAM_GLOBAL
from chrono_advisories import GLOBAL_TYPES, PACKAGE, PUBLISHER, REGISTRY_OWNER, REVIEWED_PUBLISHER, VERSION, evaluate, rows


def advisory(ghsa, ranges):
    return {
        "ghsa_id": ghsa, "state": "published", "withdrawn_at": None,
        "vulnerabilities": [{"package": {"ecosystem": "rust", "name": "sad-chrono"},
                              "vulnerable_version_range": r} for r in ranges],
    }


def fixture():
    rows = [advisory(key, [">= 0.4.46, < 0.4.47"]) for key in FIXED_IN_047]
    rows.extend(advisory(key, [">= 0.4.46, < 0.4.48", ">= 0.5.0, < 0.5.1"]) for key in FIXED_IN_051)
    return {
        "repository": dict(REVIEWED_PUBLISHER), "publisher_global": {k: [] for k in GLOBAL_TYPES},
        "registry": {"name": PACKAGE, "version": VERSION, "checksum": CHECKSUM, "yanked": False,
                     "rust_version": "1.85.0", "publisher": 365806, "owners": [dict(REGISTRY_OWNER)]},
        "rustsec_commit": "a" * 40, "rustsec_chrono": dict(REVIEWED_RUSTSEC),
        "publisher": rows, "upstream_publisher": [],
        "upstream_global": {"malware": [], "unreviewed": [], "reviewed": [{"ghsa_id": key, "updated_at": stamp, "withdrawn_at": "2022-07-21T18:06:06Z", "vulnerabilities": [
            {"package": {"ecosystem": "rust", "name": "chrono"}, "vulnerable_version_range": "< 0.4.20",
             "first_patched_version": "0.4.20"}]} for key, stamp in UPSTREAM_GLOBAL.items()]},
    }


class ChronoAdvisoriesTest(unittest.TestCase):
    def test_publisher_changes_require_review(self):
        for key, changed in [("owner", 7), ("archived", True), ("disabled", True), ("policy", "c" * 40)]:
            snapshot = fixture()
            snapshot["repository"][key] = changed
            with self.subTest(key=key), self.assertRaises(ValueError):
                evaluate(snapshot, VERSION, CHECKSUM)
        snapshot = fixture()
        snapshot["publisher_global"]["reviewed"].append(advisory("GHSA-3333-3333-3333", ["<= 1.0.0"]))
        with self.assertRaises(ValueError):
            evaluate(snapshot, VERSION, CHECKSUM)

    def test_registry_transfer_or_yank_refuses(self):
        for field, value in [("owners", []), ("publisher", 77), ("yanked", True), ("checksum", "a"*64)]:
            snapshot = fixture()
            snapshot["registry"][field] = value
            with self.subTest(field=field), self.assertRaises(ValueError):
                evaluate(snapshot, VERSION, CHECKSUM)

    def test_all_global_classifications_and_both_packages(self):
        for key in ("publisher_global", "upstream_global"):
            for kind in GLOBAL_TYPES:
                snapshot = fixture()
                del snapshot[key][kind]
                with self.subTest(key=key, absent=kind), self.assertRaises(ValueError):
                    evaluate(snapshot, VERSION, CHECKSUM)
                snapshot = fixture()
                snapshot[key][kind].append(advisory("GHSA-3333-3333-3333", ["<= 1.0.0"]) | {"updated_at":"new"})
                with self.subTest(key=key, finding=kind), self.assertRaises(ValueError):
                    evaluate(snapshot, VERSION, CHECKSUM)

    def test_follows_bound_cursor_and_refuses_selector_changes(self):
        first = f"https://api.github.com/{PUBLISHER}?per_page=1"
        later = "https://api.github.com/repositories/1462205045/security-advisories?per_page=1&after=YQ%3D%3D"
        link = lambda url: '<' + url + '>; rel="next"'
        with patch("chrono_advisories.retrieve", side_effect=[(["first"], link(later)), (["later"], "")]) as get:
            self.assertEqual(rows(PUBLISHER, 1), ["first", "later"])
            self.assertEqual([c.args[0] for c in get.call_args_list], [first, later])
        for wrong in [later.replace("api.github.com", "other.invalid"), later.replace("1462205045", "1"),
                      later.replace("per_page=1", "per_page=2"), later + "&type=reviewed"]:
            with patch("chrono_advisories.retrieve", return_value=([], link(wrong))), self.assertRaises(ValueError):
                rows(PUBLISHER, 1)
        with patch("chrono_advisories.retrieve", return_value=([], link(later))), self.assertRaises(ValueError):
            rows(PUBLISHER, 1)

    def test_reviewed_archive_and_unaffected_release(self):
        evaluate(fixture(), VERSION, CHECKSUM)
        for version, checksum in [("0.5.0", CHECKSUM), ("0.5.2", CHECKSUM), (VERSION, "0" * 64)]:
            with self.subTest(version=version, checksum=checksum), self.assertRaises(ValueError):
                evaluate(fixture(), version, checksum)

    def test_missing_publisher_evidence_cannot_clear(self):
        for field, value in [("publisher", []), ("upstream_global", []), ("rustsec_chrono", {})]:
            snapshot = fixture()
            snapshot[field] = value
            with self.subTest(field=field), self.assertRaises(ValueError):
                evaluate(snapshot, VERSION, CHECKSUM)
        original = fixture()
        for index in range(len(original["publisher"])):
            changed = copy.deepcopy(original)
            changed["publisher"].pop(index)
            with self.subTest(index=index), self.assertRaises(ValueError):
                evaluate(changed, VERSION, CHECKSUM)

    def test_new_fork_finding_and_unfamiliar_range_refuse(self):
        for expression in ["<= 0.5.1", ">= 0.5.1, < 0.6.0", "0.5.*", ">= 0.4.0 || < 2.0.0"]:
            snapshot = fixture()
            snapshot["publisher"].append(advisory("GHSA-2222-2222-2222", [expression]))
            with self.subTest(expression=expression), self.assertRaises(ValueError):
                evaluate(snapshot, VERSION, CHECKSUM)

    def test_upstream_changes_force_shared_code_review(self):
        for change in ("new_rustsec", "updated_rustsec", "upstream_disclosure", "updated_global"):
            snapshot = fixture()
            if change == "new_rustsec":
                snapshot["rustsec_chrono"]["RUSTSEC-9999-1234.md"] = "b" * 40
            elif change == "updated_rustsec":
                snapshot["rustsec_chrono"]["RUSTSEC-2020-0159.md"] = "c" * 40
            elif change == "upstream_disclosure":
                snapshot["upstream_publisher"].append(advisory("GHSA-3333-3333-3333", ["<= 1.0.0"]))
            else:
                snapshot["upstream_global"]["reviewed"][0]["updated_at"] = "2026-11-20T12:00:00Z"
            with self.subTest(change=change), self.assertRaises(ValueError):
                evaluate(snapshot, VERSION, CHECKSUM)

    def test_ambiguous_or_withdrawn_records_refuse(self):
        for change in ("duplicate", "empty", "identity", "withdrawn", "wrong_state"):
            snapshot = fixture()
            row = snapshot["publisher"][0]
            if change == "duplicate":
                snapshot["publisher"].append(row)
            elif change == "empty":
                row["vulnerabilities"] = []
            elif change == "identity":
                row["vulnerabilities"][0]["package"]["name"] = "chrono"
            elif change == "withdrawn":
                row["withdrawn_at"] = "2026-11-20T12:00:00Z"
            else:
                row["state"] = "draft"
            with self.subTest(change=change), self.assertRaises(ValueError):
                evaluate(snapshot, VERSION, CHECKSUM)


if __name__ == "__main__":
    unittest.main()
