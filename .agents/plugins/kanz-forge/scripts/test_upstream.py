"""Publisher identities and failure handling, without a live network dependency."""

import io
import json
import unittest
from http.client import IncompleteRead
from unittest.mock import patch
from urllib.error import HTTPError, URLError

import upstream as u

COMMIT = "a" * 40
TAG = "b" * 40
API = "https://api.github.com/repos/publisher/project"


class UpstreamTests(unittest.TestCase):
    def lookup(self, responses, **kwargs):
        self.urls = []

        def fetch(url):
            self.urls.append(url)
            return responses[url], "c" * 64

        return u.observe(fetch=fetch, **kwargs)

    def release(self, **overrides):
        return dict(tag_name="v2.15.0", draft=False, prerelease=False, **overrides)

    def ref(self, ref="refs/tags/v2.15.0", sha=COMMIT, kind="commit"):
        return {"ref": ref, "object": {"sha": sha, "type": kind}}

    def test_go_uses_stable_numeric_version_not_feed_order_or_preview(self):
        result = self.lookup({"https://go.dev/dl/?mode=json": [
            {"version": "go1.9.9", "stable": True},
            {"version": "go1.28rc1", "stable": False},
            {"version": "go1.27.4", "stable": True}]}, provider="go")
        self.assertEqual((result["status"], result["version"]), ("observed", "go1.27.4"))
        self.assertEqual(len(result["sources"]), 1)
        self.assertIn("+00:00", result["checked_at"])

    def test_go_absence_or_schema_change_is_unknown(self):
        for feed in ([], {}, [None], [{"version": "go1.28rc1", "stable": False}]):
            with self.subTest(feed=feed):
                result = self.lookup({"https://go.dev/dl/?mode=json": feed}, provider="go")
                self.assertEqual(result["status"], "unknown")
                self.assertNotIn("version", result)

    def test_release_resolves_lightweight_and_annotated_tags(self):
        for annotated in (False, True):
            with self.subTest(annotated=annotated):
                responses = {API + "/releases/latest": self.release(),
                             API + "/git/ref/tags/v2.15.0": self.ref(sha=TAG if annotated else COMMIT,
                                                                       kind="tag" if annotated else "commit"),
                             API + "/git/tags/" + TAG: {"sha": TAG, "object": {"type": "commit", "sha": COMMIT}}}
                result = self.lookup(responses, provider="github", project="publisher/project")
                self.assertEqual((result["status"], result["revision"]), ("observed", COMMIT))
                self.assertEqual(result["selection"], "publisher_latest_release")
                self.assertTrue(result["source_url"].endswith(COMMIT))

    def test_exact_release_encodes_tag_as_data(self):
        tag = "release/v2"
        result = self.lookup({API + "/releases/tags/release%2Fv2": {"tag_name": tag, "draft": False, "prerelease": False},
                              API + "/git/ref/tags/release%2Fv2": self.ref(ref="refs/tags/" + tag)},
                             provider="github", project="publisher/project", tag=tag)
        self.assertEqual((result["status"], result["selection"]), ("observed", "exact_release"))

    def test_default_branch_is_not_claimed_as_a_release(self):
        result = self.lookup({API: {"full_name": "Publisher/Project", "private": False, "default_branch": "main"},
                              API + "/git/ref/heads/main": self.ref(ref="refs/heads/main")},
                             provider="github", project="publisher/project", default_branch=True)
        self.assertEqual((result["status"], result["branch"], result["revision"]), ("observed", "main", COMMIT))
        self.assertNotIn("version", result)

    def test_wrong_repository_or_private_metadata_refused(self):
        for metadata in ({"full_name": "other/project", "private": False},
                         {"full_name": "publisher/project", "private": True}, {}):
            result = self.lookup({API: metadata}, provider="github", project="publisher/project", default_branch=True)
            self.assertEqual(result["reason"], "repository_identity_mismatch")
            self.assertEqual(len(self.urls), 1)

    def test_invalid_selectors_make_no_request(self):
        for arguments in ({"project": "https://private.test/secret"}, {"project": "owner/../secret"},
                          {"project": "owner/repo?token=secret"}, {"project": "owner/repo", "tag": "bad\ncommand"},
                          {"project": "owner/repo", "tag": "v1", "default_branch": True}):
            result = self.lookup({}, provider="github", **arguments)
            self.assertEqual(result["status"], "unknown")
            self.assertEqual(self.urls, [])

    def test_prerelease_draft_and_mismatched_exact_release_refused(self):
        for change in ({"prerelease": True}, {"draft": True}, {"tag_name": "other"}):
            metadata = self.release()
            metadata.update(change)
            result = self.lookup({API + "/releases/tags/v2.15.0": metadata},
                                 provider="github", project="publisher/project", tag="v2.15.0")
            self.assertEqual(result["status"], "unknown")
            self.assertNotIn("version", result)

    def test_unresolved_ref_does_not_leave_a_usable_partial_version(self):
        for ref in ({}, self.ref(ref="refs/tags/other"), self.ref(sha="bad"), self.ref(kind="tree")):
            result = self.lookup({API + "/releases/latest": self.release(), API + "/git/ref/tags/v2.15.0": ref},
                                 provider="github", project="publisher/project")
            self.assertEqual(result["status"], "unknown")
            self.assertNotIn("version", result)
            self.assertNotIn("revision", result)

    def test_tag_cycles_are_bounded_and_object_identity_checked(self):
        for sha in (TAG, COMMIT):
            responses = {API + "/releases/latest": self.release(),
                         API + "/git/ref/tags/v2.15.0": self.ref(sha=TAG, kind="tag"),
                         API + "/git/tags/" + TAG: {"sha": sha, "object": {"type": "tag", "sha": TAG}}}
            result = self.lookup(responses, provider="github", project="publisher/project")
            self.assertEqual(result["status"], "unknown")
            self.assertLessEqual(len(self.urls), 7)

    def test_network_failure_keeps_the_result_unknown_without_retry(self):
        for reason in ("http_403", "http_404", "http_429", "network_unavailable"):
            with patch.object(u, "public_json", side_effect=u.LookupError(reason)) as fetch:
                result = u.observe("go", fetch=fetch)
                self.assertEqual((result["status"], result["reason"]), ("unknown", reason))
                self.assertEqual(fetch.call_count, 1)

    def test_transport_bounds_and_hashes_response_bytes(self):
        with patch.object(u, "build_opener") as factory:
            response = factory.return_value.open.return_value.__enter__.return_value
            for payload, reason in ((b"x" * (u.MAX_BYTES + 1), "response_too_large"),
                                    (b"not json", "invalid_json")):
                response.read.return_value = payload
                with self.assertRaisesRegex(u.LookupError, reason):
                    u.public_json("https://go.dev/dl/?mode=json")
                response.read.assert_called_with(u.MAX_BYTES + 1)
            response.read.return_value = b"[]"
            self.assertEqual(u.public_json("https://go.dev/dl/?mode=json"),
                             ([], u.hashlib.sha256(b"[]").hexdigest()))
            request = factory.return_value.open.call_args.args[0]
            self.assertNotIn("Authorization", request.headers)
            self.assertEqual(factory.return_value.open.call_args.kwargs["timeout"], u.TIMEOUT)

    def test_transport_errors_are_closed_categories_without_response_text(self):
        failures = [(HTTPError("private", 403, "secret", {}, None), "http_403"),
                    (URLError("secret"), "network_unavailable"),
                    (IncompleteRead(b"secret"), "network_unavailable")]
        for error, reason in failures:
            with patch.object(u, "build_opener") as factory:
                factory.return_value.open.side_effect = error
                with self.assertRaisesRegex(u.LookupError, "^" + reason + "$"):
                    u.public_json("https://go.dev/dl/?mode=json")

    def test_redirects_are_not_followed(self):
        with self.assertRaisesRegex(u.LookupError, "redirect_refused"):
            u.NoRedirect().redirect_request(None, None, 302, "", {}, "http://127.0.0.1/private")

    def test_cli_prints_json_and_returns_failure_for_unknown(self):
        with patch.object(u.sys, "argv", ["upstream.py", "go"]), patch.object(u, "observe", return_value={"status": "unknown"}), \
                patch.object(u.sys, "stdout", new_callable=io.StringIO) as output:
            self.assertEqual(u.main(), 1)
            self.assertEqual(json.loads(output.getvalue()), {"status": "unknown"})


if __name__ == "__main__":
    unittest.main()
