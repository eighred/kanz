import copy
import datetime as dt
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import openssl_advisory
import source_candidate as candidate
from test_openssl import fixture


class SourceCandidateTests(unittest.TestCase):
    def setUp(self):
        self.now = dt.datetime.now(dt.timezone.utc)
        data = fixture(self.now)
        self.index, self.records = data['index'], data['records']
        self.registry = data['registryCoverage']
        self.fix = 'a' * 40
        self.ident = 'CVE-2026-1000'
        self.name = self.ident.lower() + '.json'
        document = json.loads(self.records[self.name])
        container = document['containers']['cna']
        container['affected'][0]['versions'][0]['lessThan'] = '3.5.10'
        container['references'] = [{'name': '3.5.10 git commit', 'tags': ['patch'],
                                    'url': 'https://github.com/openssl/openssl/commit/' + self.fix}]
        self.records[self.name] = json.dumps(document)
        evidence = {'fix': self.fix, 'cnaSHA256': candidate.digest(self.records[self.name].encode()),
                    'compareURL': 'https://api.github.com/repos/openssl/openssl/compare/' + self.fix + '...' +
                                  candidate.PINS['source'] + '?per_page=1&page=2', 'compareSHA256': 'b' * 64}
        self.producer = {'vendorFixes': {self.ident: evidence}, 'pins': {'fixes': {self.ident: self.fix}}}
        hashes = {key: candidate.digest(raw.encode()) for key, raw in self.records.items()}
        pin = candidate.digest(json.dumps(hashes, sort_keys=True, separators=(',', ':')).encode())
        self.addCleanup(patch.stopall)
        patch.dict(candidate.PINS, record_set_sha256=pin).start()

    def check(self):
        return candidate.source_catalog(self.index, self.records, self.producer, self.now, self.now, self.registry)

    def test_current_source_evidence_preserves_release_refusal(self):
        result = self.check()
        self.assertEqual(set(result['sourceFixes']), {self.ident})
        self.assertIn('CVE-2026-84782', result['officialAffectedControl'])
        with self.assertRaises(ValueError):
            openssl_advisory.version(candidate.PINS['version'])

    def test_unreviewed_record_or_revised_unaffected_record_refused(self):
        self.records[self.name] += ' '
        with self.assertRaisesRegex(ValueError, 'changed CNA catalog'):
            self.check()

    def test_new_affected_record_without_matching_ancestor_refused(self):
        old = 'cve-2026-1001.json'
        self.records[old] = self.records[self.name].replace(self.ident, 'CVE-2026-1001')
        with self.assertRaisesRegex(ValueError, 'complete release baseline'):
            self.check()

    def test_lost_positive_control_refused(self):
        control = 'cve-2026-84782.json'
        del self.records[control]
        self.index = self.index.replace(control, 'cve-2026-3000.json')
        self.records['cve-2026-3000.json'] = self.records['cve-2026-1002.json'].replace('CVE-2026-1002', 'CVE-2026-3000')
        with self.assertRaisesRegex(ValueError, 'affected control'):
            self.check()

    def test_source_ancestry_cannot_be_substituted(self):
        for key in ('fix', 'compareURL', 'compareSHA256', 'cnaSHA256'):
            with self.subTest(key=key):
                bad = copy.deepcopy(self.producer)
                bad['vendorFixes'][self.ident][key] = 'other'
                with self.assertRaisesRegex(ValueError, 'ancestry binding'):
                    candidate.source_catalog(self.index, self.records, bad, self.now, self.now, self.registry)

    def test_stale_future_and_naive_catalog_times_refused(self):
        for delta in (dt.timedelta(hours=2), -dt.timedelta(seconds=1)):
            with self.assertRaisesRegex(ValueError, 'window'):
                candidate.source_catalog(self.index, self.records, self.producer, self.now, self.now + delta, self.registry)
        with self.assertRaisesRegex(ValueError, 'window'):
            candidate.source_catalog(self.index, self.records, self.producer, self.now.replace(tzinfo=None), self.now, self.registry)

    def test_other_producer_bytes_refused_before_zip_parsing(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / 'changed.zip'
            with path.open('wb') as stream:
                stream.truncate(14035645)
            with self.assertRaisesRegex(ValueError, 'artifact digest'):
                candidate.read_producer(path)


class TrustedInputs(unittest.TestCase):
    def test_repacked_tree_and_self_rewritten_pins_refused(self):
        import gzip
        from test_offline_depot import archive, contract, MANIFEST
        with tempfile.TemporaryDirectory() as temp:
            directory = Path(temp)
            raw = archive()
            # Content and Git tree are unchanged while the actual gzip header differs.
            repacked = bytes([*raw[:4], *(int.from_bytes(raw[4:8], 'little') ^ 1).to_bytes(4, 'little')]) + raw[8:]
            self.assertEqual(gzip.decompress(raw), gzip.decompress(repacked))
            pins = json.dumps(contract(raw)).encode()
            (directory / 'offline_packages.json').write_bytes(pins)
            (directory / 'Manifest.toml').write_bytes(MANIFEST)
            (directory / 'Demo.tar.gz').write_bytes(raw)
            candidate.check_package_inputs(directory, MANIFEST, pins)
            (directory / 'Demo.tar.gz').write_bytes(repacked)
            with self.assertRaisesRegex(ValueError, 'repacked'):
                candidate.check_package_inputs(directory, MANIFEST, pins)
            (directory / 'offline_packages.json').write_bytes(json.dumps(contract(repacked)).encode())
            with self.assertRaisesRegex(ValueError, 'committed'):
                candidate.check_package_inputs(directory, MANIFEST, pins)

    def test_raw_compare_requires_both_endpoints_and_merge_base(self):
        with tempfile.TemporaryDirectory() as temp:
            directory = Path(temp)
            head, fix, tree = 'b' * 40, 'a' * 40, 'c' * 40
            base = 'https://api.github.com/repos/openssl/openssl'
            upstream = json.dumps({'sha': head, 'url': base + '/git/commits/' + head, 'tree': {'sha': tree}}).encode()
            (directory / 'upstream.json').write_bytes(upstream)
            item = {'url': base + '/compare/' + fix + '...' + head, 'base_commit': {'sha': fix},
                    'merge_base_commit': {'sha': fix}, 'status': 'ahead', 'ahead_by': 2, 'behind_by': 0, 'total_commits': 2}
            producer = {'pins': {'source': head, 'tree': tree}, 'upstreamCommit': {'sha256': candidate.digest(upstream)}}
            def prepare(changes):
                raw = json.dumps({**item, **changes}).encode()
                (directory / 'CVE-2026-1000.compare.json').write_bytes(raw)
                producer['vendorFixes'] = {'CVE-2026-1000': {'fix': fix, 'compareSHA256': candidate.digest(raw)}}
            prepare({})
            self.assertEqual(candidate.ancestry_evidence(producer, directory)['comparisons'], 1)
            for changes in [{'url': base + '/compare/' + fix + '...' + fix}, {'merge_base_commit': {'sha': head}},
                            {'base_commit': {'sha': head}}, {'behind_by': 1}, {'status': 'diverged'}]:
                with self.subTest(changes=changes):
                    prepare(changes)
                    with self.assertRaisesRegex(ValueError, 'ancestry'):
                        candidate.ancestry_evidence(producer, directory)

    def test_oci_selected_file_must_be_pinned_regular_content(self):
        import io
        import tarfile
        from source_candidate_oci import layer_files
        name = 'opt/julia/lib/julia/libcrypto.so.3'
        raw = b'\x7fELFfixture'
        expected = {name: {'size': len(raw), 'sha256': candidate.digest(raw)}}
        def check(member, contents):
            packed = io.BytesIO()
            with tarfile.open(fileobj=packed, mode='w') as archive:
                archive.addfile(member, io.BytesIO(contents))
            packed.seek(0)
            found = {}
            with tarfile.open(fileobj=packed) as archive:
                layer_files(archive, expected, found)
            return found
        entry = tarfile.TarInfo(name)
        entry.size = len(raw)
        self.assertEqual(check(entry, raw), expected)
        with self.assertRaisesRegex(ValueError, 'ELF bytes'):
            check(entry, b'\x7fELFchanged')
        entry.size = 0
        entry.type = tarfile.SYMTYPE
        entry.linkname = '/other'
        with self.assertRaisesRegex(ValueError, 'ambiguous candidate'):
            check(entry, b'')
        with self.assertRaisesRegex(ValueError, 'whiteout'):
            check(tarfile.TarInfo('opt/julia/lib/.wh.julia'), b'')
