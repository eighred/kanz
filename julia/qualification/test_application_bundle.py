import base64
import datetime as dt
import hashlib
import io
import json
from pathlib import Path
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent / 'application_bundle'))
import linked_advisory
import linked_ancestry
import pcre2_advisory
import pcre2_cve
import readback
import scan


class ApplicationReadbackTests(unittest.TestCase):
    def test_missing_or_substituted_compiler_library(self):
        selected = readback.DISTRIBUTION | readback.PATCHED | readback.GENERATED
        for removed in ('lib/libjulia.so.1.13.3', 'lib/julia/libgcc_s.so.1', 'lib/julia/sys.so'):
            for replacement in (set(), {'lib/julia/other.so'}):
                with self.subTest(removed=removed, replacement=replacement):
                    names = selected.difference({removed}) | replacement
                    with self.assertRaisesRegex(ValueError, 'missing or unexpected'):
                        readback.source_libraries([], dict.fromkeys(names))

    def test_forbidden_archive_owner_in_application(self):
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode='w') as tar:
            tar.addfile(tarfile.TarInfo('libexec/7z'))
        buf.seek(0)
        with tarfile.open(fileobj=buf) as tar:
            with self.assertRaisesRegex(ValueError, 'unselected native'):
                readback.contents(tar)

    def test_missing_dependency_identity(self):
        required = {'app': {'dynamic': [('NEEDED', 'libc.so.6')]}}
        provided = {'libc': {'dynamic': [('SONAME', 'libc.so.6')]}}
        readback.validate_needed(required, provided)
        with self.assertRaisesRegex(ValueError, 'libc.so.6'):
            readback.validate_needed(required, {})

    def test_library_alias_must_resolve_inside_verified_regular_files(self):
        files = {'lib/current': ['link', 'actual'], 'lib/actual': ['file']}
        self.assertEqual(readback.link_targets(files), {'lib/current': 'lib/actual'})
        for target in ('missing', 'current'):
            files['lib/current'] = ['link', target]
            with self.assertRaises(ValueError):
                readback.link_targets(files)

    def test_publisher_projection_covers_each_file_once(self):
        names = ['lib/julia/' + f for files in readback.JLL_LIBRARIES.values() for f in files]
        self.assertEqual(len(names), 18)
        self.assertEqual(set(names), (readback.DISTRIBUTION - readback.JULIA_CORE) | readback.PATCHED)
        self.assertFalse(set(names) & readback.GENERATED)

    def test_spdx_uses_actual_manifest_and_uuid(self):
        package = {'name': 'ProtoBuf', 'versionInfo': '1.3.0', 'sourceInfo': 'package found in: ' + scan.MANIFEST,
                   'externalRefs': [{'referenceType': 'purl', 'referenceLocator': 'pkg:julia/ProtoBuf@1.3.0?uuid=id'}]}
        expected = {'ProtoBuf': {'uuid': 'id', 'version': '1.3.0'}}
        scan.spdx_packages({'packages': [package]}, expected)
        for change in ({'sourceInfo': 'package found in: other/Manifest.toml'}, {'versionInfo': '1.1.0'}):
            with self.subTest(change=change), self.assertRaises(ValueError):
                scan.spdx_packages({'packages': [{**package, **change}]}, expected)

    def test_linked_native_commit_objects_and_pre_fix_controls(self):
        objects = {}
        def commit(parents, description):
            lines = ['tree ' + 'a' * 40, *['parent ' + h for h in parents], '', description]
            raw = '\n'.join(lines).encode()
            name = hashlib.sha1(b'commit ' + str(len(raw)).encode() + b'\0' + raw).hexdigest()
            objects[name] = base64.b64encode(raw).decode('ascii')
            return name
        old = commit([], 'previous release')
        fix = commit([old], 'publisher fix')
        head = commit([fix], 'selected source')
        unrelated = commit([], 'different root')
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'history.json'
            path.write_text(json.dumps(objects))
            linked_ancestry.verify(path, head, {fix: old})
            for selected, controls in [(old, {fix: head}), (head, {fix: unrelated}), (head, {fix: fix})]:
                with self.subTest(selected=selected, controls=controls), self.assertRaises(ValueError):
                    linked_ancestry.verify(path, selected, controls)
            path.write_text(json.dumps({**objects, fix: objects[head]}))
            with self.assertRaisesRegex(ValueError, 'commit content differs'):
                linked_ancestry.verify(path, head, {fix: old})
            del objects[fix]
            path.write_text(json.dumps(objects))
            with self.assertRaisesRegex(ValueError, 'control absent'):
                linked_ancestry.verify(path, head, {fix: old})

    def test_observed_symbols_and_exact_files_belong_to_the_same_image(self):
        native = {path: {'sha256': sha, 'size': size} for path, (sha, size) in linked_advisory.FILES.items()}
        receipt = {'image': 'selected', 'source': 'producer', 'native': native}
        runtime = {**receipt, 'symbols': linked_advisory.SYMBOLS}
        linked_advisory.validate_image(receipt, runtime)
        for change in ({'image': 'old'}, {'source': 'other'}, {'symbols': {}}):
            with self.subTest(change=change), self.assertRaisesRegex(ValueError, 'runtime symbol proof'):
                linked_advisory.validate_image(receipt, {**runtime, **change})
        for path in native:
            modified = {**native, path: {'sha256': 'other', 'size': native[path]['size']}}
            with self.subTest(path=path), self.assertRaisesRegex(ValueError, 'actual linked native ELF'):
                linked_advisory.validate_image({**receipt, 'native': modified}, runtime)

    def test_linked_nvd_applicability_includes_platform_and_range(self):
        configs = [{'operator': 'AND', 'nodes': [{'criteria': 'fixture', 'versionEndExcluding': '1.48.0'}]}]
        number = 'CVE-2024-24806'
        expected = hashlib.sha256(json.dumps(configs, sort_keys=True, separators=(',', ':')).encode()).hexdigest()
        with patch.dict(linked_advisory.NVD_CONFIGS, {number: expected}):
            linked_advisory.validate_nvd(number, configs)
            for change in ([{**configs[0], 'operator': 'OR'}], [{'operator': 'AND', 'nodes': []}]):
                with self.assertRaisesRegex(ValueError, 'applicability changed'):
                    linked_advisory.validate_nvd(number, change)

    def test_pcre2_vendor_controls_and_drift(self):
        now = dt.datetime(2026, 11, 19, tzinfo=dt.timezone.utc)
        rows = {k: list(v) for k, v in pcre2_advisory.RANGES.items()}
        policy = b'fixture publisher policy'
        checksum = hashlib.sha256(policy).hexdigest()
        def response(projected):
            return json.dumps([{'ghsa_id': k, 'cve_id': v[0], 'withdrawn_at': None,
                                'vulnerabilities': [{'package': {'name': 'PCRE2'}, 'vulnerable_version_range': v[1],
                                                     'patched_versions': v[2]}]} for k, v in projected.items()]).encode()
        def envelope(selected, raw):
            return {'api': pcre2_advisory.API, 'policy': pcre2_advisory.POLICY, 'policySHA256': checksum,
                    'apiDate': now.isoformat(), 'policyDate': now.isoformat(),
                    'apiSHA256': hashlib.sha256(raw).hexdigest(), 'rows': selected}
        raw = response(rows)
        with patch.object(pcre2_advisory, 'POLICY_HASH', checksum):
            self.assertEqual(len(pcre2_advisory.validate(envelope(rows, raw), raw, policy, now)), 7)
            for changed in ({k: v for k, v in rows.items() if k != 'GHSA-r9hj-j2rw-4q3m'},
                            {**rows, 'GHSA-r9hj-j2rw-4q3m': ['CVE-2026-103111', '<=10.49', '10.50']}):
                modified = response(changed)
                with self.assertRaises(ValueError):
                    pcre2_advisory.validate(envelope(changed, modified), modified, policy, now)
                with self.assertRaises(ValueError):
                    pcre2_advisory.validate(envelope(rows, raw), modified, policy, now)
            with self.assertRaises(ValueError):
                pcre2_advisory.validate(envelope(rows, raw), raw, policy, now + dt.timedelta(hours=25))
        with self.assertRaises(ValueError):
            pcre2_advisory.affected(next(iter(pcre2_advisory.RANGES.values())), '10.50')

    def test_additional_cna_record_requires_unchanged_native_range(self):
        key = 'CVE-2026-89161'
        affected = {'vendor': 'PCRE', 'product': 'PCRE2', 'defaultStatus': 'unaffected',
                    'versions': [{'version': '0', 'lessThan': '10.48', 'status': 'affected', 'versionType': 'custom'}]}
        doc = {'cveMetadata': {'cveId': key, 'state': 'PUBLISHED'},
               'containers': {'cna': {'providerMetadata': {'shortName': 'mitre'}, 'affected': [affected]}}}
        pcre2_cve.validate_cna(key, doc)
        affected['versions'][0]['lessThan'] = '10.50'
        with self.assertRaisesRegex(ValueError, 'ranges changed'):
            pcre2_cve.validate_cna(key, doc)
