import json
import os
import re
import yaml
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import proof


def boundary(runtime, docker, workflow):
    if len(re.findall(r"#\[cfg\(test\)\]\s*mod identity_proof\s*\{",runtime)) != 1:
        raise ValueError('fixture must remain a test-only module')
    if '#[ignore = "requires the disposable real SPIRE/NATS/Cilium cluster"]' not in runtime:
        raise ValueError('ordinary unit tests cannot contact a cluster')
    module=runtime.split('mod identity_proof {',1)[1]
    for required in ('connect(&config, Arc::new(Health::default()))', 'publisher::publish(&js, frames', 'development: false', '"fixture.forbidden".into()', 'state.messages, 4', 'install_crypto_provider();', 'async_nats::ConnectErrorKind::AuthorizationViolation', 'positive TLS control for actual broker identity required'):
        if required not in module:
            raise ValueError('missing actual runtime path or refusal proof')
    if 'cargo test --locked --release --manifest-path kanz-rs/Cargo.toml -p kanz-market-feed --lib --no-run' not in docker:
        raise ValueError('only the test harness may be built')
    wf=yaml.safe_load(workflow)
    if wf['permissions'] != {'contents':'read','packages':'read'}:
        raise ValueError('fixture must not gain publishing credentials')
    steps=wf['jobs']['developmental-identity']['steps']
    proof_step=next(s for s in steps if s.get('name')=='Prove real SPIRE NATS identity and Cilium enforcement')
    if proof_step.get('run') != 'python3 -B tools/feed-identity/proof.py' or proof_step.get('continue-on-error') or proof_step.get('if'):
        raise ValueError('actual fixture proof cannot skip or ignore failure')


