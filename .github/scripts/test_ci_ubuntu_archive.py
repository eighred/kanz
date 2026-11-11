import unittest
import os
import tempfile
from pathlib import Path
from unittest.mock import patch
import ci_ubuntu_archive as archive

SOURCE = b"""# Keep the reviewed pockets and architecture selection.
Types: deb
URIs: mirror+file:/etc/apt/apt-mirrors.txt
Suites: resolute resolute-updates resolute-backports
Components: main universe restricted multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
Architectures: amd64 i386

Types: deb
URIs: http://azure.archive.ubuntu.com/ubuntu/
Suites: resolute-security
Components: main universe restricted multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
"""


class ArchiveTests(unittest.TestCase):
    def test_only_uri_bytes_change_and_idempotent(self):
        result = archive.rewrite(SOURCE)
        old = [v for v in SOURCE.splitlines(keepends=True) if not v.startswith(b'URIs:')]
        new = [v for v in result.splitlines(keepends=True) if not v.startswith(b'URIs:')]
        self.assertEqual(old, new)
        self.assertEqual(result.count(b'URIs: '+archive.TARGET), 2)
        self.assertEqual(archive.rewrite(result), result)

    def test_output_preserves_approved_https_authority(self):
        approved = {
            b'https://azure.archive.ubuntu.com/ubuntu/',
            b'https://archive.ubuntu.com/ubuntu/',
            b'https://security.ubuntu.com/ubuntu/',
        }
        uris = [line.split(b': ', 1)[1] for line in archive.rewrite(SOURCE).splitlines()
                if line.startswith(b'URIs: ')]
        self.assertEqual(len(uris), 2)
        self.assertTrue(all(uri in approved for uri in uris))

    def test_crlf_and_comments_preserved(self):
        raw = SOURCE.replace(b'\n', b'\r\n')
        self.assertEqual(archive.rewrite(raw).count(b'\r\n'), raw.count(b'\r\n'))
        self.assertTrue(archive.rewrite(raw).startswith(raw.splitlines(keepends=True)[0]))

    def test_all_exact_supported_uris(self):
        for uri in archive.ALLOWED:
            with self.subTest(uri=uri):
                raw = SOURCE.replace(b'mirror+file:/etc/apt/apt-mirrors.txt', uri)
                self.assertEqual(archive.rewrite(raw).count(archive.TARGET), 2)

    def test_lookalikes_extra_uris_options_and_continuations_refuse(self):
        for uri in (b'https://archive.ubuntu.com.evil/ubuntu/', b'https://archiveXubuntuYcom/ubuntu/',
                    b'https://elsewhere/ubuntu/', b'http://archive.ubuntu.com/ubuntu/ extra',
                    b'mirror+file:/tmp/other', b'\n https://archive.ubuntu.com/ubuntu/'):
            with self.subTest(uri=uri), self.assertRaises(ValueError):
                archive.rewrite(SOURCE.replace(b'mirror+file:/etc/apt/apt-mirrors.txt', uri))
        for field in (b'Trusted: yes', b'Allow-Insecure: yes', b'Signed-By: /tmp/key', b'URIs: https://archive.ubuntu.com/ubuntu/'):
            with self.subTest(field=field), self.assertRaises(ValueError):
                archive.rewrite(SOURCE+field+b'\n')

    def test_signature_and_required_fields_fail_closed(self):
        for line in SOURCE.splitlines(keepends=True):
            if line.startswith((b'Types:', b'URIs:', b'Suites:', b'Components:', b'Signed-By:')):
                with self.subTest(line=line), self.assertRaises(ValueError):
                    archive.rewrite(SOURCE.replace(line, b'', 1))
        with self.assertRaises(ValueError):
            archive.rewrite(SOURCE.replace(b'ubuntu-archive-keyring.gpg', b'other.gpg'))

    def test_bounds_empty_malformed_and_nul(self):
        for raw in (b'', b'# no source\n', b'x'*(archive.LIMIT+1), SOURCE+b'\x00', SOURCE+b'bad field\n'):
            with self.subTest(length=len(raw)), self.assertRaises(ValueError):
                archive.rewrite(raw)

    @unittest.skipUnless(hasattr(os, 'O_NOFOLLOW') and hasattr(os, 'O_NONBLOCK'),
                         'Linux no-follow/nonblocking file proof required in CI')
    def test_actual_fixed_file_apply_and_nonregular_refusal(self):
        with tempfile.TemporaryDirectory() as tmp:
            source = Path(tmp) / 'ubuntu.sources'
            source.write_bytes(SOURCE)
            with patch.object(archive, 'SOURCE', source), patch.dict('os.environ',
                    {'GITHUB_ACTIONS':'true', 'RUNNER_OS':'Linux'}, clear=True):
                archive.apply()
                self.assertEqual(source.read_bytes(), archive.rewrite(SOURCE))
                archive.apply()
                self.assertEqual(source.read_bytes(), archive.rewrite(SOURCE))
                source.write_bytes(b'x'*(archive.LIMIT+1))
                with self.assertRaises(ValueError): archive.apply()
            linked = Path(tmp) / 'linked.sources'
            linked.symlink_to(source)
            fifo = Path(tmp) / 'fifo.sources'
            os.mkfifo(fifo)
            for unsupported in (linked, fifo, Path(tmp)):
                with patch.object(archive, 'SOURCE', unsupported), patch.dict('os.environ',
                        {'GITHUB_ACTIONS':'true', 'RUNNER_OS':'Linux'}, clear=True):
                    with self.assertRaises((OSError, ValueError)): archive.apply()

    def test_not_a_local_host_configuration_tool(self):
        with patch.dict('os.environ', {'GITHUB_ACTIONS':'false', 'RUNNER_OS':'Linux'}, clear=True):
            with self.assertRaises(ValueError):
                archive.apply()


if __name__ == '__main__':
    unittest.main()
