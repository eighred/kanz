"""Offline trust-boundary tests; no Docker, registry, or cluster calls."""
import copy
import json
import hashlib
import io
import tarfile
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import isolation

IMAGE = 'ghcr.io/eighred/tv-sync@sha256:' + 'a' * 64
MIGRATION_IMAGE = 'ghcr.io/eighred/kanz-migrate@sha256:' + 'c' * 64
SHA = 'b' * 40


class DockerReferenceTests(unittest.TestCase):
    def test_supported_registry_aliases_preserve_repository_and_digest(self):
        digest = '@sha256:' + 'a'*64
        for source, expected in [
            ('postgres', 'docker.io/library/postgres'),
            ('nats', 'docker.io/library/nats'),
            ('library/postgres', 'docker.io/library/postgres'),
            ('docker.io/postgres', 'docker.io/library/postgres'),
            ('docker.io/library/postgres', 'docker.io/library/postgres'),
            ('natsio/nats-box', 'docker.io/natsio/nats-box'),
            ('ghcr.io/eighred/tv-sync', 'ghcr.io/eighred/tv-sync'),
            ('ghcr.io/eighred/kanz-migrate', 'ghcr.io/eighred/kanz-migrate'),
        ]:
            with self.subTest(source=source):
                self.assertEqual(isolation.docker_reference(source+digest), expected+digest)
        local = 'kanz-isolation/postgres:kanz-tv-isolation-0123456789ab'
        self.assertEqual(isolation.docker_reference(local), 'docker.io/'+local)
        self.assertEqual(isolation.docker_reference('docker.io/'+local), 'docker.io/'+local)
        self.assertNotEqual(isolation.docker_reference('other/postgres'+digest),
                            isolation.docker_reference('postgres'+digest))

    def test_lookalikes_paths_and_unsupported_suffixes_are_refused(self):
        digest = '@sha256:' + 'a'*64
        for reference in [
            'evil.docker.io/library/postgres'+digest,
            'docker.io.evil/library/postgres'+digest,
            'evil/ghcr.io/eighred/tv-sync'+digest,
            'https://docker.io/library/postgres'+digest,
            'docker.io:443/library/postgres'+digest,
            'ghcr.io.evil/eighred/tv-sync'+digest,
            'ghcr.io/tv-sync'+digest,
            'docker.io//library/postgres'+digest,
            'docker.io/library/../postgres'+digest,
            'docker.io/library/postgres/extra'+digest,
            'docker.io/library%2fpostgres'+digest,
            'docker.io/library\\postgres'+digest,
            'postgres'+digest+'\n',
            'postgres'+digest.upper(),
            'postgres:latest',
            'postgres:kanz-tv-isolation-0123456789ab',
            'ghcr.io/kanz-isolation/postgres:kanz-tv-isolation-0123456789ab',
            'kanz-isolation/postgres:kanz-tv-isolation-short',
        ]:
            with self.subTest(reference=reference), self.assertRaises(RuntimeError):
                isolation.docker_reference(reference)


