import copy
import datetime as dt
import io
import json
from unittest.mock import patch
import tarfile
import tempfile
from pathlib import Path
import unittest

import advisory as a


class AdvisoryTests(unittest.TestCase):
    def test_actual_scanner_binary_must_match_verified_archive(self):
        with tempfile.TemporaryDirectory() as d:
            executable=Path(d)/'trivy';executable.write_bytes(b'public scanner fixture')
            archive=Path(d)/'trivy.tar.gz'
            with tarfile.open(archive,'w:gz') as package:
                member=tarfile.TarInfo('trivy');member.size=executable.stat().st_size
                package.addfile(member,io.BytesIO(executable.read_bytes()))
            expected=a.digest(archive)
            self.assertEqual(a.tool_identity(executable,expected)['executableSHA256'],a.digest(executable))
            executable.write_bytes(b'substituted binary')
            with self.assertRaises(ValueError):a.tool_identity(executable,expected)
            with self.assertRaises(ValueError):a.tool_identity(executable,'0'*64)

    def expected(self):
        return {'ProtoBuf': {'uuid': a.EXTERNAL_UUIDS['ProtoBuf'], 'version': '1.3.0'}}

    def scan(self):
        return {'Trivy': {'Version': '0.72.0'}, 'Metadata': {'ImageID': 'sha256:' + 'a'*64},
                'Results': [{'Type': 'julia', 'Target': 'opt/kanz-jl/Manifest.toml', 'Packages': [
                    {'Name': n, 'ID': p['uuid'], 'Version': p['version']} for n,p in self.expected().items()]}]}

    def test_image_inventory_requires_uuid_version_source_and_no_advisories(self):
        scan = self.scan(); image = scan['Metadata']['ImageID']
        a.validate_image_scan(scan, image, self.expected())
        mutations = [lambda s: s['Metadata'].update(ImageID='other'),
                     lambda s: s['Trivy'].update(Version='other'),
                     lambda s: s.update(Results=[]),
                     lambda s: s['Results'].append(copy.deepcopy(s['Results'][0])),
                     lambda s: s['Results'][0]['Packages'][0].update(ID='same-name-wrong-uuid'),
                     lambda s: s['Results'][0]['Packages'][0].update(Version='0'),
                     lambda s: s['Results'][0]['Packages'].append(s['Results'][0]['Packages'][0]),
                     lambda s: s['Results'][0].update(ExperimentalModifiedFindings=[{}]),
                     lambda s: s['Results'][0].update(Vulnerabilities=[{'Severity':'LOW'}])]
        for mutate in mutations:
            changed=copy.deepcopy(scan);mutate(changed)
            with self.assertRaises(ValueError): a.validate_image_scan(changed,image,self.expected())

    def test_known_advisory_control_is_not_just_nonzero_exit(self):
        scan=self.scan();r=scan['Results'][0];r['Target']='Manifest.toml'
        r['Packages']=[{'Name':'HTTP','ID':a.HTTP_UUID,'Version':'1.10.15'}]
        r['Vulnerabilities']=[{'PkgName':'HTTP','PkgID':a.HTTP_UUID,'InstalledVersion':'1.10.15','VulnerabilityID':'CVE-2025-52479'}]
        a.validate_control(scan)
        for field,value in [('PkgID','other'),('InstalledVersion','1.10.17'),('VulnerabilityID','unrelated')]:
            changed=copy.deepcopy(scan);changed['Results'][0]['Vulnerabilities'][0][field]=value
            with self.assertRaises(ValueError):a.validate_control(changed)
        r['Vulnerabilities']=[]
        with self.assertRaises(ValueError):a.validate_control(scan)

    def test_spdx_requires_real_julia_uuid_qualified_packages(self):
        expected=self.expected();uuid=expected['ProtoBuf']['uuid']
        package={'name':'ProtoBuf','versionInfo':'1.3.0','sourceInfo':'package found in: opt/kanz-jl/Manifest.toml',
                 'externalRefs':[{'referenceType':'purl','referenceLocator':'pkg:julia/ProtoBuf@1.3.0?uuid='+uuid}]}
        a.validate_spdx({'packages':[package]},expected)
        for purl in ['pkg:generic/ProtoBuf@1.3.0','pkg:julia/ProtoBuf@1.3.0?uuid=wrong',
                     'pkg:julia/ProtoBuf@1.3.0?uuid='+uuid+'&uuid='+uuid,'pkg:julia/ProtoBuf@2?uuid='+uuid]:
            changed=copy.deepcopy(package);changed['externalRefs'][0]['referenceLocator']=purl
            with self.assertRaises(ValueError):a.validate_spdx({'packages':[changed]},expected)
        with self.assertRaises(ValueError):a.validate_spdx({'packages':[package,package]},expected)

    def test_database_is_current_and_identifies_same_snapshot(self):
        now=dt.datetime(2026,11,6,tzinfo=dt.timezone.utc)
        good={'Version':2,'UpdatedAt':(now-dt.timedelta(hours=1)).isoformat(),'NextUpdate':(now+dt.timedelta(hours=1)).isoformat(),'DownloadedAt':now.isoformat()}
        a.validate_database(good,now)
        for key,value in [('Version',1),('UpdatedAt',(now-dt.timedelta(days=2)).isoformat()),
                          ('UpdatedAt',(now+dt.timedelta(hours=1)).isoformat()),
                          ('NextUpdate',(now-dt.timedelta(seconds=1)).isoformat()),
                          ('DownloadedAt',(now+dt.timedelta(hours=1)).isoformat())]:
            changed={**good,key:value}
            with self.assertRaises(ValueError):a.validate_database(changed,now)

    def test_syft_runtime_requires_scanned_image_and_actual_binary_classifier(self):
        s={'descriptor':{'version':'1.56.0'},'source':{'type':'image','metadata':{'imageID':'image'}},
           'artifacts':[{'name':'julia','version':'1.13.3','foundBy':'binary-classifier-cataloger'}]}
        a.validate_syft(s,'image')
        for mutate in [lambda x:x['source']['metadata'].update(imageID='other'),
                       lambda x:x['descriptor'].update(version='other'),lambda x:x['artifacts'][0].update(foundBy='manifest'),
                       lambda x:x['artifacts'][0].update(version='1.12.0')]:
            changed=copy.deepcopy(s);mutate(changed)
            with self.assertRaises(ValueError):a.validate_syft(changed,'image')

    def test_docker_manifest_id_is_bound_to_exported_config_before_syft_comparison(self):
        image='sha256:'+'a'*64
        config=json.dumps({'os':'linux','architecture':'amd64'}).encode()
        config_hash='sha256:'+a.hashlib.sha256(config).hexdigest()
        self.assertNotEqual(image,config_hash)
        calls=[]
        def docker(args, **kwargs):
            calls.append(args)
            if args[1:3]==['image','inspect']:return image.encode()
            if args[1]=='save':
                alias=next(call[-1] for call in calls if call[1]=='tag')
                with tarfile.open(args[3],'w') as archive:
                    for name,data in [('manifest.json',json.dumps([{'RepoTags':[alias],'Config':'blobs/sha256/'+config_hash[7:]}]).encode()),
                                      ('blobs/sha256/'+config_hash[7:],config)]:
                        member=tarfile.TarInfo(name);member.size=len(data);archive.addfile(member,io.BytesIO(data))
            return b''
        with tempfile.TemporaryDirectory() as d,patch.object(a.proof,'required',side_effect=docker):
            self.assertEqual(a.image_config_digest(image,Path(d)),config_hash)
        with tempfile.TemporaryDirectory() as d,patch.object(a.proof,'required',side_effect=[b'',b'',b'wrong-id']):
            with self.assertRaises(ValueError):a.image_config_digest(image,Path(d))
        sbom={'descriptor':{'version':'1.56.0'},'source':{'type':'image','metadata':{'imageID':config_hash}},
              'artifacts':[{'name':'julia','version':'1.13.3','foundBy':'binary-classifier-cataloger'}]}
        a.validate_syft(sbom,config_hash)
        with self.assertRaises(ValueError):a.validate_syft(sbom,image)

    def test_safe_identity_projection_never_exports_config_or_unbounded_fields(self):
        sbom={'descriptor':{'version':'private'},'source':{'type':'private','metadata':{'imageID':'private','manifestDigest':'private','config':'private'}}}
        self.assertEqual(a.syft_identity_projection(sbom),{'version':'invalid','type':'invalid','imageID':'invalid','manifestDigest':'invalid'})

    def test_report_requires_inventory_controls_snapshot_and_explicit_limits(self):
        expected=self.expected();now=dt.datetime(2026,11,6,tzinfo=dt.timezone.utc)
        q={'sourceSHA':'b'*40,'imageID':'sha256:'+'a'*64,'inventory':{'packages':{'ProtoBuf':{'treeSHA1':'c'*40}}}}
        report={'passed':True,'localOnly':True,'knownAdvisoryControlPassed':True,'limitations':a.LIMITATIONS,
                'sourceSHA':q['sourceSHA'],'imageID':q['imageID'],'installedPackages':q['inventory']['packages'],
                'juliaManifest':expected,'spdxPackages':expected,'scanTime':now.isoformat(),
                'archiveConfigDigest':'sha256:'+'f'*64,
                'syftIdentity':{'version':'1.56.0','type':'image','imageID':'sha256:'+'f'*64},
                'database':{'sha256':'d'*64,'Version':2,'UpdatedAt':(now-dt.timedelta(hours=1)).isoformat(),
                            'NextUpdate':(now+dt.timedelta(hours=1)).isoformat(),'DownloadedAt':now.isoformat()},
                'tools':{n:{'archiveSHA256':h,'executableSHA256':'e'*64}for n,h in a.ARCHIVES.items()},
                'runtimeAndOSInventory':[{'name':'julia','version':'1.13.3','foundBy':'binary-classifier-cataloger'}],'otherScanFindings':[]}
        jll = a.jll_advisory
        q['inventory']['nativeRuntime'] = {'jll': {'Fixture'+str(i)+'_jll': {'name':'Fixture'+str(i)+'_jll',
            'uuid':'00000000-0000-0000-0000-'+str(i).zfill(12), 'version':'1.0.0'} for i in range(23)}}
        packages = jll.packages(q['inventory']['nativeRuntime'])
        report['jllAdvisory'] = {'registryCommit':jll.REGISTRY_COMMIT,'registrySHA256':jll.REGISTRY_SHA256,
            'packages':packages,'scannerManifestSHA256':a.hashlib.sha256(jll.manifest(packages)).hexdigest(),
            'scannerProjectSHA256':a.hashlib.sha256(jll.project(packages)).hexdigest(),
            'knownJLLAdvisoryControlPassed':True,'advisories':[],'scope':'archive-bound-JLL-metadata-only'}
        a.validate_report(report,q,expected)
        for key in report:
            changed=copy.deepcopy(report);del changed[key]
            with self.subTest(key=key),self.assertRaises((ValueError,KeyError)):a.validate_report(changed,q,expected)
        changed=copy.deepcopy(report);changed['otherScanFindings']=[{'advisories':[{'severity':'CRITICAL'}]}]
        with self.assertRaises(ValueError):a.validate_report(changed,q,expected)


if __name__=='__main__':unittest.main()
