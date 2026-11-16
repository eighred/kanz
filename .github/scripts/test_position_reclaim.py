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
from go_test_evidence import INTEGER_BROWSER_PROOF, INTEGER_BROWSER_TEST


def modeled_capsule(path):
    """Old relay/publication controls model provisioning inside their scratch."""
    capsule=object.__new__(sandbox.VisibleInputCapsule)
    capsule.path=path;capsule.created=True;capsule.absent=None
    capsule.allocate=lambda:path.chmod(0o711)
    capsule.verify=lambda:None
    def retire(*,resources_absent):
        if resources_absent is not True: raise ValueError("modeled resource teardown unproven")
        capsule.absent=True
    capsule.retire=retire
    capsule.metadata=lambda:{"schema":1,"created":True,"absence_observed":capsule.absent}
    capsule.release=lambda:None
    return capsule


class CapsuleModel:
    """Small syscall model: no /run allocation or host provisioning in units."""
    def __init__(self):
        self.nodes={"/":[0o755,0,0,1],"/run":[0o755,0,0,2]}
        self.fds={};self.opened={};self.calls=[];self.next_fd=10

    def path(self, path, dir_fd=None):
        return str(Path(self.fds[dir_fd])/path) if dir_fd is not None else str(path)

    def info(self, path):
        return self.node_info(self.nodes[path])

    def node_info(self, node):
        mode,uid,gid,inode=node
        return types.SimpleNamespace(st_mode=stat.S_IFDIR|mode,st_uid=uid,st_gid=gid,st_dev=1,st_ino=inode)

    def open(self, path, flags, *, dir_fd=None):
        self.calls.append(("open",str(path),flags))
        if flags & (os.O_NOFOLLOW|os.O_DIRECTORY|os.O_CLOEXEC)!=(os.O_NOFOLLOW|os.O_DIRECTORY|os.O_CLOEXEC):
            raise AssertionError("descriptor safety flags missing")
        name=self.path(path,dir_fd)
        if name not in self.nodes: raise FileNotFoundError()
        if self.nodes[name][0]&stat.S_IFLNK: raise OSError(errno.ELOOP,"modeled symlink")
        fd=self.next_fd;self.next_fd+=1;self.fds[fd]=name;self.opened[fd]=self.nodes[name]
        return fd

    def mkdir(self, name, *, mode, dir_fd):
        path=self.path(name,dir_fd);self.calls.append(("mkdir",path))
        if path in self.nodes: raise FileExistsError()
        self.nodes[path]=[mode,0,0,len(self.nodes)+1]

    def fchmod(self, fd, mode):
        path=self.fds[fd];self.calls.append(("fchmod",path,mode))
        self.opened[fd][0]=mode

    def close(self, fd):
        self.fds.pop(fd);self.opened.pop(fd)

    def stat(self, path, *, dir_fd, follow_symlinks):
        if follow_symlinks is not False: raise AssertionError("followed modeled entry")
        name=self.path(path,dir_fd)
        if name not in self.nodes: raise FileNotFoundError()
        return self.info(name)

    def children(self, fd):
        parent=self.fds[fd]
        return [Path(p).name for p in self.nodes if str(Path(p).parent)==parent and p!=parent]

    def remove(self, name, *, dir_fd):
        path=self.path(name,dir_fd);self.calls.append(("remove",path))
        del self.nodes[path]

    def rmdir(self, name, *, dir_fd):
        path=self.path(name,dir_fd)
        if any(str(Path(p).parent)==path and p!=path for p in self.nodes):
            raise OSError(errno.ENOTEMPTY,"modeled nonempty directory")
        self.remove(name,dir_fd=dir_fd)

    def patches(self):
        remove=lambda name,dir_fd:self.remove(name,dir_fd=dir_fd)
        remove.avoids_symlink_attacks=True
        return (mock.patch.multiple(sandbox.os,geteuid=lambda:0,open=self.open,
                    fstat=lambda fd:self.node_info(self.opened[fd]),mkdir=self.mkdir,fchmod=self.fchmod,
                    stat=self.stat,listdir=self.children,rmdir=self.rmdir,close=self.close),
                mock.patch.object(sandbox.shutil,"rmtree",remove))


