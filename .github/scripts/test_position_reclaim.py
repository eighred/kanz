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
        self.assertEqual(os.getgid(),65534)
        self.assertEqual(os.getgroups(),[])
        status=dict(line.split(":",1) for line in Path("/proc/self/status").read_text().splitlines() if ":" in line)
        self.assertEqual(int(status["CapEff"].strip(),16),0)
        self.assertEqual(status["NoNewPrivs"].strip(),"1")
        self.assertTrue(os.statvfs("/").f_flag & os.ST_RDONLY)
        for path in ("/position-unit-probe","/etc/position-unit-probe","/dev/position-unit-probe","/dev/shm/position-unit-probe","/fixture/position-unit-probe","/receipt/position-unit-probe"):
            with self.subTest(path=path):
                try: fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
                except OSError as error: self.assertIn(error.errno,(errno.EROFS,errno.EACCES))
                else:
                    os.close(fd);os.unlink(path)
                    self.fail("write escaped owned scratch")
        with open("/dev/null","wb") as sink: self.assertEqual(sink.write(b"required device"),15)


class LaunchPermissions(unittest.TestCase):
    def test_owned_public_staging_ignores_umask_and_keeps_outputs_private(self):
        with tempfile.TemporaryDirectory() as raw:
            root=Path(raw)
            fd=sandbox.directory(root)
            previous=os.umask(0o077)
            try:
                public=sandbox.public_parents(fd,"public",mode=0o711)
                source=sandbox.public_parents(public,"source/nested")
                try: sandbox.new_file(source,"source.txt",b"owned public input",mode=0o644)
                finally: os.close(source)
                sandbox.new_file(public,"passwd",b"synthetic identity",mode=0o644)
                os.close(public)
                (root/"artifacts").mkdir(mode=0o700)
            finally: os.umask(previous);os.close(fd)
            expected={"public":0o711,"public/source":0o755,"public/source/nested":0o755,
                      "public/source/nested/source.txt":0o644,"public/passwd":0o644,"artifacts":0o700}
            for name,mode in expected.items(): self.assertEqual(stat.S_IMODE((root/name).stat().st_mode),mode)
            specs=[["public-parent",str(root/"public"),"directory",0o711,os.X_OK],
                   ["source",str(root/"public/source"),"directory",0o755,os.R_OK|os.X_OK],
                   ["synthetic-identity",str(root/"public/passwd"),"file",0o644,os.R_OK]]
            result=sandbox.launch_permissions(specs)
            self.assertEqual(result["status"],"passed")
            self.assertNotIn(str(root),json.dumps(result))
            self.assertEqual(stat.S_IMODE((root/"artifacts").stat().st_mode),0o700)

    def test_inaccessible_or_wrong_mode_inputs_refuse_without_error_text(self):
        with tempfile.TemporaryDirectory() as raw:
            root=Path(raw);hidden=root/"private-input";hidden.mkdir(mode=0o700)
            leaf=hidden/"data";leaf.write_bytes(b"fixture");hidden.chmod(0)
            try:
                specs=[["readonly-mount",str(leaf),"file",None,os.R_OK]]
                result=sandbox.launch_permissions(specs)
                self.assertEqual(result["status"],"refused")
                self.assertFalse(result["rows"][0]["accessible"])
                self.assertEqual(result["rows"][0]["errno"],errno.EACCES)
                self.assertNotIn(str(root),json.dumps(result))
            finally: hidden.chmod(0o700)
            declaration=[["readonly-mount",str(hidden),"directory",0o700,os.R_OK|os.X_OK]]
            with mock.patch.object(sandbox.os,"access",return_value=False):
                observed=sandbox.launch_permissions(declaration)
                self.assertEqual(observed["status"],"refused")
                self.assertIsNone(observed["rows"][0]["errno"],"os.access supplies no errno")
            for error in (OSError("dummy-private-error"),OSError(4096,"dummy-private-error")):
                with mock.patch.object(sandbox.os,"stat",side_effect=error):
                    observed=sandbox.launch_permissions(declaration)
                    self.assertIsNone(observed["rows"][0]["errno"])
                    self.assertNotIn("dummy-private-error",json.dumps(observed))
            result=sandbox.launch_permissions([["source",str(hidden),"directory",0o755,os.R_OK|os.X_OK]])
            self.assertEqual(result["status"],"refused")
            self.assertTrue(result["rows"][0]["accessible"])
            self.assertFalse(result["rows"][0]["mode_ok"])
            fd=sandbox.directory(root)
            try:
                with self.assertRaisesRegex(ValueError,"public staging mode refused"):
                    sandbox.public_parents(fd,"private-input")
                self.assertEqual(stat.S_IMODE(hidden.stat().st_mode),0o700)
            finally: os.close(fd)

    def test_generated_git_metadata_checks_modes_links_and_bounds_without_chmod(self):
        with tempfile.TemporaryDirectory() as raw:
            root=Path(raw);fd=sandbox.directory(root)
            meta=sandbox.public_parents(fd,".git/objects")
            os.close(meta)
            git_fd=sandbox.directory(root/".git")
            try: sandbox.new_file(git_fd,"HEAD",b"ref: refs/heads/main\n",mode=0o644)
            finally: os.close(git_fd)
            try:
                sandbox.check_git_metadata(fd)
                (root/".git").chmod(0o700)
                with self.assertRaises(ValueError): sandbox.check_git_metadata(fd)
                self.assertEqual(stat.S_IMODE((root/".git").stat().st_mode),0o700)
                (root/".git").chmod(0o755)
                os.symlink("HEAD",root/".git/link")
                with self.assertRaises(OSError): sandbox.check_git_metadata(fd)
                (root/".git/link").unlink()
                os.link(root/".git/HEAD",root/".git/hardlink")
                with self.assertRaises(ValueError): sandbox.check_git_metadata(fd)
                (root/".git/hardlink").unlink()
                with mock.patch.object(sandbox,"MAX_FILE",1):
                    with self.assertRaises(ValueError): sandbox.check_git_metadata(fd)
                with mock.patch.object(sandbox,"MAX_GIT_METADATA_ENTRIES",0):
                    with self.assertRaises(ValueError): sandbox.check_git_metadata(fd)
            finally: os.close(fd)

    def test_git_creation_umask_is_scoped_and_metadata_remains_public(self):
        with tempfile.TemporaryDirectory() as raw:
            root=Path(raw);fd=sandbox.directory(root)
            previous=os.umask(0o077)
            try:
                source=sandbox.public_parents(fd,"source")
                try:
                    sandbox.host("git","init","--template=","--quiet",str(root/"source"),umask=0o022)
                    self.assertEqual(os.umask(0o077),0o077,"subprocess changed parent umask")
                    sandbox.check_git_metadata(source)
                finally: os.close(source)
            finally: os.umask(previous);os.close(fd)

    def test_permission_frame_requires_exact_fixed_rows_and_consistent_result(self):
        row={"index":0,"role":"source","accessible":True,"mode_ok":True,"errno":0}
        result={"schema":1,"status":"passed","rows":[row]}
        raw=lambda value:b"POSITION_LAUNCH_ACCESS "+json.dumps(value).encode()
        self.assertEqual(sandbox.launch_permission_output(raw(result),["source"]),result)
        denied=copy.deepcopy(result);denied["status"]="refused";denied["rows"][0].update(accessible=False,errno=errno.EACCES)
        self.assertEqual(sandbox.launch_permission_output(raw(denied),["source"]),denied)
        unknown=copy.deepcopy(denied);unknown["rows"][0]["errno"]=None
        self.assertEqual(sandbox.launch_permission_output(raw(unknown),["source"]),unknown)
        variants=[]
        for key,value in (("index",1),("role","arbitrary"),("accessible",1),("mode_ok","true"),("errno",True),("errno",4096),("errno",None),("unknown","dummy-private-error")):
            variant=copy.deepcopy(result);variant["rows"][0][key]=value;variants.append(variant)
        variants += [dict(result,rows=[]),dict(result,rows=[row,row]),dict(result,status="refused"),dict(result,unknown="dummy-private-error"),dict(result,schema=True)]
        false_zero=copy.deepcopy(denied);false_zero["rows"][0]["errno"]=0;variants.append(false_zero)
        false_success=copy.deepcopy(unknown);false_success["status"]="passed";variants.append(false_success)
        for variant in variants:
            with self.assertRaises(ValueError): sandbox.launch_permission_output(raw(variant),["source"])
        for value in (b"",b"dummy-private-error",raw(result)+b"\nextra",b"x"*16385):
            with self.assertRaises(ValueError): sandbox.launch_permission_output(value,["source"])