class ImageIdentityTests(unittest.TestCase):
    local = 'docker.io/kanz-isolation/postgres:kanz-tv-isolation-0123456789ab'
    source = 'postgres@sha256:' + 'e' * 64

    def archive(self, path, *, modern=False, mutation=None):
        content = b'{"architecture":"amd64", "os":"linux", "config":{}}'
        if mutation == 'architecture':
            content = content.replace(b'amd64', b'arm64')
        if mutation == 'config-list':
            content = b'[]'
        digest = hashlib.sha256(content).hexdigest()
        config_path = 'blobs/sha256/' + digest if modern else digest + '.json'
        entry = {'Config': config_path, 'RepoTags': ['kanz-isolation/postgres:kanz-tv-isolation-0123456789ab']}
        if mutation == 'path':
            entry['Config'] = '../' + config_path
        if mutation == 'missing tag':
            entry['RepoTags'] = ['kanz-isolation/other:kanz-tv-isolation-0123456789ab']
        if mutation == 'repotags-string':
            entry['RepoTags'] = self.local
        if mutation == 'repotags-nonstring':
            entry['RepoTags'] = [42]
        if mutation == 'config-nonstring':
            entry['Config'] = []
        entries = [entry, entry] if mutation == 'ambiguous' else [entry]
        if mutation == 'unrelated-entry':
            entries.append({'RepoTags': ['unrelated:tag'], 'Config': config_path})
        manifest = json.dumps(entries).encode()
        with tarfile.open(path, 'w') as saved:
            def add(name, data, kind=tarfile.REGTYPE):
                info = tarfile.TarInfo(name)
                info.type = kind
                info.size = len(data) if kind == tarfile.REGTYPE else 0
                if kind in (tarfile.SYMTYPE, tarfile.LNKTYPE):
                    info.linkname = 'private'
                saved.addfile(info, io.BytesIO(data) if info.size else None)
            add('manifest.json', manifest)
            if mutation == 'oversize':
                content += b' ' * (16*1024*1024)
            if mutation == 'tamper':
                content = content.replace(b'linux', b'other')
            kind = {'symlink': tarfile.SYMTYPE, 'hardlink': tarfile.LNKTYPE}.get(mutation, tarfile.REGTYPE)
            add(config_path, content, kind)
            if mutation == 'duplicate':
                add(config_path, content)
        return 'sha256:' + digest

    def test_raw_config_hash_handles_both_docker_archive_layouts(self):
        for modern in (False, True):
            with self.subTest(modern=modern), tempfile.TemporaryDirectory() as temp:
                path = Path(temp)/'image.tar'
                expected = self.archive(path, modern=modern)
                self.assertEqual(isolation.archive_config_digest(path, self.local), expected)

    def test_unsafe_or_ambiguous_archives_are_refused(self):
        for mutation in ('tamper', 'ambiguous', 'path', 'symlink', 'hardlink', 'duplicate', 'oversize', 'architecture', 'missing tag', 'repotags-string', 'repotags-nonstring', 'config-nonstring', 'config-list', 'unrelated-entry'):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as temp:
                path = Path(temp)/'image.tar'
                self.archive(path, mutation=mutation)
                with self.assertRaises(RuntimeError):
                    isolation.archive_config_digest(path, self.local)

    @patch('isolation.run')
    def test_load_binds_repository_tag_and_config_without_equating_digest_types(self, command):
        for modern in (False, True):
            for failure in (None, 'repository', 'tag', 'config'):
                with self.subTest(modern=modern, failure=failure), tempfile.TemporaryDirectory() as temp:
                    root = Path(temp)
                    config_id = self.archive(root/'expected.tar', modern=modern)
                    docker_id = 'sha256:' + 'a'*64 if modern else config_id
                    identity = {}
                    def execute(*args, **kwargs):
                        if args[:3] == ('docker', 'image', 'inspect'):
                            if args[3] == self.source:
                                refs = ['docker.io/library/' + self.source]
                                if failure == 'repository':
                                    refs = ['docker.io/library/other@sha256:' + 'e'*64]
                                return json.dumps({'id': docker_id, 'repo_digests': refs})
                            return ('sha256:' + 'b'*64) if failure == 'tag' else docker_id
                        if args[:2] == ('docker', 'save'):
                            self.archive(root/'image.tar', modern=modern)
                        if '/bin/crictl' in args:
                            return json.dumps({'status': {'id': ('sha256:'+'c'*64) if failure == 'config' else config_id}})
                        return ''
                    command.side_effect = execute
                    if failure:
                        with self.assertRaises(RuntimeError):
                            isolation.Cluster('fixture').load(self.source, self.local, root, identity)
                        self.assertEqual(identity['docker_reference_id'], docker_id)
                        if failure == 'config':
                            self.assertEqual(identity['archive_config_digest'], config_id)
                            self.assertEqual(identity['imported_cri_config_digest'], 'sha256:'+'c'*64)
                    else:
                        self.assertEqual(isolation.Cluster('fixture').load(self.source, self.local, root, identity), config_id)
                        self.assertEqual(identity['archive_config_digest'], identity['imported_cri_config_digest'])
                    self.assertNotIn('config', identity)
                    self.assertNotIn('env', identity)