class FixtureTests(unittest.TestCase):
    def test_shared_cluster_is_new_private_and_pinned(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {'RUNNER_TEMP': directory}):
            scratch = proof.Path(directory)
            for prefix in ('feed-proof', 'cni-proof', 'redpanda-proof', 'messaging-proof'):
                name = prefix + '-012345abcdef'
                with patch.object(proof, 'command') as run:
                    proof.create_enforcing_cluster(name, scratch)
                    calls = [call.args[0] for call in run.call_args_list]
                    self.assertEqual(len(calls), 4)
                    self.assertEqual(calls[0][:5], ['kind', 'create', 'cluster', '--name', name])
                    self.assertIn(proof.NODE, calls[0])
                    for call in calls:
                        self.assertIn(str(scratch / 'kubeconfig'), call)
                    for call in calls[1:]:
                        self.assertIn('kind-' + name, call)
                    self.assertIn('daemonset/cilium', calls[2])
                    self.assertIn('--for=condition=Ready', calls[3])
                    self.assertTrue(proof.yaml.safe_load((scratch / 'kind.yaml').read_text())['networking']['disableDefaultCNI'])
        for name in ('production', 'feed-proof-abc', 'redpanda-proof-ABCDEF012345',
                     'cni-proof-zzzzzzzzzzzz', 'messaging-proof-ABCDEF012345', '../feed-proof-012345abcdef'):
            with tempfile.TemporaryDirectory() as directory, patch.object(proof, 'command') as run:
                scratch = proof.Path(directory)
                with self.assertRaises(ValueError):
                    proof.create_enforcing_cluster(name, scratch)
                run.assert_not_called()
                self.assertFalse((scratch / 'kind.yaml').exists())

    def test_source_boundary_and_mutations(self):
        values=[(proof.ROOT/path).read_text() for path in (
            'kanz-rs/crates/market-feed/src/runtime.rs','tools/feed-identity/Dockerfile','.github/workflows/feed-identity-proof.yml')]
        boundary(*values)
        for index,old,new in ((0,'#[cfg(test)]\nmod identity_proof','mod identity_proof'),
            (0,'publisher::publish(&js, frames','other::publish(&js, frames'),
            (0,'state.messages, 4','state.messages, 5'),
            (0,'async_nats::ConnectErrorKind::AuthorizationViolation','async_nats::ConnectErrorKind::Tls'),
            (0,'positive TLS control for actual broker identity required','removed control'),
            (1,'cargo test --locked','cargo build --locked'),
            (2,'packages: read','packages: write'),
            (2,'run: python3 -B tools/feed-identity/proof.py','run: python3 -B tools/feed-identity/proof.py || true')):
            with self.subTest(old=old):
                changed=values.copy();changed[index]=changed[index].replace(old,new)
                self.assertNotEqual(changed,values)
                with self.assertRaises(ValueError): boundary(*changed)

    def test_spire_canonical_identity_and_pinned_images(self):
        docs=proof.spire_objects()
        server=next(d for d in docs if d['kind']=='StatefulSet')
        self.assertNotIn('volumeClaimTemplates',server['spec'])
        self.assertIn({'name':'spire-data','emptyDir':{'medium':'Memory'}},server['spec']['template']['spec']['volumes'])
        registration=proof.manifests('kanz/infra/security/spire/registration.yaml')[0]
        self.assertIn('ServiceAccountName',registration['spec']['spiffeIDTemplate'])
        for d in docs:
            proof.pin_images(d)

    def test_broker_keeps_tls_and_dedicated_narrow_identities(self):
        docs=proof.broker_objects()
        configs={d['metadata']['name']:d['data'] for d in docs if d['kind']=='ConfigMap'}
        config=configs['nats-config']['nats.conf']
        self.assertIn('verify: true',config)
        self.assertIn('verify_and_map: true',config)
        self.assertNotIn('cluster {',config)
        tenants=configs['nats-tenants']['tenants.conf']
        self.assertNotIn('market-ingest',tenants)
        self.assertNotIn('unregistered-feed',tenants)
        self.assertNotIn('fixture.forbidden',tenants)
        for identity in ('binance-depth','okx-depth','identity-bootstrap'):
            self.assertIn('spiffe://kanz.internal/ns/kanz-services/sa/'+identity,tenants)
        for name in ('nats','nats-wrong'):
            pod=next(d for d in docs if d['kind']=='StatefulSet' and d['metadata']['name']==name)
            self.assertEqual(pod['spec']['template']['spec']['serviceAccountName'],name)
            self.assertEqual(pod['spec']['replicas'],1)

    def test_unpinned_image_refused(self):
        for image in ('nats:latest','ghcr.io/spiffe/spire-server:1.15.3'):
            with self.assertRaises(ValueError): proof.pin_images({'image':image})

    def test_two_venues_and_no_test_pod_host_or_secret_mounts(self):
        for sa,venue in (('binance-depth','BINANCE'),('okx-depth','OKX')):
            pod=proof.job('test',sa,'publish','10.0.0.1:4222','fixture:sha')['spec']['template']['spec']
            self.assertFalse(pod['automountServiceAccountToken'])
            self.assertNotIn('hostNetwork',pod)
            self.assertEqual(pod['volumes'],[{'name':'spiffe','csi':{'driver':'csi.spiffe.io','readOnly':True}}])
            container=pod['containers'][0]
            env={e['name']:e['value'] for e in container['env']}
            self.assertEqual(env['IDENTITY_PROOF_VENUE'],venue)
            self.assertTrue(container['securityContext']['readOnlyRootFilesystem'])
            self.assertEqual(container['securityContext']['capabilities']['drop'],['ALL'])

    def test_network_allow_is_namespace_and_pod_intersection_exact_port(self):
        self.assertEqual(proof.policy()['spec']['ingress'],[])
        rule=proof.policy(True)['spec']['ingress'][0]
        self.assertEqual(len(rule['from']),1)
        self.assertIn('namespaceSelector',rule['from'][0])
        self.assertIn('podSelector',rule['from'][0])
        self.assertEqual(rule['ports'],[{'protocol':'TCP','port':4222}])
        blocked=proof.job('deny','binance-depth','blocked','10.0.0.1:4222','fixture:sha',False)
        self.assertEqual(blocked['spec']['template']['metadata']['labels']['identity-proof-client'],'false')

    def test_cleanup_failure_still_records_failed_sanitized_evidence(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ,{'RUNNER_TEMP':directory}), \
             patch.object(proof,'command',side_effect=RuntimeError('fixture failure')):
            with self.assertRaises(RuntimeError): proof.main()
            evidence=json.loads((Path(directory)/'feed-identity.json').read_text())
            self.assertFalse(evidence['passed'])
            self.assertFalse(evidence['cleanupPassed'])
            self.assertEqual(evidence['failureStage'],'build fixture')
            self.assertNotIn('stderr',evidence)
            self.assertNotIn('logs',evidence)

    def test_pinning_does_not_change_canonical_source(self):
        before=(proof.ROOT/'kanz/infra/nats/nats.yaml').read_bytes()
        proof.broker_objects()
        self.assertEqual(before,(proof.ROOT/'kanz/infra/nats/nats.yaml').read_bytes())


if __name__=='__main__': unittest.main()