class ProbeObservations(unittest.TestCase):
    def test_fixed_refusal_stages_and_candidate_record_are_not_kernel_success(self):
        for stage in sorted(sandbox.PROBE_STAGES):
            for refusal in sorted(sandbox.PROBE_REFUSALS):
                raw=sandbox.PROBE_ENTRY+b"\nPOSITION_PROBE_REFUSAL v1 stage "+stage.encode()+b" "+refusal.encode()+b"\n"
                observation,payload=sandbox.probe_output(1,raw,len(raw)+100)
                self.assertIsNone(payload)
                self.assertEqual(observation,{"schema":1,"supervisor_exit":1,"combined_output_bytes":len(raw)+100,
                    "inner_entry":True,"framing":"refusal","stage":stage,"refusal":refusal,"launcher_refusal":"unknown"})
        raw=sandbox.PROBE_ENTRY+b'\n{"pid": 1}\n'
        observation,payload=sandbox.probe_output(0,raw,len(raw)+100)
        self.assertEqual(payload,b'{"pid": 1}')
        self.assertEqual(observation["framing"],"candidate-record")
        self.assertEqual(observation["stage"],"unknown")
        self.assertNotIn("passed",json.dumps(observation))

    def test_malformed_unknown_oversized_or_contradictory_probe_output_refuses(self):
        entry=sandbox.PROBE_ENTRY+b"\n"
        frame=b"POSITION_PROBE_REFUSAL v1 stage environment assertion\n"
        secret=b"dummy-unknown-environment-value"
        for code,raw in ((1,b""),(1,secret),(1,frame),(1,entry+frame.replace(b"environment",secret)),
                         (1,entry+frame.replace(b"assertion",b"arbitrary")),(1,entry+frame+frame),
                         (1,entry+entry+frame),(1,entry+frame+secret),(1,entry+b'{}\n'),
                         (0,entry+frame),(0,entry+b'[]\n'),(0,entry+secret),(0,entry+b'{}\n'+secret),
                         (1,entry+frame.replace(b"assertion",b"\xff")),(1,b"x"*65537)):
            with self.subTest(code=code,bytes=len(raw)):
                observation,payload=sandbox.probe_output(code,raw,len(raw))
                self.assertIsNone(payload)
                self.assertNotIn(secret.decode(),json.dumps(observation))
                self.assertEqual(observation["stage"],"unknown")
        for code,raw,count in ((True,b"",0),(256,b"",0),(1,b"value",0),(1,b"",sandbox.MAX_OUTPUT+65537)):
            with self.assertRaises(ValueError): sandbox.probe_output(code,raw,count)

    def test_only_verified_literal_launcher_markers_are_projected(self):
        secret=b"dummy-private-error-text"
        for marker,expected in sandbox.LAUNCHER_REFUSALS.items():
            raw=marker+b"\n"+secret
            observed=sandbox.launch_observation(1,raw)
            self.assertEqual(observed,{"schema":1,"supervisor_exit":1,"combined_output_bytes":len(raw),"launcher_refusal":expected})
            self.assertNotIn(secret.decode(),json.dumps(observed))
            self.assertNotIn(marker.decode(),json.dumps(observed))
            for modified in (b"prefix "+marker,marker+b" suffix",b"x"*4096+b"\n"+marker,
                             b"\n"*(4096-len(marker))+marker+b" hidden suffix"):
                self.assertEqual(sandbox.launch_observation(1,modified)["launcher_refusal"],"unknown")
            self.assertEqual(sandbox.launch_observation(0,marker)["launcher_refusal"],"unknown")
        mixed=b"\n".join(sandbox.LAUNCHER_REFUSALS)
        self.assertEqual(sandbox.launch_observation(1,mixed)["launcher_refusal"],"unknown")
        self.assertEqual(sandbox.launch_observation(1,secret)["launcher_refusal"],"unknown")
        for code in (None,True,256):
            with self.assertRaises(ValueError): sandbox.launch_observation(code,secret)
        observation,payload=sandbox.probe_output(1,b"prlimit: failed to execute python3: Permission denied\n",100)
        self.assertIsNone(payload)
        self.assertFalse(observation["inner_entry"])
        self.assertEqual(observation["launcher_refusal"],"prlimit-exec-refused")


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

    def test_dispatch_control_requires_named_run_receipt_and_no_skip(self):
        package,test=proof.DISPATCH_PACKAGE,proof.DISPATCH_TEST
        rows=[{"Action":"start","Package":package},{"Action":"run","Package":package,"Test":test},
              {"Action":"output","Package":package,"Test":test,"Output":"owned dispatch scope receipts: groups=2 deliveries=3,3 effects=2,2 repeated_frame_sha256="+"a"*64},
              {"Action":"pass","Package":package,"Test":test},{"Action":"pass","Package":package}]
        raw=lambda value:("\n".join(map(json.dumps,value))+"\n").encode()
        self.assertEqual(proof.dispatch_control_result(raw(rows))["deliveries"],[3,3])
        variants=[rows[:-1],rows[:2]+rows[3:],rows+[{"Action":"skip","Package":package,"Test":test}],rows+[rows[2]]]
        for variant in variants:
            with self.assertRaises(ValueError): proof.dispatch_control_result(raw(variant))
        negative=copy.deepcopy(rows)
        witness="    consumer_dispatch_scope_integration_test.go:170: owned dispatch scope refusal witness: deliveries=1,1 effects=1,0 marker_deliveries=0,0 marker_effects=0,0\n"
        reason="    consumer_dispatch_scope_integration_test.go:172: independent durable group dispatch missing\n"
        negative[2]["Output"]=witness+reason
        negative[3]["Action"]=negative[4]["Action"]="fail"
        self.assertEqual(proof.dispatch_control_result(raw(negative),mutant=True)["status"],"expected failure")
        alternate=copy.deepcopy(negative);alternate[2]["Output"]=witness.replace("effects=1,0 ","effects=0,1 ")+reason
        self.assertEqual(proof.dispatch_control_result(raw(alternate),mutant=True)["witness"]["effects"],[0,1])
        for refused in (reason,witness.replace("deliveries=1,1","deliveries=1,0")+reason,
                        witness.replace("effects=1,0 ","effects=0,0 ")+reason,witness.replace("effects=1,0 ","effects=1,1 ")+reason,
                        witness.replace("marker_deliveries=0,0","marker_deliveries=1,0")+reason,
                        witness.replace("marker_effects=0,0","marker_effects=0,1")+reason):
            variant=copy.deepcopy(negative);variant[2]["Output"]=refused
            with self.assertRaises(ValueError): proof.dispatch_control_result(raw(variant),mutant=True)
        for variant in (negative[:1]+negative[2:],negative[:-2]+negative[-1:],rows):
            with self.assertRaises(ValueError): proof.dispatch_control_result(raw(variant),mutant=True)
        negative[2]["Output"]="unrelated failure"
        with self.assertRaises(ValueError): proof.dispatch_control_result(raw(negative),mutant=True)

    def test_dispatch_units_require_all_six_roots_and_twenty_subcases(self):
        package=proof.DISPATCH_PACKAGE
        self.assertEqual(len(proof.DISPATCH_UNIT_ROOTS),6)
        self.assertEqual(len(proof.DISPATCH_UNIT_TESTS),26)
        rows=[{"Action":"start","Package":package}]
        for name in sorted(proof.DISPATCH_UNIT_TESTS):
            rows += [{"Action":"run","Package":package,"Test":name},{"Action":"pass","Package":package,"Test":name}]
        rows.append({"Action":"pass","Package":package})
        raw=lambda value:("\n".join(map(json.dumps,value))+"\n").encode()
        self.assertEqual(proof.dispatch_unit_result(raw(rows))["passed"],26)
        skipped=copy.deepcopy(rows);skipped[2]["Action"]="skip"
        unknown=copy.deepcopy(rows);unknown[1]["Test"]=unknown[2]["Test"]="TestUnknownDispatch"
        failed=copy.deepcopy(rows);failed[2]["Action"]="fail"
        cached=rows[:-1]+[{"Action":"output","Package":package,"Output":"ok pkg (cached)"}]+rows[-1:]
        for variant in (rows[:-1],rows[:1]+rows[3:],rows+[rows[2]],skipped,unknown,failed,cached):
            with self.assertRaises(ValueError): proof.dispatch_unit_result(raw(variant))

    def test_scope_uses_source_paths_and_preserves_existing_owners(self):
        for path in (b"kanz/internal/new/unknown_test.go",b"kanz/internal/bustest/reclaim.go",b"kanz/test/backing/position_reclaim/main.go",b".github/scripts/position_reclaim_proof.py",b".github/workflows/kanz-ci.yml"):
            self.assertTrue(required_ci.position_reclaim_path(path))
            self.assertTrue(required_ci.application_path(path))
        self.assertFalse(required_ci.position_reclaim_path(b"docs/readme.md"))
        for path in (b"kanz/services/oms/internal/position/projector.go",b"kanz/pkg/bus/frame.go",b"kanz/pkg/bus/consumer.go",b"kanz/pkg/bus/consumer_dispatch_scope_test.go",b"kanz/pkg/bus/consumer_dispatch_scope_integration_test.go",b"kanz/internal/platform/subject/topic.go",b"rust/crates/oms_store/src/position.rs",b"kanz/infra/nats/bootstrap-job.yaml"):
            self.assertTrue(required_ci.position_reclaim_path(path))

    def test_owned_broker_reset_requires_fresh_storage_and_distinct_identity(self):
        before={"container":"a"*64,"image":"reviewed-image","started":"first","restart_count":0,"name":"/owned"}
        for problem in (None,"nonempty","same-container","restarted","other-image"):
            with self.subTest(problem=problem),tempfile.TemporaryDirectory() as raw:
                candidate=object.__new__(proof.Proof)
                candidate.data=Path(raw);candidate.data_mounted=True
                candidate.negative_id=before["container"];candidate.negative_receipts=[]
                after={**before,"container":"b"*64,"started":"second"}
                if problem=="nonempty": (candidate.data/"stale-history").write_bytes(b"old fixture")
                elif problem=="same-container": after["container"]=before["container"]
                elif problem=="restarted": after["restart_count"]=1
                elif problem=="other-image": after["image"]="different-image"
                calls=[]
                def remove():
                    calls.append("remove");candidate.negative_id=None
                def create():
                    calls.append("create");candidate.negative_id=after["container"]
                def host(*args):
                    calls.append(args)
                    if args[0]=="mount": self.assertFalse(candidate.data_mounted)
                candidate.negative_remove=remove;candidate.negative_create=create
                with mock.patch.object(proof,"host",side_effect=host),mock.patch.object(proof,"docker_identity",side_effect=[before,after]) as identity:
                    if problem is not None:
                        with self.assertRaises(ValueError): candidate.negative_fresh()
                        self.assertEqual(candidate.negative_receipts,[])
                    else:
                        result=candidate.negative_fresh()
                        self.assertEqual(result,{"before":before,"after":after,"fresh_storage_empty_before_start":True})
                        self.assertTrue(candidate.data_mounted)
                        self.assertEqual(calls,["remove",("umount",str(candidate.data)),("mount","-t","tmpfs","-o","size=128M,nodev,nosuid,noexec,mode=0700,uid=65534,gid=65534","position-negative",str(candidate.data)),"create"])
                        self.assertEqual(identity.call_args_list,[mock.call(before["container"]),mock.call(after["container"])])
                if problem=="nonempty": self.assertNotIn("create",calls)

    def test_workflow_cannot_drop_or_unconditionally_disable_proof(self):
        text=Path(".github/workflows/kanz-ci.yml").read_text()
        required_ci.workflow_jobs(text)
        self.assertIn("path: ${{ runner.temp }}/position-reclaim-published-${{ github.run_id }}-${{ github.run_attempt }}/",text)
        self.assertNotIn("position-reclaim-*/artifacts/",text)
        for mutated in (text.replace("python3 -B ../.github/scripts/position_reclaim_proof.py","true # removed proof"),text.replace('&& [ "$POSITION_RECLAIM_SCOPE" = "true" ]','&& [ "$POSITION_RECLAIM_SCOPE" = "never" ]')):
            with self.assertRaises(ValueError): required_ci.workflow_jobs(mutated)