class ContainerCLITests(unittest.TestCase):
    @patch('isolation.subprocess.run')
    def test_unlisted_container_tool_is_refused_before_execution(self, command):
        with self.assertRaises(KeyError):
            isolation.Cluster('fixture-node').cli('sh', '-c', 'unexpected')
        command.assert_not_called()

    @patch('isolation.subprocess.run')
    def test_multicall_aliases_preserve_arguments_stdin_and_deadlines(self, command):
        # Model the pinned image's argv[0] dispatch at the subprocess boundary.
        # A "k3s kubectl" invocation leaves kubectl in argv[1] and must fail.
        def dispatch(argv, **kwargs):
            self.assertEqual(argv[:4], ('docker', 'exec', '-i', 'fixture-node'))
            executable, arguments = argv[4], argv[5:]
            expected = {
                '/bin/kubectl': ('apply', '-f', '-'),
                '/bin/ctr': ('images', 'import', '/tmp/probe.tar'),
                '/bin/crictl': ('inspecti', 'sha256:fixture'),
            }
            self.assertIn(executable, expected)
            self.assertEqual(arguments, expected[executable])
            self.assertEqual(kwargs['timeout'], 17)
            self.assertEqual(kwargs['input'], 'manifest' if executable.endswith('kubectl') else None)
            return isolation.subprocess.CompletedProcess(argv, 0, stdout='ok', stderr='')
        command.side_effect = dispatch
        cluster = isolation.Cluster('fixture-node')
        self.assertEqual(cluster.k('apply', '-f', '-', data='manifest', timeout=17), 'ok')
        cluster.cli('ctr', 'images', 'import', '/tmp/probe.tar', timeout=17)
        cluster.cli('crictl', 'inspecti', 'sha256:fixture', timeout=17)
        self.assertEqual(command.call_count, 3)


class InputTests(unittest.TestCase):
    def test_digest_and_reference_validation(self):
        isolation.validate_inputs(IMAGE, 'v2026.11.1', SHA)
        isolation.validate_inputs(MIGRATION_IMAGE, 'v2026.11.1', SHA, 'kanz-migrate')
        with self.assertRaises(ValueError):
            isolation.validate_inputs(IMAGE, 'v1', SHA, 'kanz-migrate')
        for image, tag, sha in [
            ('ghcr.io/eighred/tv-sync:latest', 'v1', SHA),
            (IMAGE.replace('tv-sync', 'other'), 'v1', SHA),
            (IMAGE, 'v1; touch stolen', SHA),
            (IMAGE, '--help', SHA),
            (IMAGE, 'v1', SHA + '\n--help'),
        ]:
            with self.subTest(image=image, tag=tag, sha=sha):
                with self.assertRaises(ValueError):
                    isolation.validate_inputs(image, tag, sha)

    @patch('isolation.run')
    def test_tag_mismatch_stops_before_signature_or_execution(self, run):
        run.return_value = 'c' * 40
        with self.assertRaisesRegex(RuntimeError, 'tag does not resolve'):
            isolation.verify_release(IMAGE, 'v1', SHA)
        self.assertEqual(run.call_count, 1)

    @patch('isolation.run')
    def test_every_signature_and_attestation_binds_sha_and_tag(self, run):
        run.return_value = SHA
        candidate = 'e' * 40
        isolation.verify_release(IMAGE, 'v1', SHA, candidate_sha=candidate)
        isolation.verify_release(MIGRATION_IMAGE, 'v1', SHA, 'kanz-migrate', candidate_sha=candidate)
        ancestry = [c.args[3:] for c in run.call_args_list if 'merge-base' in c.args]
        self.assertEqual(ancestry.count(('merge-base', '--is-ancestor', SHA, candidate)), 2)
        self.assertEqual(ancestry.count(('merge-base', '--is-ancestor', SHA, 'origin/main')), 2)
        calls = [c.args for c in run.call_args_list if c.args[0] == 'cosign']
        self.assertEqual(len(calls), 6)
        for image in (IMAGE, MIGRATION_IMAGE):
            image_calls = [c for c in calls if image in c]
            self.assertEqual([c[1] for c in image_calls], ['verify', 'verify-attestation', 'verify-attestation'])
            self.assertEqual([c[c.index('--type') + 1] for c in image_calls[1:]], ['spdxjson', 'vuln'])
        for call in calls:
            self.assertTrue(IMAGE in call or MIGRATION_IMAGE in call)
            self.assertEqual(call[call.index('--certificate-github-workflow-sha') + 1], SHA)
            self.assertEqual(call[call.index('--certificate-identity') + 1],
                             'https://github.com/eighred/kanz/.github/workflows/release.yml@refs/tags/v1')


