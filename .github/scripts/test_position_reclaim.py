"""Discriminating, local-only refusal controls for the #1014 proof boundary."""
import copy
import errno
import json
import os
from pathlib import Path
import socket
import stat
import tempfile
import threading
import types
import unittest
from unittest import mock

import position_reclaim_proof as proof
import position_reclaim_sandbox as sandbox
import required_ci


class ProtectedFiles(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root=Path(self.temp.name)
        self.fd=sandbox.directory(self.root)
        self.addCleanup(os.close,self.fd)

    def test_regular_roundtrip_and_exclusive_destination(self):
        sandbox.new_file(self.fd,"receipt",b"actual bytes")
        self.assertEqual(sandbox.read_regular(self.fd,"receipt",12),b"actual bytes")
        with self.assertRaises(FileExistsError): sandbox.new_file(self.fd,"receipt",b"replacement")
        with self.assertRaises(ValueError): sandbox.read_regular(self.fd,"receipt",11)
        previous=os.umask(0o777)
        try:
            sandbox.new_file(self.fd,"complete.json",b"reviewed completion",mode=0o644)
            sandbox.new_file(self.fd,"private",b"private evidence")
        finally: os.umask(previous)
        self.assertEqual(stat.S_IMODE((self.root/"complete.json").stat().st_mode),0o644)
        self.assertEqual(stat.S_IMODE((self.root/"private").stat().st_mode),0o600)
        self.assertEqual(sandbox.read_regular(self.fd,"complete.json",32),b"reviewed completion")

    def test_links_fifo_and_parent_traversal_refused(self):
        (self.root/"ordinary").write_bytes(b"value")
        os.symlink("ordinary",self.root/"link")
        os.mkfifo(self.root/"pipe")
        os.link(self.root/"ordinary",self.root/"hardlink")
        (self.root/"folder").mkdir()
        os.symlink("folder",self.root/"parent")
        for name in ("link","pipe","hardlink","ordinary","parent/new","../escape"):
            with self.subTest(name=name),self.assertRaises((OSError,ValueError)):
                sandbox.read_regular(self.fd,name,32)

    def test_symlink_directory_refused(self):
        os.symlink(self.root,self.root/"directory-link")
        with self.assertRaises(OSError): sandbox.directory(self.root/"directory-link")


class KernelBoundary(unittest.TestCase):
    def test_synthetic_root_and_device_directories_are_readonly(self):
        # Refuse before attempting any write if called outside the owned lane.
        self.assertEqual(os.getuid(),65534)
        self.assertTrue(os.statvfs("/").f_flag & os.ST_RDONLY)
        for path in ("/position-unit-probe","/etc/position-unit-probe","/dev/position-unit-probe","/dev/shm/position-unit-probe","/fixture/position-unit-probe","/receipt/position-unit-probe"):
            with self.subTest(path=path):
                try: fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
                except OSError as error: self.assertIn(error.errno,(errno.EROFS,errno.EACCES))
                else:
                    os.close(fd);os.unlink(path)
                    self.fail("write escaped owned scratch")
        with open("/dev/null","wb") as sink: self.assertEqual(sink.write(b"required device"),15)


class EvidenceRefusals(unittest.TestCase):
    def test_recreation_even_with_same_image_and_storage_is_rejected(self):
        before={"container":"a","image":"same","started":"first","restart_count":0,"name":"owned"}
        proof.stable_broker(before,dict(before))
        for key,value in (("container","b"),("started","second"),("restart_count",1),("image","different")):
            changed=dict(before,**{key:value})
            with self.subTest(key=key),self.assertRaises(ValueError): proof.stable_broker(before,changed)

    def test_negative_fixture_mounts_refuse_anonymous_or_extra_writes(self):
        data=Path("/owned/data")
        mounts=[{"Type":"bind","Source":str(data),"Destination":"/data","RW":True},{"Type":"tmpfs","Destination":"/dev/shm","RW":False}]
        proof.validate_negative_mounts({"Mounts":mounts},data)
        for extra in ({"Type":"volume","Destination":"/anonymous","RW":True},{"Type":"bind","Source":"/","Destination":"/host","RW":True},{"Type":"tmpfs","Destination":"/other","RW":True}):
            with self.assertRaises(ValueError): proof.validate_negative_mounts({"Mounts":mounts+[extra]},data)
        writable_shm=copy.deepcopy(mounts);writable_shm[1]["RW"]=True
        with self.assertRaises(ValueError): proof.validate_negative_mounts({"Mounts":writable_shm},data)
        with self.assertRaises(ValueError): proof.validate_negative_mounts({"Mounts":mounts[1:]},data)

    def native_events(self):
        result=[]
        for name in ("TestRustPositionStorePostgresNATS","TestRustRelayPostgresNATS","TestRustPositionProjectorPostgresNATS"):
            result += [{"Action":"run","Package":proof.STORE,"Test":name},{"Action":"pass","Package":proof.STORE,"Test":name}]
        return result

    def test_native_skip_or_missing_native_case_is_not_proof(self):
        rows=self.native_events()
        self.assertEqual(len(proof.require_native_cases(map(json.dumps,rows))),3)
        missing=rows[2:]
        with self.assertRaises(ValueError): proof.require_native_cases(map(json.dumps,missing))
        skipped=copy.deepcopy(rows)
        skipped[1]["Action"]="skip"
        with self.assertRaises(ValueError): proof.require_native_cases(map(json.dumps,skipped))

    def test_complete_event_inventory_rejects_truncation_duplicate_and_cached(self):
        package=proof.STORE
        packages=sorted({package}|{p for p,_ in proof.CAUSAL_CASES})
        rows=[]
        for p in packages:
            rows.append({"Action":"start","Package":p})
            for owner,name in sorted(proof.CAUSAL_CASES):
                if owner==p: rows += [{"Action":"run","Package":p,"Test":name},{"Action":"output","Package":p,"Test":name,"Output":"causal live-OMS POSITION receipts: 1 inputs, both exact keys per input"},{"Action":"pass","Package":p,"Test":name}]
            if p==package: rows += self.native_events()+[{"Action":"output","Package":p,"Output":"Chromium shared-session inventory and revocation PASS"}]
            rows.append({"Action":"pass","Package":p})
        raw=lambda value:("\n".join(map(json.dumps,value))+"\n").encode()
        self.assertEqual(proof.pass_result(raw(rows),packages)["package_count"],len(packages))
        variants=[rows[:-1],rows+[rows[-1]],[*rows[:-1],{"Action":"output","Package":package,"Output":"ok pkg (cached)"},rows[-1]]]
        for variant in variants:
            with self.assertRaises(ValueError): proof.pass_result(raw(variant),packages)
        with self.assertRaises(ValueError): proof.pass_result(raw(rows),packages+[proof.CONTROL])
        without_causal=[r for r in rows if "causal live-OMS" not in r.get("Output","")]
        with self.assertRaises(ValueError): proof.pass_result(raw(without_causal),packages)
        skipped_after_marker=copy.deepcopy(rows)
        owner,name=sorted(proof.CAUSAL_CASES)[0]
        for row in skipped_after_marker:
            if row["Action"]=="pass" and (row.get("Package"),row.get("Test"))==(owner,name): row["Action"]="skip"
        with self.assertRaises(ValueError): proof.pass_result(raw(skipped_after_marker),packages)

    def test_scope_uses_source_paths_and_preserves_existing_owners(self):
        for path in (b"kanz/internal/new/unknown_test.go",b"kanz/internal/bustest/reclaim.go",b"kanz/test/backing/position_reclaim/main.go",b".github/scripts/position_reclaim_proof.py",b".github/workflows/kanz-ci.yml"):
            self.assertTrue(required_ci.position_reclaim_path(path))
            self.assertTrue(required_ci.application_path(path))
        self.assertFalse(required_ci.position_reclaim_path(b"docs/readme.md"))
        for path in (b"kanz/services/oms/internal/position/projector.go",b"kanz/pkg/bus/frame.go",b"kanz/internal/platform/subject/topic.go",b"kanz-rs/crates/oms-store/src/position.rs",b"kanz/infra/nats/bootstrap-job.yaml"):
            self.assertTrue(required_ci.position_reclaim_path(path))

    def test_workflow_cannot_drop_or_unconditionally_disable_proof(self):
        text=Path(".github/workflows/kanz-ci.yml").read_text()
        required_ci.workflow_jobs(text)
        for mutated in (text.replace("python3 -B ../.github/scripts/position_reclaim_proof.py","true # removed proof"),text.replace('&& [ "$POSITION_RECLAIM_SCOPE" = "true" ]','&& [ "$POSITION_RECLAIM_SCOPE" = "never" ]')):
            with self.assertRaises(ValueError): required_ci.workflow_jobs(mutated)


class FixedLoopbackRelay(unittest.TestCase):
    def test_only_declared_endpoint_transports_bytes_and_closes(self):
        upstream=socket.socket()
        upstream.bind(("127.0.0.2",0))
        upstream.listen(1)
        port=upstream.getsockname()[1]
        def echo():
            with upstream:
                conn,_=upstream.accept()
                with conn:
                    data=conn.recv(64)
                    conn.sendall(data)
        thread=threading.Thread(target=echo)
        thread.start()
        relay=sandbox.FixedRelay("127.0.0.1","127.0.0.2",ports=(port,))
        try:
            with socket.create_connection(("127.0.0.1",port),timeout=2) as client:
                client.sendall(b"canonical bytes")
                self.assertEqual(client.recv(64),b"canonical bytes")
        finally:
            relay.close()
            thread.join(3)
        self.assertFalse(thread.is_alive())


class CleanupRefusals(unittest.TestCase):
    def test_log_refusal_does_not_skip_descriptors_or_mount_cleanup(self):
        with tempfile.TemporaryDirectory() as raw:
            root=Path(raw)
            work=root/"scratch";work.mkdir()
            public=root/"public";public.mkdir()
            box=object.__new__(sandbox.Sandbox)
            box.active=set();box.inner=None;box.relay=None
            box.veth_created=False;box.net_created=False
            box.namespace="owned-fixture";box.interface="owned-veth"
            box.work=work;box.work_fd=sandbox.directory(work)
            box.out_fd=sandbox.directory(root)
            box.public_mounts={"fixture":public};box.mounted=True
            box.executions=[]
            work_fd,out_fd=box.work_fd,box.out_fd
            def host(*args,**kwargs):
                if args==("umount",str(work)): raise ValueError("owned scratch unmount refused")
                return b""
            with mock.patch.object(sandbox,"host",side_effect=host) as calls,mock.patch.object(sandbox.subprocess,"run",return_value=types.SimpleNamespace(returncode=1)),mock.patch.object(sandbox,"new_file",side_effect=ValueError("unsafe log refused")):
                with self.assertRaises(ValueError): box.close()
            self.assertIn(mock.call("umount",str(work)),calls.call_args_list)
            self.assertIn(mock.call("umount",str(public)),calls.call_args_list)
            self.assertTrue(box.mounted,"failed unmount was mislabeled successful")
            for fd in (work_fd,out_fd):
                with self.assertRaises(OSError): os.fstat(fd)


if __name__=="__main__": unittest.main()
