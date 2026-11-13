"""Real protocol fixtures, hostile destinations and discovery claim boundaries."""

import socket
import unittest
from unittest.mock import Mock, patch

import git_refs as g
import repository_discovery as d
import upstream as u

A, B, C = "a" * 40, "b" * 40, "c" * 40
TYPE = "application/x-git-upload-pack-advertisement"
URL = "https://forge.example/team/project.git"


def pkt(value):
    return f"{len(value) + 4:04x}".encode() + value


def advertisement(*extra):
    return (pkt(b"# service=git-upload-pack\n") + b"0000"
            + pkt(f"{A} HEAD\0multi_ack symref=HEAD:refs/heads/main\n".encode())
            + pkt(f"{A} refs/heads/main\n".encode())
            + b"".join(pkt(row.encode() + b"\n") for row in extra) + b"0000")


def inventory(*extra):
    return (f"ref: refs/heads/main\tHEAD\n{A}\tHEAD\n" + "".join(row + "\n" for row in extra)).encode()


class GitRefsTests(unittest.TestCase):
    def test_url_boundary(self):
        self.assertEqual(g.repository_url("https://Forge.Example/team/repo.git/"), URL.replace("project", "repo"))
        for url in ("http://forge.example/r", "file:///r", "ssh://forge.example/r", "https://user@forge.example/r",
                    "https://forge.example:444/r", "https://localhost/r", "https://forge.example/r?x=y",
                    "https://forge.example/r#x", "https://forge.example/a/%2e%2e/r", "https://forge.example/a%5cr",
                    "https://forge.example/a%00r", "https://forge.example/a%ffr", "https://forge.\nexample/r"):
            with self.subTest(url=url), self.assertRaises(g.RefError):
                g.repository_url(url)

    def test_all_dns_answers_must_be_public(self):
        for addresses in (("127.0.0.1",), ("10.0.0.1",), ("169.254.169.254",), ("::1",),
                          ("224.0.0.1",), ("8.8.8.8", "192.168.0.1")):
            rows = [(socket.AF_INET, socket.SOCK_STREAM, 0, "", (address, 443)) for address in addresses]
            with self.subTest(addresses=addresses), patch.object(g.socket, "getaddrinfo", return_value=rows):
                with self.assertRaisesRegex(g.RefError, "nonpublic_destination"):
                    g.public_address("forge.example")

    def test_pinned_ip_retains_tls_hostname_verification(self):
        context, raw = Mock(), Mock()
        conn = g.PublicHTTPS("forge.example", context=context, timeout=10)
        conn.address = "8.8.8.8"
        with patch.object(g.socket, "create_connection", return_value=raw) as connect:
            conn.connect()
        connect.assert_called_once_with(("8.8.8.8", 443), timeout=10)
        context.wrap_socket.assert_called_once_with(raw, server_hostname="forge.example")

    def test_http_denials_are_not_followed_or_retried(self):
        for status in (301, 302, 401, 403, 404, 429, 500):
            conn = Mock()
            conn.getresponse.return_value.status = status
            with self.subTest(status=status), patch.object(g, "PublicHTTPS", return_value=conn), patch.object(g, "public_address", return_value="8.8.8.8"):
                with self.assertRaisesRegex(g.RefError, f"http_{status}"):
                    g.request(URL)
                self.assertEqual(conn.request.call_count, 1)
                conn.close.assert_called_once()

    def test_response_size_encoding_and_cleanup(self):
        for encoding, chunks, error in (("gzip", [], "encoded_response_refused"),
                                        ("identity", [b"x" * 65536] * 33, "response_too_large")):
            conn, response = Mock(), Mock()
            response.status = 200
            response.getheader.side_effect = lambda name, default="": encoding if name == "Content-Encoding" else default
            response.read1.side_effect = chunks
            conn.getresponse.return_value = response
            with self.subTest(error=error), patch.object(g, "PublicHTTPS", return_value=conn), patch.object(g, "public_address", return_value="8.8.8.8"):
                with self.assertRaisesRegex(g.RefError, error):
                    g.request(URL)
                conn.close.assert_called_once()

    def test_truncated_http_body_is_unknown(self):
        conn, response = Mock(), Mock()
        response.status, response.length = 200, 20
        response.getheader.side_effect = lambda name, default="": default
        response.read1.side_effect = [b"short", b""]
        conn.getresponse.return_value = response
        with patch.object(g, "PublicHTTPS", return_value=conn), patch.object(g, "public_address", return_value="8.8.8.8"):
            with self.assertRaisesRegex(g.RefError, "incomplete_response"):
                g.request(URL)
            conn.close.assert_called_once()

    def test_response_deadline_closes_connection(self):
        conn, response = Mock(), Mock()
        response.status = 200
        response.getheader.side_effect = lambda name, default="": default
        conn.getresponse.return_value = response
        with patch.object(g, "PublicHTTPS", return_value=conn), patch.object(g, "public_address", return_value="8.8.8.8"), patch.object(g.time, "monotonic", side_effect=[0, 21]):
            with self.assertRaisesRegex(g.RefError, "response_timeout"):
                g.request(URL)
            response.read1.assert_not_called()
            conn.close.assert_called_once()

    def test_smart_advertisement_annotated_tag_and_numeric_order(self):
        data = advertisement(f"{A} refs/tags/v1.9.0", f"{B} refs/tags/v1.10.0", f"{C} refs/tags/v1.10.0^{{}}")
        refs, sources = g.read_refs(URL, lambda url: (data, TYPE))
        result = g.summarize(refs)
        self.assertEqual((result["branch"], result["version"], result["revision"], result["revision_kind"]),
                         ("main", "v1.10.0", C, "git_object"))
        self.assertEqual(len(sources[0]["sha256"]), 64)

    def test_dumb_protocol_reuses_advertisement_and_resolves_head(self):
        calls = []
        def fetch(url):
            calls.append(url)
            return ((f"{A}\trefs/heads/main\n{B}\trefs/tags/2.0\n".encode(), "text/plain")
                    if len(calls) == 1 else (b"ref: refs/heads/main\n", "text/plain"))
        refs, sources = g.read_refs(URL, fetch)
        self.assertEqual(g.summarize(refs)["version"], "2.0")
        self.assertEqual(calls, [URL + "/info/refs?service=git-upload-pack", URL + "/HEAD"])
        self.assertEqual(len(sources), 2)

    def test_complete_inventory_required(self):
        valid = advertisement(f"{B} refs/tags/v2.0")
        for data in (valid[:-1], valid + b"0000", valid.replace(b"# service=git-upload-pack", b"# service=bad-upload-pack"),
                     valid.replace(f"{A} refs/heads/main".encode(), f"{B} refs/heads/main".encode()),
                     advertisement(f"{B} refs/tags/v2.0^{{}}"), advertisement(f"{B} refs/tags/../bad"),
                     advertisement(f"{B} refs/tags/v2.0", f"{C} refs/tags/v2.0"),
                     advertisement(f"{'d' * 64} refs/tags/v2.0")):
            with self.subTest(data=data[-80:]), self.assertRaises(g.RefError):
                g.read_refs(URL, lambda url: (data, TYPE))

    def test_monorepo_families_and_equivalent_tags_remain_ambiguous(self):
        refs = inventory(f"{B}\trefs/tags/client/v2.0", f"{C}\trefs/tags/server/v9.0")
        self.assertEqual(g.summarize(refs)["tag_status"], "ambiguous_tag_family")
        self.assertEqual(g.summarize(refs, "client/v")["version"], "client/v2.0")
        refs = inventory(f"{B}\trefs/tags/v2.0", f"{C}\trefs/tags/v2.0.0")
        self.assertEqual(g.summarize(refs)["tag_status"], "ambiguous_version_tag")

    def test_root_numeric_tags_exclude_prerelease_and_component_families(self):
        refs = inventory(f"{B}\trefs/tags/v2.0", f"{C}\trefs/tags/v3.0-rc1.0", f"{C}\trefs/tags/client/v9.0")
        self.assertEqual(g.summarize(refs)["version"], "v2.0")

    def test_unversioned_head_and_invalid_inventory(self):
        self.assertEqual(g.summarize(inventory())["tag_status"], "no_numeric_tag_observed")
        for data in (b"", inventory()[:-1], inventory(f"{B}\trefs/tags/v2.0^{{}}"), inventory(f"{'0' * 40}\trefs/tags/v2.0")):
            with self.subTest(data=data), self.assertRaises(g.RefError):
                g.summarize(data)