class VisibleInputs(unittest.TestCase):
    def test_declared_capsule_path_cannot_be_overridden_before_allocation(self):
        for field,value in (("path",Path("/tmp/alternate")),("name","../alternate"),("nonce","invalid")):
            fs=CapsuleModel();capsule=sandbox.VisibleInputCapsule();setattr(capsule,field,value)
            os_patch,remove_patch=fs.patches()
            with os_patch,remove_patch:
                with self.assertRaises(ValueError): capsule.allocate()
                self.assertEqual(fs.calls,[],"an alternate input path was touched")

    def test_one_initial_capsule_has_fixed_ancestry_nonce_and_observed_retirement(self):
        fs=CapsuleModel();capsule=sandbox.VisibleInputCapsule()
        os_patch,remove_patch=fs.patches()
        with os_patch,remove_patch:
            capsule.allocate();capsule.verify()
            self.assertEqual(capsule.path,Path("/run")/("position-reclaim-inputs-"+capsule.nonce))
            self.assertEqual(len([c for c in fs.calls if c[0]=="mkdir"]),1)
            fs.nodes[str(capsule.path/"inputs")]=(0o711,0,0,4)
            fs.nodes[str(capsule.path/"scratch")]=(0o700,0,0,5)
            metadata=capsule.metadata()
            self.assertEqual([r["role"] for r in metadata["ancestors"]],["filesystem-root","runtime-parent"])
            self.assertNotIn("/",json.dumps(metadata))
            capsule.retire(resources_absent=True)
            self.assertTrue(capsule.absent)
            self.assertEqual(set(fs.nodes),{"/","/run"})
            self.assertEqual(fs.fds,{})
            self.assertTrue(all(c[1]==str(capsule.path) for c in fs.calls if c[0]=="fchmod"),"ancestor mode was changed")

    def test_bad_ancestor_owner_mode_or_symlink_refuses_before_creation(self):
        for path,value in (("/",(0o777,0,0,1)),("/run",(0o755,1000,0,2)),
                           ("/run",(0o755,0,1000,2)),("/run",(stat.S_IFLNK|0o755,0,0,2))):
            with self.subTest(path=path,value=value):
                fs=CapsuleModel();fs.nodes[path]=value;capsule=sandbox.VisibleInputCapsule()
                os_patch,remove_patch=fs.patches()
                with os_patch,remove_patch:
                    with self.assertRaises((ValueError,OSError)): capsule.allocate()
                    self.assertFalse(capsule.created)
                    self.assertFalse(any(c[0] in ("mkdir","fchmod") for c in fs.calls))
                    capsule.release()

    def test_existing_or_replaced_capsule_is_never_adopted_or_removed(self):
        for collision in (True,False):
            fs=CapsuleModel();capsule=sandbox.VisibleInputCapsule()
            os_patch,remove_patch=fs.patches()
            with os_patch,remove_patch:
                if collision:
                    fs.nodes[str(capsule.path)]=(0o711,1000,1000,99)
                    with self.assertRaises(FileExistsError): capsule.allocate()
                    capsule.retire(resources_absent=True)
                else:
                    capsule.allocate()
                    fs.nodes[str(capsule.path)]=(0o711,0,0,99)
                    with self.assertRaises(ValueError): capsule.retire(resources_absent=True)
                    capsule.release()
                self.assertIn(str(capsule.path),fs.nodes)
                self.assertFalse(any(c[0]=="remove" for c in fs.calls))

    def test_failed_initial_mode_setup_retires_only_owned_empty_capsule(self):
        fs=CapsuleModel();capsule=sandbox.VisibleInputCapsule()
        os_patch,remove_patch=fs.patches()
        with os_patch,remove_patch:
            with mock.patch.object(sandbox.os,"fchmod",side_effect=PermissionError(errno.EPERM,"modeled mode refusal")):
                with self.assertRaises(PermissionError): capsule.allocate()
            self.assertEqual(capsule.stage,"exclusive-allocation")
            capsule.retire(resources_absent=True)
            self.assertEqual(set(fs.nodes),{"/","/run"})
            self.assertTrue(capsule.absent)
            self.assertEqual(fs.fds,{})

    def test_live_resources_or_private_child_refuse_retirement(self):
        fs=CapsuleModel();capsule=sandbox.VisibleInputCapsule()
        os_patch,remove_patch=fs.patches()
        with os_patch,remove_patch:
            capsule.allocate()
            with self.assertRaises(ValueError): capsule.retire(resources_absent=False)
            fs.nodes[str(capsule.path/"artifacts")]=(0o700,0,0,4)
            with self.assertRaises(ValueError): capsule.retire(resources_absent=True)
            self.assertFalse(any(c[0]=="remove" for c in fs.calls))
            capsule.release()

    def test_nonce_parent_and_leaf_custody_changes_refuse_before_removal(self):
        for change in ("nonce","parent","owner","child-link"):
            with self.subTest(change=change):
                fs=CapsuleModel();capsule=sandbox.VisibleInputCapsule()
                os_patch,remove_patch=fs.patches()
                with os_patch,remove_patch:
                    capsule.allocate()
                    if change=="nonce": capsule.nonce="0"*32
                    elif change=="parent": fs.nodes["/run"]=(0o755,0,0,99)
                    elif change=="owner": fs.nodes[str(capsule.path)]=(0o711,1000,0,3)
                    else: fs.nodes[str(capsule.path/"inputs")]=(stat.S_IFLNK|0o755,0,0,4)
                    with self.assertRaises(ValueError): capsule.retire(resources_absent=True)
                    self.assertFalse(any(c[0]=="remove" for c in fs.calls))
                    capsule.release()

    def test_private_artifact_tree_cannot_become_a_visible_input_or_mount(self):
        capsule=sandbox.VisibleInputCapsule()
        with mock.patch.object(sandbox.os,"geteuid",return_value=0):
            with self.assertRaises(ValueError):
                sandbox.Sandbox(Path("/repo"),capsule=capsule,artifact_root=capsule.path/"private",tools=[],env={})
        box=object.__new__(sandbox.Sandbox);box.artifact_root=Path("/private");box.out=box.artifact_root/"artifacts"
        box.artifact_root_canonical=box.artifact_root
        for path in (box.artifact_root,box.out,box.out/"receipt.json"):
            with self.assertRaises(ValueError): box.approved_mount(path,"/input")
        box.approved_mount(box.out/"oms-store","/fixture/oms-store")
        box.approved_mount(box.out/"seed.json","/receipt/seed.json")


