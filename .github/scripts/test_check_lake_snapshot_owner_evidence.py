"""Offline checker mutations; these synthetic events are never runtime proof."""
import copy
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
import hashlib
from unittest.mock import patch

import check_lake_snapshot_owner_evidence as gate
import lake_cache_probe as cache


def events(version="2.12.6"):
    package = gate.PREFIX + gate.PACKAGE
    result = [{"Action":"start","Package":package}]
    for name in sorted(gate.NAMES):
        result += [{"Action":"run","Package":package,"Test":name},
                   {"Action":"output","Package":package,"Test":name,"Output":"actual snapshot broker version: " + version + "\n"},
                   {"Action":"pass","Package":package,"Test":name}]
    result += [{"Action":"output","Package":package,"Output":"actual snapshot PostgreSQL role: NOSUPERUSER NOBYPASSRLS; FORCE RLS tables: 2\n"},
               {"Action":"pass","Package":package}]
    return result


def wire(items):
    return ("\n".join(json.dumps(e) for e in items)+"\n").encode()


class EvidenceTests(unittest.TestCase):
    def test_complete_both_versions(self):
        for version in gate.VERSIONS:
            gate.check_stream(wire(events(version)),version)

    def test_every_required_outcome_missing_or_skipped_refuses(self):
        for name in gate.NAMES:
            for change in ("remove","skip"):
                with self.subTest(name=name,change=change):
                    rows=events()
                    if change=="remove": rows=[e for e in rows if not(e.get("Test")==name and e["Action"]=="pass")]
                    else:
                        for e in rows:
                            if e.get("Test")==name and e["Action"]=="pass": e["Action"]="skip"
                    with self.assertRaises(ValueError):gate.check_stream(wire(rows),"2.12.6")

    def test_failure_duplicate_truncation_cache_and_unexecuted_refuse(self):
        base=events()
        bad=[base[:-1],base+[base[-1]],base+[{"Action":"fail","Package":gate.PREFIX+gate.PACKAGE}],
             [e for e in base if e["Action"]!="run"],
             base+[{"Action":"output","Package":gate.PREFIX+gate.PACKAGE,"Output":"ok (cached)"}]]
        for rows in bad:
            with self.assertRaises(ValueError):gate.check_stream(wire(rows),"2.12.6")
        with self.assertRaises((ValueError,KeyError)):gate.check_stream(b'{"Action":',"2.12.6")

    def test_wrong_broker_and_missing_role_evidence_refuse(self):
        with self.assertRaises(ValueError):gate.check_stream(wire(events("2.14.5")),"2.12.6")
        rows=[e for e in events() if "FORCE RLS" not in e.get("Output","")]
        with self.assertRaises(ValueError):gate.check_stream(wire(rows),"2.12.6")

    def test_source_attempt_stream_binary_and_cleanup_mutations(self):
        hashes={v+".jsonl":"c"*64 for v in gate.VERSIONS}
        p={"schema":2,"source":"a"*40,"tree":"b"*40,"run":"123","attempt":"2","versions":list(gate.VERSIONS),
           "binaries":{name:"d"*64 for name in gate.BINARIES},"streams":hashes,
           "cleanup":{"run":"123","attempt":"2","postgres_absent":True,"compose_absent":True,"image_containers_absent":True,"image_volumes_absent":True},
           "dependencies":{"postgres":"sha256:"+"e"*64,"redpanda":"sha256:"+"f"*64},
           "hybrid":{"image":"sha256:"+"e"*64,"worker":"f"*64,"files":{name:"c"*64 for name in gate.HYBRID_FILES}}}
        def check(item):gate.check_manifest(item,"a"*40,"b"*40,"123","2",hashes)
        check(p)
        for key,value in (("source","z"*40),("tree","a"*40),("run","124"),("attempt","1"),("versions",["2.14.5"]),("schema",True)):
            changed=copy.deepcopy(p);changed[key]=value
            with self.assertRaises(ValueError):check(changed)
        for key in gate.BINARIES:
            changed=copy.deepcopy(p);del changed["binaries"][key]
            with self.assertRaises(ValueError):check(changed)
        for field,value in (("run","124"),("attempt","1"),("postgres_absent",False),("compose_absent",1),("image_containers_absent",False),("image_volumes_absent",1)):
            changed=copy.deepcopy(p);changed["cleanup"][field]=value
            with self.assertRaises(ValueError):check(changed)
        changed=copy.deepcopy(p);changed["streams"]["2.12.6.jsonl"]="0"*64
        with self.assertRaises(ValueError):check(changed)
        changed=copy.deepcopy(p);changed["binaries"]["worker-swap"]="0"*64
        with self.assertRaises(ValueError):check(changed)

    def test_image_execution_binding_and_cgroup_are_required(self):
        hybrid={"image":"sha256:"+"e"*64,"worker":"f"*64}
        output="actual hybrid identity: image="+hybrid["image"]+" worker="+hybrid["worker"]+" source="+"a"*40+"\nactual hybrid cgroup: memory_max=1610612736 memory_peak=123456 pids_max=64\n"
        rows=[{"Action":"output","Test":gate.IMAGE_CASE,"Output":output}]
        gate.image_stream(wire(rows),hybrid,"a"*40)
        for old,new in (("a"*40,"b"*40),("f"*64,"a"*64),("pids_max=64","pids_max=0"),("memory_peak=123456","memory_peak=9999999999"),("memory_max=1610612736","memory_max=512")):
            with self.subTest(old=old),self.assertRaises(ValueError):
                gate.image_stream(wire([{**rows[0],"Output":output.replace(old,new)}]),hybrid,"a"*40)
        with self.assertRaises(ValueError):gate.image_stream(wire(rows+rows),hybrid,"a"*40)
        with self.assertRaises(ValueError):gate.image_stream(wire([]),hybrid,"a"*40)

    def test_hybrid_artifact_identity_audits_and_member_hashes(self):
        image="sha256:"+"a"*64
        bodies={name:b'{}' for name in gate.HYBRID_FILES}
        bodies['lake-image.txt']=(image+'\n').encode()
        bodies['source.txt']=("b"*40+"\n"+"f"*40+"\n").encode()
        bodies['run.txt']=b'1\n2\n'
        version=cache.digest(("kanz-lake-joint-cache-probe:"+"b"*40).encode())[:40]
        vertices=[{'digest':'sha256:'+'1'*64,'name':'[rust-build 2/6] RUN '+cache.COOK,'started':'start','completed':'done','cached':True},
                  {'digest':'sha256:'+'2'*64,'name':'[rust-build 6/6] RUN '+cache.BUILD.replace('$VERSION',version),'started':'start','completed':'done'}]
        bodies['cache-probe-progress.jsonl']=(json.dumps({'vertexes':vertices})+'\n').encode()
        probe={'schema':1,'source':'b'*40,'tree':'f'*40,'run':'1','attempt':'2','image':image,'builder':'synthetic',
               **{k:'d'*64 for k in ('contextSHA256','dockerfileSHA256','sourceBeforeSHA256')},
               'sourceAfterSHA256':'e'*64,'versionSHA256':cache.digest(version.encode()),
               'progressSHA256':cache.digest(bodies['cache-probe-progress.jsonl']),**cache.progress(bodies['cache-probe-progress.jsonl'],'b'*40),
               'jointSourceAndVersion':True,'cookCached':True,'finalBuildExecuted':True,'syntheticOnly':True}
        bodies['cache-probe.json']=json.dumps(probe).encode()
        bodies['hybrid-trivy.json']=json.dumps({'Trivy':{'Version':'0.72.0'},'Metadata':{'ImageID':image},
            'Results':[{'Class':'os-pkgs','Packages':[{'Name':'synthetic'}]}]}).encode()
        audit={'vulnerabilities':{'found':False,'count':0,'list':[]},'warnings':{},
               'settings':{'ignore':[]},'database':{'last-commit':'d'*40}}
        for name in ('lock','binary'):bodies['hybrid/audit-'+name+'.json']=json.dumps(audit).encode()
        native={'base':{'elf':{},'links':{}},'image':{'elf':{},'links':{}}}
        for name,char,mode in (('/lake-sink','e',0o755),('/kanz-lake-snapshot','c',0o555)):
            native['image']['elf'][name]={'sha256':char*64,'mode':mode,'uid':0,'gid':0,'paxMetadata':{}}
        bodies['hybrid/native-inventory.json']=json.dumps(native).encode()
        bodies['hybrid/go-buildinfo.json']=json.dumps({'GoVersion':'go1.26.10','Deps':[]}).encode()
        config={'protocol_version':'v1.0.0','scanner_name':'govulncheck','scanner_version':'v1.6.0',
            'scan_mode':'binary','scan_level':'symbol','db':'https://vuln.go.dev','db_last_modified':'2026-11-07T00:00:00Z'}
        modules={('github.com/eighred/kanz','(devel)'),('stdlib','go1.26.10')}
        sbom={'go_version':'go1.26.10','roots':['github.com/eighred/kanz'],
              'modules':[{'path':p,'version':v} for p,v in sorted(modules)]}
        go_scan=json.dumps({'config':config})+'\n'+json.dumps({'SBOM':sbom})+'\n'
        bodies['hybrid/go-audit.json']=go_scan.encode()
        evidence={'passed':True,'localOnly':True,'sourceSHA':'b'*40,'image':image,'imageId':image,
            'binarySHA256':'c'*64,'lockAdvisoryDBCommit':'d'*40,'binaryAdvisoryDBCommit':'d'*40,
            'sbomSHA256':hashlib.sha256(bodies['hybrid.spdx.json']).hexdigest(),'hybrid':{
                'goBinarySHA256':'e'*64,'goVersion':'go1.26.10','nativeELFs':2,
                'nativeBase':gate.image_recipe('lake-sink').native_base.replace(':nonroot@','@'),
                'goAudit':gate.validate_go_audit(go_scan,'go1.26.10',modules)}}
        for field,name in (('goBuildinfoSHA256','go-buildinfo.json'),('nativeInventorySHA256','native-inventory.json'),
                           ('goPlainAuditSHA256','go-audit.txt'),('goJSONAuditSHA256','go-audit.json')):
            evidence['hybrid'][field]=hashlib.sha256(bodies['hybrid/'+name]).hexdigest()
        bodies['hybrid/evidence.json']=json.dumps(evidence).encode()
        def check(values):
            def read(path,limit):
                raw=values[path.as_posix().removeprefix('proof/')]
                if len(raw)>limit:raise ValueError('test bound')
                return raw
            with patch.object(gate,'read_file',side_effect=read), patch.object(gate,'cache_source_hashes',return_value={key:probe[key] for key in cache.SOURCE_HASHES}):return gate.image_evidence(Path('proof'),'b'*40)
        self.assertEqual(check(bodies)['worker'],'c'*64)
        for name in ('hybrid.spdx.json','hybrid/go-buildinfo.json','hybrid/native-inventory.json','hybrid/go-audit.txt','hybrid/go-audit.json'):
            changed=dict(bodies);changed[name]+=b' '
            with self.subTest(name=name),self.assertRaises(ValueError):check(changed)
        for field,value in (('passed',False),('sourceSHA','f'*40),('imageId','sha256:'+'f'*64)):
            changed=dict(bodies);changed['hybrid/evidence.json']=json.dumps({**evidence,field:value}).encode()
            with self.assertRaises(ValueError):check(changed)
        changed=dict(bodies);bad=copy.deepcopy(audit);bad['warnings']={'unmaintained':[{}]}
        changed['hybrid/audit-lock.json']=json.dumps(bad).encode()
        with self.assertRaises(ValueError):check(changed)
        for key,value in [('source','f'*40),('image','sha256:'+'f'*64),('cookCached',False), *[(key,'f'*64) for key in cache.SOURCE_HASHES]]:
            changed=dict(bodies);changed['cache-probe.json']=json.dumps({**probe,key:value}).encode()
            with self.subTest(cache=key),self.assertRaises(ValueError):check(changed)
        # Coherently resealed metadata must still satisfy the native/Go verifier.
        for mode in ('native-owner','go-scanner'):
            changed=dict(bodies);report=copy.deepcopy(evidence)
            if mode=='native-owner':
                altered=copy.deepcopy(native);altered['image']['elf']['/kanz-lake-snapshot']['uid']=65532
                name,field='native-inventory.json','nativeInventorySHA256'
                changed['hybrid/'+name]=json.dumps(altered).encode()
            else:
                name,field='go-audit.json','goJSONAuditSHA256'
                changed['hybrid/'+name]=go_scan.replace('"binary"','"source"').encode()
            report['hybrid'][field]=hashlib.sha256(changed['hybrid/'+name]).hexdigest()
            changed['hybrid/evidence.json']=json.dumps(report).encode()
            with self.subTest(mode=mode),self.assertRaises(ValueError):check(changed)

    def test_duplicate_json_key_refuses(self):
        with self.assertRaises(ValueError):json.loads('{"a":1,"a":2}',object_pairs_hook=gate.strict)

    def test_source_inventory_matches_checkout(self):
        gate.check_source_inventory(Path(__file__).resolve().parents[2])

    @unittest.skipUnless(sys.platform=="linux","Linux no-follow/nonblocking file proof")
    def test_bounded_reader_rejects_symlink_fifo_and_oversize(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp);regular=root/"regular";regular.write_bytes(b"proof")
            self.assertEqual(gate.read_file(regular,5),b"proof")
            with self.assertRaises(ValueError):gate.read_file(regular,4)
            link=root/"link";link.symlink_to(regular)
            with self.assertRaises((ValueError,OSError)):gate.read_file(link,10)
            fifo=root/"fifo";os.mkfifo(fifo)
            with self.assertRaises(ValueError):gate.read_file(fifo,10)
            hardlink=root/"hardlink";os.link(regular,hardlink)
            with self.assertRaises(ValueError):gate.read_file(hardlink,10)
            with self.assertRaises(ValueError):gate.read_file(regular,10)
            directory=root/"directory";directory.mkdir()
            with self.assertRaises((ValueError,OSError)):gate.read_file(directory,10)


if __name__=="__main__":unittest.main()
