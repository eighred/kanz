import base64
import io
import json
import pathlib
import tempfile
import unittest
import urllib.request

from ecr_release_recovery import RecoveryError, SafeRedirect, copy_verified, digest, validate_entry, image_result, MEDIA


class ArtifactIntegrityTests(unittest.TestCase):
    def test_only_explicit_image_not_found_permits_restore(self):
        self.assertIsNone(image_result({'images': [], 'failures': [{'failureCode': 'ImageNotFound'}]}))
        image = {'imageId': {'imageDigest': digest(b'image')}}
        self.assertEqual(image_result({'images': [image]}), image)
        for bad in ({}, {'images': []}, {'images': [image, image]},
                    {'failures': [{'failureCode': 'AccessDenied'}]},
                    {'images': [image], 'failures': [{'failureCode': 'ImageNotFound'}]}):
            with self.subTest(bad=bad), self.assertRaises(RecoveryError):
                image_result(bad)

    def test_stream_integrity_rejects_truncation_substitution_and_oversize(self):
        expected = {'digest': digest(b'original'), 'size': 8}
        with tempfile.TemporaryDirectory() as directory:
            target = pathlib.Path(directory) / 'blob'
            for bad in (b'orig', b'substitu', b'original-extra'):
                with self.subTest(bad=bad), self.assertRaises(RecoveryError):
                    copy_verified(io.BytesIO(bad), target, expected)
                self.assertEqual(list(pathlib.Path(directory).iterdir()), [])
            copy_verified(io.BytesIO(b'original'), target, expected)
            self.assertEqual(target.read_bytes(), b'original')
            with self.assertRaises(RecoveryError):
                copy_verified(io.BytesIO(b'corrupt!'), target, expected)
            self.assertEqual(target.read_bytes(), b'original')

    def test_source_manifest_is_not_rebuilt_or_reformatted(self):
        d = {'digest': digest(b'config'), 'size': 6}
        raw = json.dumps({'mediaType': MEDIA, 'schemaVersion': 2, 'config': d, 'layers': []}).encode()
        entry = {'repository': 'accounting', 'digest': digest(raw), 'manifest': base64.b64encode(raw).decode(),
                 'labels': {'org.opencontainers.image.revision': 'a' * 40}}
        self.assertEqual(validate_entry(entry)[1], raw)
        entry['manifest'] = base64.b64encode(raw + b'\n').decode()
        with self.assertRaisesRegex(RecoveryError, 'manifest digest mismatch'):
            validate_entry(entry)

    def test_descriptors_cannot_escape_archive(self):
        for value in ('../../secret', 'sha256:../secret', 'sha256:' + 'A' * 64):
            with self.subTest(value=value), tempfile.TemporaryDirectory() as directory:
                with self.assertRaises(RecoveryError):
                    copy_verified(io.BytesIO(b''), pathlib.Path(directory) / 'blob', {'digest': value, 'size': 0})

    def test_registry_auth_never_follows_cross_host_redirect(self):
        redirect = SafeRedirect()
        req = urllib.request.Request('https://ghcr.io/v2/eighred/accounting/blobs/sha256:a',
                                     headers={'Authorization': 'Bearer test-only'})
        same = redirect.redirect_request(req, None, 302, '', {}, 'https://ghcr.io/other')
        self.assertEqual(same.get_header('Authorization'), 'Bearer test-only')
        other = redirect.redirect_request(req, None, 302, '', {}, 'https://storage.example/blob')
        self.assertIsNone(other.get_header('Authorization'))
        with self.assertRaises(RecoveryError):
            redirect.redirect_request(req, None, 302, '', {}, 'http://storage.example/blob')


if __name__ == '__main__':
    unittest.main()