class ProtectedFiles(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root=Path(self.temp.name)
        self.fd=sandbox.directory(self.root)
        self.addCleanup(os.close,self.fd)

    def test_rejected_file_metadata_distinguishes_link_type_and_size_without_promoting_bytes(self):
        (self.root/"source").write_bytes(b"data")
        os.link(self.root/"source",self.root/"alias")
        os.mkfifo(self.root/"pipe")
        (self.root/"oversized").write_bytes(b"data!")
        for name,regular,links,size,reason in (("source",True,2,4,"multiple-links"),
                                              ("pipe",False,1,0,"nonregular"),
                                              ("oversized",True,1,5,"oversized")):
            with self.subTest(reason=reason),self.assertRaises(sandbox.EvidenceFileRefusal) as caught:
                sandbox.read_regular(self.fd,name,4)
            self.assertEqual(caught.exception.observation,{"regular":regular,"link_count":links,"bytes":size,"limit_bytes":4,"reason":reason})
        box=object.__new__(sandbox.Sandbox);box.work_fd=self.fd
        retained={}
        box.retain=lambda name,raw:retained.update({name:json.loads(raw)})
        with self.assertRaises(sandbox.EvidenceFileRefusal):
            box.promote("source","oms-store",4)
        self.assertEqual(retained,{"promotion-refusal.json":{"schema":1,"role":"rust-store-executable",
            "regular":True,"link_count":2,"bytes":4,"limit_bytes":4,"reason":"multiple-links"}})
        self.assertNotIn(str(self.root),json.dumps(retained))

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


class RustStoreExport(unittest.TestCase):
    def model(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        root.chmod(0o700)
        examples = root/"rust-target/debug/examples"
        examples.mkdir(parents=True, mode=0o700)
        fixture = examples/"fixture"
        fixture.write_bytes(b"source-free dummy fixture bytes")
        fixture.chmod(0o755)
        os.link(fixture, examples/"fixture-0123456789abcdef")
        for name in ("fixture.d", "fixture-0123456789abcdef.d"):
            (examples/name).write_bytes(b"fixed dummy dependency record")
            (examples/name).chmod(0o644)
        fd = sandbox.directory(root)
        self.addCleanup(os.close, fd)
        return root, fd, examples

    def test_two_live_aliases_export_independent_one_link_bytes(self):
        root, fd, examples = self.model()
        original = (examples/"fixture").read_bytes()
        previous = os.umask(0o077)
        try: receipt = sandbox.export_rust_store(fd)
        finally: os.umask(previous)
        exported = root/"rust-store-export/fixture"
        self.assertEqual(receipt["entry_count"], 4)
        self.assertEqual(receipt["alias_count"], 2)
        self.assertEqual(receipt["source"]["links"], 2)
        self.assertEqual(exported.stat().st_nlink, 1)
        self.assertNotEqual(exported.stat().st_ino, (examples/"fixture").stat().st_ino)
        self.assertEqual(exported.read_bytes(), original)
        self.assertEqual(stat.S_IMODE(exported.stat().st_mode), 0o555)
        self.assertEqual(stat.S_IMODE(exported.parent.stat().st_mode), 0o700)
        self.assertEqual(sandbox.validate_store_export(json.dumps(receipt).encode(), original, exported.stat()), receipt)
        (examples/"fixture-0123456789abcdef").write_bytes(b"old alias changes after export")
        self.assertNotEqual((examples/"fixture").read_bytes(), original)
        self.assertEqual(exported.read_bytes(), original)
        self.assertEqual(exported.stat().st_nlink, 1)
        self.assertNotIn(str(root), json.dumps(receipt))

    def test_outside_second_link_and_third_link_refuse_without_source_unlink(self):
        for problem in ("outside", "third"):
            with self.subTest(problem=problem):
                root, fd, examples = self.model()
                if problem == "outside":
                    (examples/"fixture-0123456789abcdef").unlink()
                    os.link(examples/"fixture", root/"outside")
                else:
                    os.link(examples/"fixture", examples/"fixture-fedcba9876543210")
                before = (examples/"fixture").stat()
                with self.assertRaises(ValueError): sandbox.export_rust_store(fd)
                self.assertEqual((examples/"fixture").stat().st_nlink, before.st_nlink)
                self.assertFalse((root/"rust-store-export").exists())

    def test_unknown_name_symlink_fifo_and_unrelated_executable_refuse(self):
        for problem in ("name", "symlink", "fifo", "unrelated", "linked-dependency", "inventory"):
            with self.subTest(problem=problem):
                root, fd, examples = self.model()
                if problem == "name": (examples/"fixture-INVALID").write_bytes(b"unknown")
                elif problem == "symlink":
                    (examples/"fixture").unlink()
                    os.symlink("fixture-0123456789abcdef", examples/"fixture")
                elif problem == "fifo": os.mkfifo(examples/"fixture-fedcba9876543210.d")
                elif problem == "unrelated": (examples/"fixture-fedcba9876543210").write_bytes(b"other inode")
                elif problem == "linked-dependency": os.link(examples/"fixture.d", root/"dependency-alias")
                else:
                    for i in range(17): (examples/(("fixture-%016x.d") % (i+1))).write_bytes(b"record")
                with self.assertRaises((OSError, ValueError)): sandbox.export_rust_store(fd)
                self.assertFalse((root/"rust-store-export").exists())

    def test_missing_alias_empty_oversized_owner_mode_and_linked_parent_refuse(self):
        for problem in ("single", "empty", "oversized", "owner", "mode", "parent"):
            with self.subTest(problem=problem):
                root, fd, examples = self.model()
                if problem == "single": (examples/"fixture-0123456789abcdef").unlink()
                elif problem == "empty": (examples/"fixture").write_bytes(b"")
                elif problem == "oversized": (examples/"fixture").write_bytes(b"x"*65)
                elif problem == "mode": (examples/"fixture").chmod(0o777)
                elif problem == "parent":
                    (root/"rust-target").rename(root/"linked-target")
                    os.symlink("linked-target", root/"rust-target")
                with mock.patch.object(sandbox, "STORE_EXPORT_MAX_BYTES", 64), mock.patch.object(sandbox, "UID", 65533 if problem == "owner" else 65534):
                    with self.assertRaises((OSError, ValueError)): sandbox.export_rust_store(fd)
                self.assertFalse((root/"rust-store-export").exists())

    def test_foreign_existing_output_directory_or_symlink_is_preserved(self):
        for problem in ("directory", "symlink"):
            with self.subTest(problem=problem):
                root, fd, _ = self.model()
                foreign = root/"foreign"; foreign.mkdir()
                (foreign/"fixture").write_bytes(b"foreign existing bytes")
                if problem == "symlink": os.symlink("foreign", root/"rust-store-export")
                else:
                    (root/"rust-store-export").mkdir()
                    (root/"rust-store-export/fixture").write_bytes(b"foreign existing bytes")
                with self.assertRaises((OSError, ValueError)): sandbox.export_rust_store(fd)
                self.assertEqual((root/"rust-store-export/fixture").read_bytes(), b"foreign existing bytes")
                self.assertEqual((foreign/"fixture").read_bytes(), b"foreign existing bytes")

    def test_exclusive_copy_collision_preserves_foreign_file_or_symlink(self):
        for problem in ("file", "symlink"):
            with self.subTest(problem=problem):
                root, fd, _ = self.model()
                original_open = os.open
                def collision(name, flags, mode=0o777, *, dir_fd=None):
                    if name == "fixture" and flags & os.O_CREAT:
                        if problem == "symlink": os.symlink("../foreign", name, dir_fd=dir_fd)
                        else:
                            foreign = original_open(name, os.O_WRONLY|os.O_CREAT|os.O_EXCL, 0o600, dir_fd=dir_fd)
                            os.write(foreign, b"foreign collision"); os.close(foreign)
                    return original_open(name, flags, mode, dir_fd=dir_fd)
                (root/"foreign").write_bytes(b"foreign collision")
                with mock.patch.object(sandbox.os, "open", side_effect=collision):
                    with self.assertRaises((OSError, ValueError)): sandbox.export_rust_store(fd)
                destination = root/"rust-store-export/fixture"
                if problem == "symlink": self.assertTrue(destination.is_symlink())
                else: self.assertEqual(destination.read_bytes(), b"foreign collision")
                self.assertEqual((root/"foreign").read_bytes(), b"foreign collision")

    def test_source_metadata_or_link_change_refuses_and_cleans_only_owned_copy(self):
        for problem in ("time", "links", "directory"):
            with self.subTest(problem=problem):
                root, fd, examples = self.model()
                original_read = os.read
                changed = False
                def mutate(descriptor, count):
                    nonlocal changed
                    result = original_read(descriptor, count)
                    if not changed and result:
                        changed = True
                        if problem == "links": os.link(examples/"fixture", root/"late-link")
                        elif problem == "directory":
                            examples.rename(examples.parent/"replaced-examples")
                            examples.mkdir()
                        else:
                            info = (examples/"fixture").stat()
                            os.utime(examples/"fixture", ns=(info.st_atime_ns, info.st_mtime_ns+1))
                    return result
                with mock.patch.object(sandbox.os, "read", side_effect=mutate):
                    with self.assertRaises(ValueError): sandbox.export_rust_store(fd)
                self.assertTrue(changed)
                surviving = examples.parent/"replaced-examples/fixture" if problem == "directory" else examples/"fixture"
                self.assertTrue(surviving.is_file())
                self.assertFalse((root/"rust-store-export").exists())

    def test_copy_hash_is_independently_read_and_partial_write_failure_cleans(self):
        for problem in ("hash", "write"):
            with self.subTest(problem=problem):
                root, fd, _ = self.model()
                original_read = os.read
                reads = 0
                def corrupt_copy(descriptor, count):
                    nonlocal reads
                    result = original_read(descriptor, count)
                    reads += 1
                    return b"X" + result[1:] if reads == 3 and result else result
                with mock.patch.object(sandbox.os, "read", side_effect=corrupt_copy if problem == "hash" else original_read), mock.patch.object(sandbox.os, "write", side_effect=OSError(errno.ENOSPC, "dummy full scratch") if problem == "write" else os.write):
                    with self.assertRaises((OSError, ValueError)): sandbox.export_rust_store(fd)
                self.assertFalse((root/"rust-store-export").exists())

    def test_typed_receipt_refuses_forged_counts_hashes_owner_or_unknown_fields(self):
        root, fd, _ = self.model()
        row = sandbox.export_rust_store(fd)
        target = root/"rust-store-export/fixture"
        binary = target.read_bytes()
        mutations = [("schema", True), ("role", "other"), ("entry_count", 17), ("entry_count", True), ("alias_count", 1), ("alias_count", 3), ("distinct_inode", False), ("unknown", 1)]
        for key, value in mutations:
            with self.subTest(key=key, value=value):
                changed = copy.deepcopy(row); changed[key] = value
                with self.assertRaises(ValueError): sandbox.validate_store_export(json.dumps(changed).encode(), binary, target.stat())
        for section in ("source", "copy"):
            for key, value in (("bytes", True), ("uid", 0), ("gid", True), ("mode", 0o777), ("links", 4), ("sha256", "0"*64), ("extra", 0)):
                with self.subTest(section=section, key=key):
                    changed = copy.deepcopy(row); changed[section][key] = value
                    with self.assertRaises(ValueError): sandbox.validate_store_export(json.dumps(changed).encode(), binary, target.stat())
        for raw in (b"", b"null", b"{}", b"{"+b" "*16384+b"}", json.dumps(row).encode().replace(b'"schema": 1', b'"schema": 1, "schema": 1')):
            with self.assertRaises(ValueError): sandbox.validate_store_export(raw, binary, target.stat())
        with self.assertRaises(ValueError): sandbox.validate_store_export(json.dumps(row).encode(), b"forged same role", target.stat())
        os.link(target, root/"export-alias")
        with self.assertRaises(ValueError): sandbox.validate_store_export(json.dumps(row).encode(), binary, target.stat())

    def test_fixed_program_and_successful_cgroup_absence_are_mandatory(self):
        import ast
        program = sandbox.rust_store_export_program()
        ast.parse(program)
        self.assertIn('root_fd=directory("/scratch")', program)
        self.assertIn('os.write(1,b"POSITION_STORE_EXPORT_REFUSED v1\\n")', program)
        self.assertNotIn("sys.argv", program)
        command = ["cargo", "build", "--frozen", "-p", "kanz-oms-store", "--example", "fixture"]
        facts = {"command": command, "supervisor_exit": 0, "remaining_processes": False,
                 "effective": {"memory.max": str(6*1024**3), "memory.swap.max": "0", "pids.max": "512", "cpu.max": "200000 100000"}}
        box = object.__new__(sandbox.Sandbox); box.active = set(); box.executions = [facts]
        box.stopped_execution(command)
        for key, value in (("remaining_processes", True), ("remaining_processes", None), ("supervisor_exit", 1), ("supervisor_exit", False), ("command", ["cargo", "other"]), ("effective", None)):
            with self.subTest(key=key, value=value):
                changed = copy.deepcopy(facts); changed[key] = value; box.executions = [changed]
                with self.assertRaises(ValueError): box.stopped_execution(command)
        box.executions = [facts]; box.active = {"live-worker"}
        with self.assertRaises(ValueError): box.stopped_execution(command)

    def test_parent_join_rechecks_actual_bytes_and_exporter_termination(self):
        for problem in (None, "bytes", "receipt", "linked", "live", "exit"):
            with self.subTest(problem=problem):
                root, fd, _ = self.model()
                receipt = sandbox.export_rust_store(fd)
                target = root/"rust-store-export/fixture"
                original = target.read_bytes()
                if problem == "bytes":
                    target.chmod(0o600); target.write_bytes(b"different exported bytes"); target.chmod(0o555)
                elif problem == "receipt": receipt["alias_count"] = 1
                elif problem == "linked": os.link(target, root/"unaccounted-export-alias")
                cargo = ["cargo", "build", "--frozen", "-p", "kanz-oms-store", "--example", "fixture"]
                effective = {"memory.max": str(6*1024**3), "memory.swap.max": "0", "pids.max": "512", "cpu.max": "200000 100000"}
                box = object.__new__(sandbox.Sandbox)
                box.active = set(); box.work_fd = fd
                box.executions = [{"command": cargo, "supervisor_exit": 0, "remaining_processes": False, "effective": effective}]
                retained = {}
                box.retain = lambda name, raw: retained.update({name: raw})
                def execute(command, **kwargs):
                    self.assertEqual(command, ["python3", "-I", "-S", "-c", sandbox.rust_store_export_program()])
                    self.assertEqual(kwargs, {"cwd": "/input/rust", "seconds": 30, "output_limit": 16384})
                    box.executions.append({"command": command, "supervisor_exit": 1 if problem == "exit" else 0,
                                           "remaining_processes": problem == "live", "effective": effective})
                    if problem == "live": box.active.add("still-live-exporter")
                    return box.executions[-1]["supervisor_exit"], json.dumps(receipt).encode()
                box.execute = execute
                if problem is None:
                    self.assertEqual(box.export_store(), original)
                    self.assertEqual(set(retained), {"oms-store", "rust-store-export.json", "rust-store-export-utility.json"})
                    self.assertEqual(retained["oms-store"], original)
                    self.assertEqual(json.loads(retained["rust-store-export.json"]), receipt)
                else:
                    with self.assertRaises((OSError, ValueError)): box.export_store()
                    self.assertNotIn("oms-store", retained)
                    if problem == "linked":
                        refusal = json.loads(retained["promotion-refusal.json"])
                        self.assertEqual((refusal["reason"], refusal["link_count"]), ("multiple-links", 2))


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


class ObservedProcessStatus(unittest.TestCase):
    def test_actual_own_pid_receipt_uses_known_host_identity(self):
        # This is a file-reader positive control, including ordinary CI. Only
        # the separate strict wrapper/kernel controls establish isolation.
        pid = os.getpid()
        with mock.patch.object(sandbox, "UID", os.getuid()):
            row = sandbox.observed_process_status(str(pid))
        self.assertEqual(row["host_pid"], pid)
        self.assertEqual(row["namespace_pid"], pid)
        self.assertEqual(row["host_uids"], [os.getuid()])
        self.assertGreaterEqual(int(row["cap_eff"], 16), 0)

    def test_only_enoent_and_esrch_mean_the_enumerated_sample_vanished(self):
        for error in (FileNotFoundError(errno.ENOENT, "dummy vanished PID"),
                      ProcessLookupError(errno.ESRCH, "dummy vanished PID")):
            with self.subTest(errno=error.errno), mock.patch.object(sandbox.Path, "read_text", autospec=True, side_effect=error) as read:
                self.assertIsNone(sandbox.observed_process_status("1234"))
                read.assert_called_once_with(Path("/proc/1234/status"))

    def test_permission_other_os_errors_and_contradictory_errno_stay_fatal(self):
        for error in (PermissionError(errno.EACCES, "dummy permission refusal"),
                      OSError(errno.EIO, "dummy read failure"),
                      FileNotFoundError(errno.EIO, "wrong disappearance errno"),
                      ProcessLookupError(errno.EPERM, "wrong disappearance errno")):
            with self.subTest(kind=type(error).__name__, errno=error.errno), mock.patch.object(sandbox.Path, "read_text", autospec=True, side_effect=error):
                with self.assertRaises(type(error)) as caught:
                    sandbox.observed_process_status("1234")
                self.assertIs(caught.exception, error)

    def test_status_receipt_and_malformed_or_privileged_identity_discriminate(self):
        good = "Uid:\t65534\t65534\t65534\t65534\nNSpid:\t1234\t2\nCapEff:\t0000000000000000\n"
        with mock.patch.object(sandbox.Path, "read_text", autospec=True, return_value=good) as read:
            self.assertEqual(sandbox.observed_process_status("1234"),
                             {"host_pid": 1234, "namespace_pid": 2, "host_uids": [65534], "cap_eff": "0000000000000000"})
            read.assert_called_once_with(Path("/proc/1234/status"))
        for changed in (good.replace("65534", "0"), good.replace("65534", "invalid"),
                        good.replace("Uid:", "MissingUid:"), good.replace("NSpid:\t1234\t2", "NSpid:"),
                        good.replace("NSpid:\t1234\t2", "NSpid:\t0"), good.replace("CapEff:", "MissingCaps:"),
                        good.replace("0000000000000000", "malformed"), good + "x"*16385):
            with self.subTest(status=changed[:24]), mock.patch.object(sandbox.Path, "read_text", autospec=True, return_value=changed):
                with self.assertRaises((ValueError, KeyError, IndexError)):
                    sandbox.observed_process_status("1234")

    def test_invalid_pid_cannot_select_any_other_proc_path(self):
        for pid in (None, True, 1, "", "0", "-1", "01", "self", "../1", "1/status", " 1", "1\n", "2147483648", "1"*100):
            with self.subTest(pid=pid), mock.patch.object(sandbox.Path, "read_text", autospec=True) as read:
                with self.assertRaises(ValueError): sandbox.observed_process_status(pid)
                read.assert_not_called()


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
    def test_native_compiler_uses_measured_driver_inside_readonly_usr(self):
        driver=Path("/usr/bin/x86_64-linux-gnu-gcc-13")
        self.assertEqual(proof.native_compiler_environment(driver), {
            "CC":str(driver),"CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_LINKER":str(driver)})
        for bad in (Path("/usr/bin/cc"),Path("/etc/alternatives/cc"),
                    Path("/scratch/x86_64-linux-gnu-gcc-13"),
                    Path("/usr/bin/../bin/x86_64-linux-gnu-gcc-13"),
                    Path("/usr/bin/x86_64-linux-gnu-gcc-13/extra"),str(driver)):
            with self.subTest(driver=str(bad)),self.assertRaises(ValueError):
                proof.native_compiler_environment(bad)

    def test_actual_toolchain_must_be_exact_go1274_linux_amd64_with_cgo(self):
        good = {"GOVERSION": "go1.27.4", "GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "1"}
        proof.require_go_toolchain(good)
        for key, value in (("GOVERSION", "go1.26.11"), ("GOVERSION", "go1.27.5"),
                           ("GOOS", "windows"), ("GOARCH", "arm64"), ("CGO_ENABLED", "0")):
            with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                proof.require_go_toolchain(dict(good, **{key: value}))
        for key in good:
            missing = dict(good)
            missing.pop(key)
            with self.assertRaises(ValueError):
                proof.require_go_toolchain(missing)

    def test_working_directory_is_declared_per_invocation_and_cannot_be_overridden(self):
        base = {"HOME": "/scratch/home", "LANG": "C.UTF-8"}
        extra = {"TEST_NATS_URL": "nats://127.0.0.1:4222"}
        with mock.patch.dict(os.environ, {"PWD": "/private-parent", "DUMMY_PRIVATE_KEY": "dummy-private-value"}):
            for cwd in ("/input", "/input/kanz", "/input/rust"):
                result = sandbox.effective_environment(base, cwd, extra)
                self.assertEqual(result, {**base, **extra, "PWD": cwd})
        self.assertEqual(base, {"HOME": "/scratch/home", "LANG": "C.UTF-8"})
        self.assertEqual(extra, {"TEST_NATS_URL": "nats://127.0.0.1:4222"})
        for cwd in ("/private", "/input/../private", "relative", ""):
            with self.assertRaises(ValueError):
                sandbox.effective_environment(base, cwd)
        for source, added in ((dict(base, PWD="/input/kanz"), None), (base, {"PWD": "/input/kanz"})):
            with self.assertRaises(ValueError):
                sandbox.effective_environment(source, "/input/kanz", added)

    def test_environment_difference_retains_only_closed_names_and_counts_not_success(self):
        value = {"schema": 1, "expected_count": 4, "observed_count": 5, "missing_count": 0,
                 "unexpected_count": 1, "known_unexpected": ["PWD"], "unknown_count": 0, "pwd_matches_cwd": True}
        for known, unknown in ((["PWD"], 0), ([], 1)):
            expected = dict(value, known_unexpected=known, unknown_count=unknown)
            raw = (sandbox.PROBE_ENTRY + b"\n" + sandbox.PROBE_ENVIRONMENT + json.dumps(expected).encode()
                   + b"\nPOSITION_PROBE_REFUSAL v1 stage environment assertion\n")
            observation, payload = sandbox.probe_output(1, raw, len(raw))
            self.assertIsNone(payload)
            self.assertEqual(observation["framing"], "refusal")
            self.assertEqual(observation["environment_difference"], expected)
            self.assertNotIn("passed", json.dumps(observation))
            for code, frame in ((0, raw), (1, raw.replace(b"stage environment", b"stage identity")),
                                (1, raw.replace(b"assertion", b"os-operation")), (1, raw + b"extra\n")):
                refused, payload = sandbox.probe_output(code, frame, len(frame))
                self.assertIsNone(payload)
                self.assertNotIn("environment_difference", refused)

    def test_environment_metadata_rejects_private_unknown_duplicate_and_inconsistent_fields(self):
        original = {"schema": 1, "expected_count": 4, "observed_count": 5, "missing_count": 0,
                    "unexpected_count": 1, "known_unexpected": ["PWD"], "unknown_count": 0, "pwd_matches_cwd": True}
        secret = "dummy-private-environment-name-or-value"
        changes = {"schema": True, "expected_count": 0, "observed_count": 4, "missing_count": 5,
                   "unexpected_count": 129, "unknown_count": 1, "known_unexpected": [secret],
                   "pwd_matches_cwd": 1, "private": secret}
        variants = [dict(original, **{key: value}) for key, value in changes.items()]
        variants += [dict(original, known_unexpected=["PWD", "PWD"]),
                     dict(original, expected_count=True), dict(original, observed_count=-1),
                     dict(original, observed_count=4, unexpected_count=0, known_unexpected=[])]
        encoded = [json.dumps(value).encode() for value in variants]
        encoded += [b'{"schema":1,"schema":1}', b"[]", b"\xff", b"x" * 2049]
        for content in encoded:
            raw = (sandbox.PROBE_ENTRY + b"\n" + sandbox.PROBE_ENVIRONMENT + content
                   + b"\nPOSITION_PROBE_REFUSAL v1 stage environment assertion\n")
            observation, payload = sandbox.probe_output(1, raw, len(raw))
            self.assertIsNone(payload)
            self.assertEqual(observation["stage"], "unknown")
            self.assertNotIn(secret, json.dumps(observation))
            self.assertNotIn("environment_difference", observation)

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
        packages=sorted({package,INTEGER_BROWSER_TEST[0]}|{p for p,_ in proof.CAUSAL_CASES})
        rows=[]
        for p in packages:
            rows.append({"Action":"start","Package":p})
            for owner,name in sorted(proof.CAUSAL_CASES):
                if owner==p: rows += [{"Action":"run","Package":p,"Test":name},{"Action":"output","Package":p,"Test":name,"Output":"causal live-OMS POSITION receipts: 1 inputs, both exact keys per input"},{"Action":"pass","Package":p,"Test":name}]
            if p==package: rows += self.native_events()+[{"Action":"output","Package":p,"Output":"Chromium shared-session inventory and revocation PASS"}]
            if p==INTEGER_BROWSER_TEST[0]:
                rows += [{"Action":"run","Package":p,"Test":INTEGER_BROWSER_TEST[1]},
                         {"Action":"output","Package":p,"Test":INTEGER_BROWSER_TEST[1],"Output":INTEGER_BROWSER_PROOF},
                         {"Action":"pass","Package":p,"Test":INTEGER_BROWSER_TEST[1]}]
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
        without_integer_marker=[r for r in rows if r.get("Output")!=INTEGER_BROWSER_PROOF]
        with self.assertRaises(ValueError): proof.pass_result(raw(without_integer_marker),packages)

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
            private=root/"private";private.mkdir(mode=0o700)
            repo=root/"empty-repo";repo.mkdir()
            occupied.bind(("127.0.0.1",0));occupied.listen(1)
            available.bind(("127.0.0.1",0))
            routes=((available.getsockname()[1],1),(occupied.getsockname()[1],2))
            available.close()
            # Model privileged setup without invoking any host provisioning.
            # The real relay still hits an owned loopback-port collision.
            with mock.patch.object(sandbox.os,"geteuid",return_value=0):
                box=sandbox.Sandbox(repo,capsule=modeled_capsule(boundary),artifact_root=private,tools=[],env={})
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
                self.assertFalse(cleanup["relay_stopped"])
                self.assertFalse(cleanup["capsule_cleanup_complete"])
                self.assertIsNone(box.capsule.absent,"capsule retired without proven relay teardown")
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
        candidate.input_capsule=sandbox.VisibleInputCapsule()
        candidate.source={"sha":"a"*40,"tree":"b"*40,"run":"1","attempt":"1"}
        candidate.publication=publication
        candidate.publication_fd=sandbox.directory(publication)
        candidate.box=types.SimpleNamespace(cleanup_verified=True,capsule_cleanup_complete=True,receipts_fd=sandbox.directory(retained),artifacts={})
        for name,raw in (("passes-complete.json",b"two passes and controls"),("receipt.json",b"bounded receipt"),("oms-store",b"private executable")):
            sandbox.new_file(candidate.box.receipts_fd,name,raw)
            candidate.box.artifacts[name]={"size":len(raw),"sha256":proof.digest(raw)}
        return candidate,private,retained

    def test_literal_sibling_is_listable_after_teardown_and_excludes_binaries(self):
        with tempfile.TemporaryDirectory() as raw:
            candidate,private,retained=self.candidate(Path(raw))
            try:
                # Model a path-denied operator tree. Publication is a sibling;
                # it never needs to make the private artifact root traversable.
                private.chmod(0o000)
                with self.assertRaises(PermissionError): list(os.scandir(private))
                private.chmod(0o700)
                previous=os.umask(0o777)
                try: candidate.publish(complete=True)
                finally: os.umask(previous)
                with os.scandir(candidate.publication) as entries: names={p.name for p in entries}
                self.assertEqual(names,{"receipt.json","passes-complete.json","complete.json"})
                self.assertEqual(stat.S_IMODE(private.stat().st_mode),0o700)
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

    def test_unretired_visible_inputs_refuse_even_otherwise_complete_publication(self):
        with tempfile.TemporaryDirectory() as raw:
            candidate,_,_=self.candidate(Path(raw))
            candidate.box.capsule_cleanup_complete=False
            try:
                with self.assertRaisesRegex(ValueError,"observed complete sandbox teardown"):
                    candidate.publish(complete=True)
                self.assertEqual(list(candidate.publication.iterdir()),[])
            finally: candidate.close_receipts()


class CleanupRefusals(unittest.TestCase):
    def test_log_refusal_does_not_skip_descriptors_or_mount_cleanup(self):
        with tempfile.TemporaryDirectory() as raw:
            root=Path(raw)
            work=root/"scratch";work.mkdir()
            public=root/"public";public.mkdir()
            box=object.__new__(sandbox.Sandbox)
            box.capsule=modeled_capsule(root/"capsule")
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
