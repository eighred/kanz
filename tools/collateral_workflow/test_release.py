import base64
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest

import release


class ReleaseEvidence(unittest.TestCase):
    def test_attestation_must_bind_digest_and_predicate(self):
        statement = {'subject': [{'name': 'ghcr.io/eighred/accounting', 'digest': {'sha256': release.IMAGES['accounting']}}],
                     'predicateType': release.PREDICATES['spdxjson'],
                     'predicate': {'spdxVersion': 'SPDX-2.3', 'packages': [{'name': 'accounting'}]}}
        def encoded(value):
            return json.dumps({'payloadType': 'application/vnd.in-toto+json',
                               'payload': base64.b64encode(json.dumps(value).encode()).decode()})
        self.assertEqual(release.attestations(encoded(statement), 'accounting', 'spdxjson'), [1])
        for changed in ({**statement, 'subject': []}, {**statement, 'predicateType': 'different'},
                        {**statement, 'subject': [{'digest': {'sha256': '0' * 64}}]}):
            with self.subTest(changed=changed), self.assertRaises(ValueError):
                release.attestations(encoded(changed), 'accounting', 'spdxjson')
        with self.assertRaises(ValueError):
            release.attestations('', 'accounting', 'spdxjson')

    def test_filesystem_requires_the_expected_binary_and_refuses_jvm_and_extra_elf(self):
        for extra in (None, ('other', b'\x7fELF'), ('app.jar', b'PK'), ('opaque', b'\xca\xfe\xba\xbe')):
            with self.subTest(extra=extra), tempfile.TemporaryDirectory() as folder:
                stream = io.BytesIO()
                with tarfile.open(fileobj=stream, mode='w') as archive:
                    for name, body in [('accounting', b'\x7fELF\x02\x01fixture')] + ([extra] if extra else []):
                        member = tarfile.TarInfo(name)
                        member.size = len(body)
                        member.mode = 0o555
                        archive.addfile(member, io.BytesIO(body))
                stream.seek(0)
                with tarfile.open(fileobj=stream) as archive:
                    if extra:
                        with self.assertRaises(ValueError):
                            release.rootfs(archive, 'accounting', Path(folder) / 'app')
                    else:
                        self.assertEqual(release.rootfs(archive, 'accounting', Path(folder) / 'app')[1], 0)