class FixedLoopbackRelay(unittest.TestCase):
    def test_exact_seven_translations_preserve_canonical_inner_ports(self):
        self.assertEqual(sandbox.RELAY_PORTS,((24222,4222),(24223,4223),(24224,4224),(24225,4225),(25432,5432),(26379,6379),(39092,9092)))
        outer,backend=zip(*sandbox.RELAY_PORTS)
        self.assertEqual(backend,sandbox.PORTS)
        self.assertEqual(len(set(outer)),7)
        self.assertFalse(set(outer)&set(backend))
        self.assertNotIn(29092,outer)

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
        listener=socket.socket()
        listener.bind(("127.0.0.1",0))
        transit=listener.getsockname()[1]
        listener.close()
        relay=sandbox.FixedRelay("127.0.0.1","127.0.0.2",((transit,port),))
        try:
            with socket.create_connection(("127.0.0.1",transit),timeout=2) as client:
                client.sendall(b"canonical bytes")
                self.assertEqual(client.recv(64),b"canonical bytes")
        finally:
            relay.close()
            thread.join(3)
        self.assertFalse(thread.is_alive())

    def test_later_occupied_port_closes_partial_constructor_and_preserves_error(self):
        original_socket=socket.socket
        original_close=sandbox.FixedRelay.close
        with original_socket() as occupied,original_socket() as available:
            occupied.bind(("127.0.0.1",0));occupied.listen(1)
            available.bind(("127.0.0.1",0))
            first,later=available.getsockname()[1],occupied.getsockname()[1]
            available.close()
            for cleanup_refusal in (False,True):
                created=[]
                def create(*args,**kwargs):
                    result=original_socket(*args,**kwargs);created.append(result);return result
                def close(relay):
                    original_close(relay)
                    if cleanup_refusal: raise ValueError("separate cleanup refusal")
                with self.subTest(cleanup_refusal=cleanup_refusal),mock.patch.object(sandbox.socket,"socket",side_effect=create),mock.patch.object(sandbox.FixedRelay,"close",close),mock.patch.object(sandbox.threading.Thread,"start") as started:
                    with self.assertRaises(OSError) as caught:
                        sandbox.FixedRelay("127.0.0.1","127.0.0.2",((first,1),(later,2)))
                    self.assertEqual(caught.exception.errno,errno.EADDRINUSE)
                    self.assertEqual(bool(getattr(caught.exception,"__notes__",[])),cleanup_refusal)
                    self.assertEqual(getattr(caught.exception,"position_relay_cleanup_refused",False),cleanup_refusal)
                    started.assert_not_called()
                    self.assertTrue(created)
                    self.assertTrue(all(s.fileno()==-1 for s in created))
                with original_socket() as reclaimed: reclaimed.bind(("127.0.0.1",first))
                self.assertGreaterEqual(occupied.fileno(),0,"unowned listener was closed")

    def test_constructor_cleanup_refusal_stays_unqualified_through_publication(self):
        original_close=sandbox.FixedRelay.close
        with tempfile.TemporaryDirectory() as raw,socket.socket() as occupied,socket.socket() as available:
            root=Path(raw);boundary=root/"boundary";boundary.mkdir()
            repo=root/"empty-repo";repo.mkdir()
            occupied.bind(("127.0.0.1",0));occupied.listen(1)
            available.bind(("127.0.0.1",0))
            routes=((available.getsockname()[1],1),(occupied.getsockname()[1],2))
            available.close()
            # Model privileged setup without invoking any host provisioning.
            # The real relay still hits an owned loopback-port collision.
            with mock.patch.object(sandbox.os,"geteuid",return_value=0):
                box=sandbox.Sandbox(repo,boundary,[],{})
            box.host_ip="127.0.0.1"
            def host(*args,**kwargs):
                if args[:2]==("git","init"):
                    (box.inputs/"source/.git").mkdir()
                elif args[0]=="git" and "fetch" in args:
                    (box.inputs/"source/.git/FETCH_HEAD").write_bytes(b"fixture input")
                return b""
            def refuse(relay):
                original_close(relay)
                raise ValueError("constructor cleanup observation refused")
            try:
                with mock.patch.object(sandbox,"host",side_effect=host),mock.patch.object(sandbox.shutil,"which",return_value="/reviewed/tool"),mock.patch.object(sandbox,"RELAY_PORTS",routes),mock.patch.object(sandbox.FixedRelay,"close",refuse):
                    with self.assertRaises(OSError) as caught: box.start()
                self.assertEqual(caught.exception.errno,errno.EADDRINUSE)
                self.assertTrue(getattr(caught.exception,"position_relay_cleanup_refused",False))
                self.assertTrue(box.relay_constructor_cleanup_refused)
                self.assertIsNone(box.relay,"failed constructor was adopted as an owned relay")
                with mock.patch.object(sandbox,"host",return_value=b""),mock.patch.object(sandbox.subprocess,"run",return_value=types.SimpleNamespace(returncode=1)):
                    with self.assertRaisesRegex(ValueError,"fixed_relay_constructor_cleanup_refused"): box.close()
                cleanup=json.loads(sandbox.read_regular(box.receipts_fd,"sandbox-cleanup.json",16384))
                self.assertTrue(cleanup["relay_constructor_cleanup_refused"])
                self.assertIn("fixed_relay_constructor_cleanup_refused",cleanup["errors"])
                self.assertFalse(box.cleanup_verified)
                candidate=object.__new__(proof.Proof);candidate.box=box
                with self.assertRaisesRegex(ValueError,"observed complete sandbox teardown"): candidate.publish(complete=False)
                self.assertGreaterEqual(occupied.fileno(),0)
                with socket.socket() as reclaimed: reclaimed.bind(("127.0.0.1",routes[0][0]))
            finally:
                for name in ("work_fd","out_fd","receipts_fd"):
                    descriptor=getattr(box,name,None)
                    if descriptor is not None: os.close(descriptor);setattr(box,name,None)

    def test_worker_start_failure_joins_already_started_acceptor(self):
        original_socket=socket.socket
        original_start=threading.Thread.start
        created=[];started=[]
        with original_socket() as first,original_socket() as second:
            for s in (first,second): s.bind(("127.0.0.1",0))
            ports=(first.getsockname()[1],second.getsockname()[1])
            first.close();second.close()
            def create(*args,**kwargs):
                result=original_socket(*args,**kwargs);created.append(result);return result
            def start(thread):
                if started: raise RuntimeError("second acceptor start refused")
                original_start(thread);started.append(thread)
            with mock.patch.object(sandbox.socket,"socket",side_effect=create),mock.patch.object(sandbox.threading.Thread,"start",start):
                with self.assertRaisesRegex(RuntimeError,"second acceptor start refused"):
                    sandbox.FixedRelay("127.0.0.1","127.0.0.2",tuple((p,1) for p in ports))
            self.assertEqual(len(started),1)
            self.assertFalse(started[0].is_alive())
            self.assertTrue(all(s.fileno()==-1 for s in created))


