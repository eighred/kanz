import copy
import hashlib
import json
from pathlib import Path
import tempfile
import tomllib
import unittest
from unittest.mock import patch

import advisory
import jll_advisory as j


class JLLTests(unittest.TestCase):
    def native(self):
        return {'jll': {'Fixture'+str(i)+'_jll': {'name': 'Fixture'+str(i)+'_jll',
            'uuid': '00000000-0000-0000-0000-'+str(i).zfill(12), 'version':'1.2.3+0'} for i in range(23)}}

    def registry(self, expected):
        return ('[packages]\n'+'\n'.join(k['uuid']+' = { name = "'+n+'", path = "jll/'+n[0].upper()+'/'+n+'" }'
            for n,k in expected.items())).encode()

    def test_registry_requires_pinned_bytes_and_uuid_name_path(self):
        expected=j.packages(self.native());raw=self.registry(expected)
        with patch.object(j,'REGISTRY_SHA256',hashlib.sha256(raw).hexdigest()):
            j.registry_identity(raw,expected)
            with self.assertRaises(ValueError):j.registry_identity(raw+b'\n',expected)
            for key,value in [('uuid',j.OPENSSL_UUID)]:
                changed=copy.deepcopy(expected);changed['Fixture0_jll'][key]=value
                with self.assertRaises(ValueError):j.registry_identity(raw,changed)
        bad=raw.replace(b'jll/F/Fixture0_jll',b'elsewhere/Fixture0_jll')
        with patch.object(j,'REGISTRY_SHA256',hashlib.sha256(bad).hexdigest()):
            with self.assertRaises(ValueError):j.registry_identity(bad,expected)
        with self.assertRaises(ValueError):j.registry_identity(b'x'*(4*1024*1024+1),expected)

    def test_inventory_scope_and_manifest_roundtrip(self):
        native=self.native();expected=j.packages(native)
        parsed=tomllib.loads(j.manifest(expected).decode())
        self.assertEqual({n:v[0] for n,v in parsed['deps'].items()},expected)
        for mutate in [lambda n:n['jll'].pop('Fixture0_jll'),
                       lambda n:n['jll']['Fixture0_jll'].update(name='Wrong_jll'),
                       lambda n:n['jll']['Fixture0_jll'].update(uuid=n['jll']['Fixture1_jll']['uuid']),
                       lambda n:n['jll']['Fixture0_jll'].update(version='1\nmalicious')]:
            changed=copy.deepcopy(native);mutate(changed)
            with self.assertRaises(ValueError):j.packages(changed)

    def test_control_binds_exact_package_version_and_advisory(self):
        v={'PkgName':'OpenSSL_jll','PkgID':j.OPENSSL_UUID,'InstalledVersion':'3.0.12+0','VulnerabilityID':'CVE-2024-5535'}
        j.validate_control({'Vulnerabilities':[v]})
        for key in v:
            with self.assertRaises(ValueError):j.validate_control({'Vulnerabilities':[{**v,key:'other'}]})
        with self.assertRaises(ValueError):j.validate_control({})

    def test_real_qualify_flow_scans_control_then_bound_inventory_and_refuses_findings(self):
        native=self.native();expected=j.packages(native);raw=self.registry({**expected,**j.CONTROL})
        def scan(packages, vulnerabilities):
            return json.dumps({'Trivy':{'Version':'0.72.0'},'Results':[{'Type':'julia','Target':'Manifest.toml',
                'Packages':[{'Name':n,'ID':p['uuid'],'Version':p['version']} for n,p in packages.items()],
                'Vulnerabilities':vulnerabilities}]}).encode()
        control=[{'PkgName':'OpenSSL_jll','PkgID':j.OPENSSL_UUID,'InstalledVersion':'3.0.12+0','VulnerabilityID':'CVE-2024-5535'}]
        for bad in (False,True):
            calls=[]
            def run(args, **kwargs):
                calls.append(args)
                if args[0]=='curl':return raw
                cwd=kwargs['cwd'];p=tomllib.loads((cwd/'Manifest.toml').read_text())
                self.assertFalse((cwd/'Project.toml').exists())
                if cwd.name=='jll-control':return scan(j.CONTROL,control)
                self.assertEqual({n:v[0] for n,v in p['deps'].items()},expected)
                return scan(expected,[{'PkgName':'Fixture0_jll','Severity':'LOW'}] if bad else [])
            with tempfile.TemporaryDirectory() as tmp,patch.object(j,'REGISTRY_SHA256',hashlib.sha256(raw).hexdigest()):
                report={}
                if bad:
                    with self.assertRaises(ValueError):j.qualify(run,[],Path(tmp),native,advisory.manifest_result,advisory.validate_packages,report)
                    self.assertEqual(report['advisories'][0]['Severity'],'LOW')
                else:
                    j.qualify(run,[],Path(tmp),native,advisory.manifest_result,advisory.validate_packages,report)
                    j.validate_report(report,native)
                    for key in list(report):
                        changed=copy.deepcopy(report);changed.pop(key)
                        with self.assertRaises(ValueError):j.validate_report(changed,native)
                self.assertEqual([c[0] for c in calls],['curl','trivy','trivy'])


if __name__=='__main__':unittest.main()
