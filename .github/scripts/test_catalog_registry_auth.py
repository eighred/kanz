"""Source-free Linux controls; dummy file credentials, no Docker or registry."""
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import catalog_registry_auth as auth


IDENTITY = {'source': 'a' * 40, 'run': '123', 'attempt': '1'}
DUMMY = 'SOURCE_FREE_DUMMY_AUTH'


class RegistryProfileTests(unittest.TestCase):
    def create(self, root):
        profile = auth.prepare(root, IDENTITY)
        self.write(profile, {'auths': {'ghcr.io': {'auth': DUMMY}}})
        return profile

    def write(self, profile, value):
        (profile / auth.CONFIG).write_text(json.dumps(value), encoding='utf-8')

    def erased(self, profile):
        self.write(profile, {'auths': {}})
        return 0

    def test_exclusive_source_run_profile_suppresses_native_autodetection(self):
        with tempfile.TemporaryDirectory() as temporary, mock.patch.dict(os.environ, {'DOCKER_AUTH_CONFIG': ''}):
            root = Path(temporary)
            with mock.patch.object(auth.uuid, 'uuid4') as nonce:
                nonce.return_value.hex = 'b' * 32
                profile = auth.prepare(root, IDENTITY)
                before = {p.name: p.read_bytes() for p in profile.iterdir()}
                self.assertEqual(json.loads(before[auth.CONFIG]), {'auths': {'ghcr.io': {}}})
                self.assertEqual(profile.stat().st_mode & 0o777, 0o700)
                self.assertTrue(profile.name.startswith('catalog-registry-' + 'a' * 12 + '-123-1-'))
                auth.verify_backend(root, profile, IDENTITY, authenticated=False)
                with self.assertRaises(FileExistsError):
                    auth.prepare(root, IDENTITY)
                self.assertEqual({p.name: p.read_bytes() for p in profile.iterdir()}, before)

    def test_zero_logout_exit_with_residual_auth_is_failure_even_when_directory_removed(self):
        for residual in ('auth', 'credsStore', 'credHelpers'):
            with self.subTest(residual=residual), tempfile.TemporaryDirectory() as temporary, mock.patch.dict(os.environ, {'DOCKER_AUTH_CONFIG': ''}):
                root = Path(temporary)
                profile = self.create(root)
                auth.verify_backend(root, profile, IDENTITY)
                def failed_erasure(profile):
                    if residual != 'auth':
                        self.write(profile, {'auths': {}, residual: 'dummy-helper' if residual == 'credsStore' else {'ghcr.io': 'dummy-helper'}})
                    return 0
                logout = mock.Mock(side_effect=failed_erasure)
                result = auth.cleanup(root, profile, IDENTITY, logout=logout)
                logout.assert_called_once_with(profile)
                self.assertEqual(result['logout']['exitCode'], 0)
                self.assertFalse(result['credentialsAbsent'])
                self.assertTrue(result['directoryRemoved'])
                self.assertFalse(result['passed'])
                self.assertFalse(profile.exists())
                self.assertNotIn(DUMMY, json.dumps(result))

    def test_only_actual_erasure_and_directory_absence_pass(self):
        with tempfile.TemporaryDirectory() as temporary, mock.patch.dict(os.environ, {'DOCKER_AUTH_CONFIG': ''}):
            root = Path(temporary)
            profile = self.create(root)
            result = auth.cleanup(root, profile, IDENTITY, logout=self.erased)
            self.assertTrue(all(result[key] for key in ('ownershipVerified', 'fileBackend', 'credentialsAbsent', 'directoryRemoved', 'passed')))
            self.assertEqual(result['logout'], {'attempted': True, 'exitCode': 0})
            self.assertFalse(profile.exists())

    def test_external_stores_helpers_and_ambient_auth_refused_without_calling_them(self):
        for field, value in (('credsStore', 'dummy-helper'), ('credHelpers', {'ghcr.io': 'dummy-helper'})):
            with self.subTest(field=field), tempfile.TemporaryDirectory() as temporary, mock.patch.dict(os.environ, {'DOCKER_AUTH_CONFIG': ''}):
                root = Path(temporary)
                profile = self.create(root)
                self.write(profile, {'auths': {'ghcr.io': {'auth': DUMMY}}, field: value})
                with self.assertRaises(ValueError):
                    auth.verify_backend(root, profile, IDENTITY)
                logout = mock.Mock(return_value=0)
                result = auth.cleanup(root, profile, IDENTITY, logout=logout)
                logout.assert_not_called()
                self.assertFalse(result['fileBackend'])
                self.assertFalse(result['credentialsAbsent'])
                self.assertTrue(result['directoryRemoved'])
                self.assertFalse(result['passed'])
        with tempfile.TemporaryDirectory() as temporary, mock.patch.dict(os.environ, {'DOCKER_AUTH_CONFIG': DUMMY}):
            with self.assertRaises(ValueError):
                auth.prepare(Path(temporary), IDENTITY)
            self.assertEqual(list(Path(temporary).iterdir()), [])

    def test_missing_or_changed_ownership_never_authorizes_cleanup(self):
        for mutation in ('missing', 'source', 'run', 'attempt'):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as temporary, mock.patch.dict(os.environ, {'DOCKER_AUTH_CONFIG': ''}):
                root = Path(temporary)
                profile = self.create(root)
                marker = profile / auth.OWNER
                if mutation == 'missing':
                    marker.unlink()
                else:
                    value = json.loads(marker.read_text())
                    value[mutation] = 'b' * 40 if mutation == 'source' else '999'
                    marker.write_text(json.dumps(value))
                logout = mock.Mock(return_value=0)
                result = auth.cleanup(root, profile, IDENTITY, logout=logout)
                logout.assert_not_called()
                self.assertFalse(result['ownershipVerified'])
                self.assertFalse(result['passed'])
                self.assertTrue(profile.exists())
                self.assertIn(DUMMY, (profile / auth.CONFIG).read_text())

    def test_foreign_path_symlink_and_hardlink_are_preserved(self):
        for mutation in ('foreign-root', 'symlink', 'hardlink'):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as temporary, mock.patch.dict(os.environ, {'DOCKER_AUTH_CONFIG': ''}):
                base = Path(temporary)
                root, foreign = base / 'owned', base / 'foreign'
                root.mkdir()
                foreign.mkdir()
                profile = self.create(foreign if mutation == 'foreign-root' else root)
                outside = foreign / 'untouched'
                outside.write_text(DUMMY)
                if mutation in ('symlink', 'hardlink'):
                    (profile / auth.CONFIG).unlink()
                    if mutation == 'symlink':
                        os.symlink(outside, profile / auth.CONFIG)
                    else:
                        os.chmod(outside, 0o600)
                        os.link(outside, profile / auth.CONFIG)
                logout = mock.Mock(return_value=0)
                result = auth.cleanup(root, profile, IDENTITY, logout=logout)
                logout.assert_not_called()
                self.assertFalse(result['passed'])
                self.assertTrue(profile.exists())
                self.assertEqual(outside.read_text(), DUMMY)

    def test_unknown_entries_prevent_false_directory_removal(self):
        with tempfile.TemporaryDirectory() as temporary, mock.patch.dict(os.environ, {'DOCKER_AUTH_CONFIG': ''}):
            root = Path(temporary)
            profile = self.create(root)
            (profile / 'foreign-directory').mkdir()
            result = auth.cleanup(root, profile, IDENTITY, logout=self.erased)
            self.assertTrue(result['credentialsAbsent'])
            self.assertFalse(result['directoryRemoved'])
            self.assertFalse(result['passed'])
            self.assertTrue((profile / auth.OWNER).is_file())
            self.assertTrue((profile / 'foreign-directory').is_dir())


if __name__ == '__main__':
    unittest.main()