class ExportTests(unittest.TestCase):
    @patch('isolation.run')
    def test_exports_only_manifest_blobs_with_literal_git_arguments(self, run):
        content = b'kind: NetworkPolicy\n'
        oid = 'd' * 40
        listing = (f'100644 blob {oid} {len(content)}\tkanz/infra/policy.yaml\0'
                   f'100755 blob {oid} 20\tkanz/infra/bootstrap.sh\0').encode()
        run.side_effect = [SHA, listing, content]
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            sources = isolation.export_manifests(SHA, root)
            self.assertEqual((root/'kanz/infra/policy.yaml').read_bytes(), content)
            self.assertFalse((root/'kanz/infra/bootstrap.sh').exists())
            self.assertEqual(sources[0]['blob'], oid)
            self.assertEqual(len(sources[0]['sha256']), 64)
        calls = [c.args for c in run.call_args_list]
        self.assertEqual(calls[0][3:], ('rev-parse', '--verify', SHA + '^{commit}'))
        self.assertEqual(calls[1][3:], ('ls-tree', '-r', '-l', '-z', SHA, '--', 'kanz/infra'))
        self.assertEqual(calls[2][3:], ('show', oid))

    @patch('isolation.run')
    def test_symlinks_gitlinks_escapes_and_oversize_refused_before_blob_read(self, run):
        oid = 'd' * 40
        for mode, kind, size, path in [
            ('120000', 'blob', '10', 'kanz/infra/linked.yaml'),
            ('160000', 'commit', '-', 'kanz/infra/module'),
            ('100644', 'blob', '1', 'kanz/infra/../escape.yaml'),
            ('100644', 'blob', '1', '/kanz/infra/escape.yaml'),
            ('100644', 'blob', '1', 'kanz/infra/a\\escape.yaml'),
            ('100644', 'blob', str(16*1024*1024+1), 'kanz/infra/huge.yaml'),
        ]:
            with self.subTest(path=path), tempfile.TemporaryDirectory() as temp:
                run.reset_mock()
                run.side_effect = [SHA, f'{mode} {kind} {oid} {size}\t{path}\0'.encode()]
                with self.assertRaises(RuntimeError):
                    isolation.export_manifests(SHA, Path(temp))
                self.assertEqual(run.call_count, 2)

    @patch('isolation.run')
    def test_candidate_argument_injection_never_reaches_git(self, run):
        for sha in ('--help', SHA+'; touch bad', SHA+'\n'):
            with self.assertRaises(ValueError):
                isolation.resolve_candidate(sha)
        run.assert_not_called()


class CandidateTests(unittest.TestCase):
    def setUp(self):
        self.deployment = {'metadata':{'name':'tv-sync','namespace':'kanz-services'}, 'spec': {'replicas':1, 'strategy':{'type':'Recreate'}, 'template': {'spec': {
            'containers': [{'name': 'tv-sync', 'image': IMAGE, 'ports':[{'containerPort':8091},{'containerPort':8113}], 'env': [
                {'name': 'TV_SYNC_LISTEN', 'value': ':8091'},
                {'name': 'TV_SYNC_API_LISTEN', 'value': ':8113'},
            ]}], 'initContainers': [{'name': 'migrate', 'image': MIGRATION_IMAGE}],
        }}}}
        self.service = {'metadata':{'name':'tv-sync','namespace':'kanz-services'}, 'spec': {'ports': [{'port': 8113, 'targetPort': 8113}]}}

    def test_valid_split(self):
        isolation.candidate_listener(self.deployment, self.service, IMAGE, MIGRATION_IMAGE)

    def test_changed_namespace_is_not_silently_rewritten(self):
        self.deployment['metadata']['namespace']='different-estate'
        with self.assertRaisesRegex(RuntimeError,'resource identity'):
            isolation.candidate_listener(self.deployment,self.service,IMAGE,MIGRATION_IMAGE)

    def test_replica_and_rollout_shape_cannot_be_silently_rewritten(self):
        for change in ({'replicas':2}, {'replicas':0}, {'strategy':{'type':'RollingUpdate'}}, {'strategy':{}}):
            with self.subTest(change=change):
                doc=copy.deepcopy(self.deployment)
                doc['spec'].update(change)
                with self.assertRaisesRegex(RuntimeError,'singleton'):
                    isolation.candidate_listener(doc,self.service,IMAGE,MIGRATION_IMAGE)

    def test_different_immutable_migration_digest_is_refused(self):
        self.deployment['spec']['template']['spec']['initContainers'][0]['image'] = (
            'ghcr.io/eighred/kanz-migrate@sha256:' + 'd' * 64)
        with self.assertRaisesRegex(RuntimeError, 'migration image differs'):
            isolation.candidate_listener(self.deployment, self.service, IMAGE, MIGRATION_IMAGE)

    def test_dangerous_candidate_shapes_refused(self):
        mutations = {
            'host port': lambda spec: spec['containers'][0]['ports'][0].update(hostPort=8091),
            'host namespace': lambda spec: spec.update(hostNetwork=True),
            'host mount': lambda spec: spec.update(volumes=[{'name': 'host', 'hostPath': {'path': '/'}}]),
            'sidecar': lambda spec: spec['containers'].append({'name': 'unreviewed'}),
            'extra init': lambda spec: spec['initContainers'].append({'name': 'unreviewed'}),
            'privileged': lambda spec: spec['containers'][0].update(securityContext={'privileged': True}),
            'migration substitution': lambda spec: spec['initContainers'][0].update(image='ghcr.io/eighred/kanz-migrate:latest'),
            'image substitution': lambda spec: spec['containers'][0].update(image='attacker:latest'),
            'blank listener': lambda spec: spec['containers'][0]['env'][1].update(value=''),
        }
        for name, mutate in mutations.items():
            with self.subTest(name=name):
                doc = copy.deepcopy(self.deployment)
                mutate(doc['spec']['template']['spec'])
                with self.assertRaises(RuntimeError):
                    isolation.candidate_listener(doc, self.service, IMAGE, MIGRATION_IMAGE)