class PublishedReceipts(unittest.TestCase):
    def candidate(self,root):
        private=root/"private";private.mkdir(mode=0o700)
        retained=private/"artifacts";retained.mkdir(mode=0o700)
        publication=root/"position-reclaim-published-1-1";publication.mkdir(mode=0o700)
        candidate=object.__new__(proof.Proof)
        candidate.source={"sha":"a"*40,"tree":"b"*40,"run":"1","attempt":"1"}
        candidate.publication=publication
        candidate.publication_fd=sandbox.directory(publication)
        candidate.box=types.SimpleNamespace(cleanup_verified=True,receipts_fd=sandbox.directory(retained),artifacts={})
        for name,raw in (("passes-complete.json",b"two passes and controls"),("receipt.json",b"bounded receipt"),("oms-store",b"private executable")):
            sandbox.new_file(candidate.box.receipts_fd,name,raw)
            candidate.box.artifacts[name]={"size":len(raw),"sha256":proof.digest(raw)}
        private.chmod(0o711)
        return candidate,private,retained

    def test_literal_sibling_is_listable_after_teardown_and_excludes_binaries(self):
        with tempfile.TemporaryDirectory() as raw:
            candidate,private,retained=self.candidate(Path(raw))
            try:
                # The sandbox unit owns its fixtures. Model the upload runner's
                # execute-only access to a root-owned 0711 private directory.
                private.chmod(0o111)
                with self.assertRaises(PermissionError): list(os.scandir(private))
                private.chmod(0o711)
                previous=os.umask(0o777)
                try: candidate.publish(complete=True)
                finally: os.umask(previous)
                with os.scandir(candidate.publication) as entries: names={p.name for p in entries}
                self.assertEqual(names,{"receipt.json","passes-complete.json","complete.json"})
                self.assertEqual(stat.S_IMODE(private.stat().st_mode),0o711)
                self.assertEqual(stat.S_IMODE(retained.stat().st_mode),0o700)
                self.assertEqual(stat.S_IMODE(candidate.publication.stat().st_mode),0o755)
                for name in names: self.assertEqual(stat.S_IMODE((candidate.publication/name).stat().st_mode),0o644)
                complete=json.loads((candidate.publication/"complete.json").read_bytes())
                self.assertEqual(complete["cleanup"],"observed-complete")
                self.assertEqual(set(complete["artifacts_sha256"]),names-{"complete.json"})
            finally: candidate.close_receipts()

    def test_unproven_cleanup_changed_linked_or_undeclared_data_refuses_publication(self):
        for problem in ("cleanup","changed","linked","undeclared"):
            with self.subTest(problem=problem),tempfile.TemporaryDirectory() as raw:
                candidate,_,retained=self.candidate(Path(raw))
                try:
                    if problem=="cleanup": candidate.box.cleanup_verified=False
                    elif problem=="changed": (retained/"passes-complete.json").write_bytes(b"changed bytes")
                    elif problem=="linked":
                        (retained/"passes-complete.json").unlink()
                        os.symlink("receipt.json",retained/"passes-complete.json")
                    else: sandbox.new_file(candidate.box.receipts_fd,"unregistered.json",b"unregistered")
                    with self.assertRaises((OSError,ValueError)): candidate.publish(complete=True)
                    self.assertEqual(stat.S_IMODE(candidate.publication.stat().st_mode),0o700)
                    self.assertFalse((candidate.publication/"complete.json").exists())
                finally: candidate.close_receipts()

    def test_failure_receipts_never_gain_a_completion_manifest(self):
        with tempfile.TemporaryDirectory() as raw:
            candidate,_,_=self.candidate(Path(raw))
            try:
                candidate.publish(complete=False)
                self.assertFalse((candidate.publication/"complete.json").exists())
                self.assertTrue((candidate.publication/"receipt.json").is_file())
            finally: candidate.close_receipts()


class CleanupRefusals(unittest.TestCase):
    def test_log_refusal_does_not_skip_descriptors_or_mount_cleanup(self):
        with tempfile.TemporaryDirectory() as raw:
            root=Path(raw)
            work=root/"scratch";work.mkdir()
            public=root/"public";public.mkdir()
            box=object.__new__(sandbox.Sandbox)
            box.active=set();box.inner=None;box.relay=None
            box.relay_constructor_cleanup_refused=False
            box.veth_created=False;box.net_created=False
            box.namespace="owned-fixture";box.interface="owned-veth"
            box.work=work;box.work_fd=sandbox.directory(work)
            box.out_fd=sandbox.directory(root)
            box.public_mounts={"fixture":public};box.mounted=True
            box.executions=[]
            box.retained=0;box.artifacts={}
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