class DiscoveryTests(unittest.TestCase):
    def discover(self, value=URL, **kwargs):
        return d.discover(value, kwargs.pop("observe", Mock()), kwargs.pop("fetch", Mock()), u.LookupError,
                          read=kwargs.pop("read", lambda url: (inventory(f"{B}\trefs/tags/v2.0"), [])), **kwargs)

    def test_github_publisher_release_preferred_without_git_fallback(self):
        read = Mock()
        release = {"status": "observed", "version": "v1.0", "revision": A, "sources": []}
        result = self.discover("publisher/repo", observe=Mock(return_value=release), read=read)
        self.assertEqual(result["version"], "v1.0")
        read.assert_not_called()

    def test_only_public_release_404_allows_fallback(self):
        for reason in ("http_403", "http_429", "network_unavailable", "http_404"):
            observe = Mock(return_value={"status": "unknown", "reason": reason, "sources": []})
            fetch = Mock(return_value=({"full_name": "publisher/repo", "private": False}, "d" * 64))
            read = Mock(return_value=(inventory(), []))
            result = self.discover("publisher/repo", observe=observe, fetch=fetch, read=read)
            self.assertEqual(result["status"], "observed" if reason == "http_404" else "unknown")
            self.assertEqual(read.call_count, int(reason == "http_404"))
            self.assertEqual(fetch.call_count, int(reason == "http_404"))

    def test_private_or_mismatched_identity_refused(self):
        for repo in ({"full_name": "publisher/repo", "private": True}, {"full_name": "other/repo", "private": False},
                     {"full_name": None, "private": False}):
            read = Mock()
            result = self.discover("publisher/repo", observe=Mock(return_value={"status": "unknown", "reason": "http_404", "sources": []}),
                                   fetch=Mock(return_value=(repo, "d" * 64)), read=read)
            self.assertEqual(result["reason"], "repository_identity_mismatch")
            read.assert_not_called()

    def test_generic_host_has_no_language_or_repository_allowlist(self):
        for url in (URL, "https://self.hosted.example/nested/team/repo"):
            observe, fetch = Mock(), Mock()
            result = self.discover(url, observe=observe, fetch=fetch)
            self.assertEqual((result["selection"], result["version"]), ("latest_numeric_tag", "v2.0"))
            observe.assert_not_called()
            fetch.assert_not_called()

    def test_head_ambiguity_and_absent_family_are_distinct(self):
        result = self.discover(read=lambda url: (inventory(), []))
        self.assertEqual((result["selection"], result["revision"]), ("default_branch", A))
        self.assertNotIn("version", result)
        result = self.discover(prefix="missing/v")
        self.assertEqual((result["status"], result["reason"]), ("unknown", "tag_family_not_observed"))
        result = self.discover(read=lambda url: (inventory(f"{B}\trefs/tags/a/v1.0", f"{C}\trefs/tags/b/v2.0"), []))
        self.assertEqual(result["status"], "unknown")
        self.assertNotIn("version", result)

    def test_search_candidates_never_become_official_identity(self):
        fetch = Mock(return_value=({"incomplete_results": False, "total_count": 2, "items": [
            {"full_name": "popular/repo", "private": False}, {"full_name": "publisher/repo", "private": False}]}, "d" * 64))
        result = d.find("repo", fetch, u.LookupError)
        self.assertEqual(result["status"], "candidates")
        self.assertTrue(result["identity_verification_required"])
        self.assertNotIn("repository", result)
        for value in ("", "repo user:attacker", "repo\nsecret", "x" * 101):
            fetch.reset_mock()
            self.assertEqual(d.find(value, fetch, u.LookupError)["status"], "unknown")
            fetch.assert_not_called()

    def test_incomplete_search_or_transport_failure_is_unknown(self):
        for value in ({"incomplete_results": True, "items": [], "total_count": 0}, {}, []):
            self.assertEqual(d.find("repo", Mock(return_value=(value, "d" * 64)), u.LookupError)["status"], "unknown")
        self.assertEqual(d.find("repo", Mock(side_effect=u.LookupError("http_429")), u.LookupError)["reason"], "http_429")


if __name__ == "__main__":
    unittest.main()
