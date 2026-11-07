import copy
import hashlib
import json
import unittest

import image_custody as image


class ImageCustodyTest(unittest.TestCase):
    def setUp(self):
        self.blobs={}
        self.config={'architecture':'amd64','os':'linux','rootfs':{'type':'layers','diff_ids':['sha256:'+'1'*64]},'config':{'Env':['SECRET=not-exported']}}
        self.cfg=self.add(self.config)
        self.manifest={'schemaVersion':2,'mediaType':'application/vnd.oci.image.manifest.v1+json','config':self.desc(self.cfg,'application/vnd.oci.image.config.v1+json'),
                       'layers':[{'mediaType':'application/vnd.oci.image.layer.v1.tar+gzip','digest':'sha256:'+'2'*64,'size':123}]}
        self.man=self.add(self.manifest)
        self.index={'schemaVersion':2,'mediaType':'application/vnd.oci.image.index.v1+json',
                    'manifests':[dict(self.desc(self.man,self.manifest['mediaType']),platform={'os':'linux','architecture':'amd64'})]}
        self.idx=self.add(self.index)
        self.tag='kanz-native-probe:'+'a'*40

    def add(self,value):
        raw=json.dumps(value,separators=(',',':')).encode()
        digest='sha256:'+hashlib.sha256(raw).hexdigest();self.blobs[digest]=raw
        return digest

    def desc(self,digest,kind):
        return {'digest':digest,'size':len(self.blobs[digest]),'mediaType':kind}

    def verify(self,host=None,node=None,inspection=None,read=None):
        node=node or self.idx
        ref='docker.io/library/import-fixed@'+node
        inspection=inspection or {'status':{'id':self.cfg,'repoTags':['docker.io/library/'+self.tag],'repoDigests':[ref]}}
        return image.verify(inspection,self.tag,ref,host or self.idx,read or self.blobs.__getitem__)

    def test_verified_digest_types_not_untyped_equality(self):
        for host,kind in [(self.cfg,'config'),(self.man,'manifest'),(self.idx,'index')]:
            for node in (self.man,self.idx):
                value=self.verify(host,node)
                self.assertTrue(value['verified'])
                self.assertEqual(value['dockerIdentityType'],kind)
                self.assertEqual(value['config'],self.cfg)
                self.assertEqual(value['platformManifest'],self.man)
                self.assertNotIn('SECRET',json.dumps(value))

    def test_wrong_hash_graph_platform_and_descriptor_refuse(self):
        with self.assertRaises(ValueError):self.verify(read=lambda _:b'{}')
        foreign=copy.deepcopy(self.config);foreign['architecture']='arm64'
        with self.assertRaises(ValueError):self.verify(host=self.add(foreign))
        foreign=copy.deepcopy(self.config);foreign['rootfs']['diff_ids']=['sha256:'+'3'*64]
        with self.assertRaises(ValueError):self.verify(host=self.add(foreign))
        for mutate in (lambda x:x['manifests'].append(copy.deepcopy(x['manifests'][0])),lambda x:x['manifests'][0].update(size=1),lambda x:x['manifests'][0].update(urls=['https://forbidden']),lambda x:x.update(mediaType='unknown')):
            bad=copy.deepcopy(self.index);mutate(bad)
            with self.assertRaises(ValueError):self.verify(node=self.add(bad))

    def test_source_tag_cri_and_pod_reference_must_agree(self):
        good={'status':{'id':self.cfg,'repoTags':['docker.io/library/'+self.tag],'repoDigests':['docker.io/library/import-fixed@'+self.idx]}}
        for mutate in (lambda x:x['status'].update(id=self.man),lambda x:x['status'].update(repoTags=['docker.io/library/kanz-native-probe:'+'b'*40]),lambda x:x['status'].update(repoDigests=[])):
            bad=copy.deepcopy(good);mutate(bad)
            with self.assertRaises(ValueError):self.verify(inspection=bad)
        for ref in ('https://host/path','containerd://'+self.cfg,'sha256:bad'):
            with self.assertRaises(ValueError):image.reference_digest(ref)

    def test_closed_predicate_codes_and_progress_do_not_export_content(self):
        progress={}
        ref='docker.io/library/import-fixed@'+self.idx
        inspection={'status':{'id':self.cfg,'repoTags':['docker.io/library/'+self.tag],'repoDigests':[ref]}}
        with self.assertRaises(image.CustodyError) as caught:
            image.verify(inspection,self.tag,ref,self.idx,lambda _:b'SECRET',progress)
        self.assertEqual(caught.exception.code,'image_content_digest_differs')
        self.assertEqual(progress,{'phase':'pod_graph','contentCount':0,'mediaType':'UNKNOWN'})
        self.assertNotIn('SECRET',str(caught.exception))
        self.assertEqual(image.failure(RuntimeError('SECRET')),'collector_failure')
        self.assertEqual(image.failure(TimeoutError('SECRET')),'observation_timeout')
        with self.assertRaises(ValueError):image.CustodyError('SECRET')
        result=image.verify(inspection,self.tag,ref,self.idx,self.blobs.__getitem__,progress)
        self.assertEqual(progress['phase'],'complete')
        self.assertEqual(progress['contentCount'],result['contentCount'])


if __name__=='__main__': unittest.main()