class InventoryTests(unittest.TestCase):
    def make_inventory(self, root):
        infra = root / 'infra'
        infra.mkdir()
        policy = {'apiVersion': 'networking.k8s.io/v1', 'kind': 'NetworkPolicy',
                  'metadata': {'name': 'same-overlay-name', 'namespace': 'kanz-services'},
                  'spec': {'podSelector': {}, 'policyTypes': ['Ingress'], 'ingress': []}}
        for i in range(20):
            (infra / f'{i}.yaml').write_text(json.dumps(policy), encoding='utf-8')
        return infra

    def test_additive_collisions_remain_distinct_and_nonpolicies_are_excluded(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            infra = self.make_inventory(root)
            (infra / 'ignored.yaml').write_text('kind: Pod\nmetadata: {name: never-applied}\n', encoding='utf-8')
            original=json.loads((infra/'0.yaml').read_text(encoding='utf-8'))
            (infra/'nested.yml').write_text(json.dumps({'kind':'List','items':[{'kind':'List','items':[original]}]}),encoding='utf-8')
            (infra/'extra.json').write_text(json.dumps(original),encoding='utf-8')
            for suffix in ('YAML', 'YML', 'JSON'):
                (infra/('uppercase.'+suffix)).write_text(json.dumps(original), encoding='utf-8')
            policies, sources = isolation.policy_inventory(root)
            self.assertEqual(len(policies), 25)
            self.assertEqual(len({p['metadata']['name'] for p in policies}), 25)
            self.assertEqual({s['name'] for s in sources}, {'same-overlay-name'})
            self.assertTrue(all(s['spec_sha256'] for s in sources))

    def test_unsupported_relevant_yaml_fails_closed(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            infra = self.make_inventory(root)
            (infra / 'bad.yaml').write_text('kind: NetworkPolicy\nspec: [broken\n', encoding='utf-8')
            with self.assertRaisesRegex(RuntimeError, 'unparseable'):
                isolation.policy_inventory(root)

    def test_symlink_and_escape_refused(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp)
            inside=root/'checkout'
            inside.mkdir()
            outside=root/'private'
            outside.write_text('private',encoding='utf-8')
            with self.assertRaisesRegex(RuntimeError,'escapes'):
                isolation.safe_read(outside,inside)
            link=inside/'linked.yaml'
            try:
                link.symlink_to(outside)
            except OSError:
                self.skipTest('host does not permit symlink creation; Linux CI exercises this arm')
            with self.assertRaises(RuntimeError):
                isolation.safe_read(link,inside)

    def test_empty_inventory_refused(self):
        with tempfile.TemporaryDirectory() as temp:
            with self.assertRaisesRegex(RuntimeError, 'empty or truncated'):
                isolation.policy_inventory(Path(temp))


if __name__ == '__main__':
    unittest.main()
