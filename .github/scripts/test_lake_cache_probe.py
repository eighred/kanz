import copy
import json
from pathlib import Path
import sys
import tempfile
import unittest
import subprocess

import lake_cache_probe as gate

SOURCE = "a" * 40


def events():
    version = gate.digest(("kanz-lake-joint-cache-probe:" + SOURCE).encode())[:40]
    return [{"vertexes": [
        {"digest":"sha256:"+"1"*64,"name":"[rust-build 2/6] RUN "+gate.COOK,
         "started":"2026-11-07T00:00:00Z","completed":"2026-11-07T00:00:01Z","cached":True},
        {"digest":"sha256:"+"2"*64,"name":"[rust-build 6/6] RUN "+gate.BUILD.replace("$VERSION",version),
         "started":"2026-11-07T00:00:01Z","completed":"2026-11-07T00:00:02Z"}]}]


def wire(value):
    return ("\n".join(json.dumps(x) for x in value)+"\n").encode()


class CacheProbeTests(unittest.TestCase):
    def test_synthetic_solve_imports_only_named_read_cache(self):
        args=gate.build_args('owned-builder',Path('/owned-context'),'a'*40)
        self.assertEqual([arg for arg in args if arg.startswith('--cache-')],
                         ['--cache-from=type=gha,scope=lake-sink'])
        self.assertIn('--output=type=cacheonly',args)
        self.assertEqual(args[args.index('--target')+1],'rust-build')
        self.assertEqual(args[args.index('--build-arg')+1],'VERSION='+'a'*40)

    @unittest.skipUnless(sys.platform == "linux", "actual Linux no-follow Git source custody required")
    def test_actual_tracked_context_refuses_changed_bytes_and_links(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)/"repo";root.mkdir()
            subprocess.run(["git","init","-q",str(root)],check=True)
            for name in (gate.SOURCE,gate.DOCKERFILE,".dockerignore"):
                path=root/name;path.parent.mkdir(parents=True,exist_ok=True);path.write_bytes(b"original\n")
            subprocess.run(["git","-C",str(root),"add","."],check=True)
            subprocess.run(["git","-C",str(root),"-c","user.name=Fixture","-c","user.email=fixture@example.invalid","commit","-qm","synthetic"],check=True)
            out=Path(directory)/"copy";out.mkdir()
            hashes=gate.copy_context(root,out)
            self.assertEqual(hashes[gate.SOURCE]["sha256"],gate.digest(b"original\n"))
            source,tree=subprocess.check_output(["git","-C",str(root),"rev-parse","HEAD","HEAD^{tree}"],text=True).splitlines()
            expected=gate.source_hashes(root,source,tree)
            self.assertEqual(expected,gate.source_projection(hashes,b"original\n"))
            (root/gate.SOURCE).write_bytes(b"changed\n")
            bad=Path(directory)/"changed";bad.mkdir()
            with self.assertRaises(subprocess.CalledProcessError):gate.copy_context(root,bad)
            subprocess.run(["git","-C",str(root),"add","."],check=True)
            bad=Path(directory)/"staged";bad.mkdir()
            with self.assertRaises(subprocess.CalledProcessError):gate.copy_context(root,bad)
            subprocess.run(["git","-C",str(root),"reset","--hard","-q","HEAD"],check=True)
            (root/gate.SOURCE).unlink();(root/gate.SOURCE).symlink_to(root/".dockerignore")
            bad=Path(directory)/"linked";bad.mkdir()
            with self.assertRaises((OSError,ValueError,subprocess.CalledProcessError)):gate.copy_context(root,bad)

    def test_actual_schema_controls_require_cache_and_execution(self):
        expected = {"cook":"sha256:"+"1"*64,"build":"sha256:"+"2"*64}
        self.assertEqual(gate.progress(wire(events()), SOURCE), expected)
        for mutation in ("missing", "uncached", "bool", "final-cached", "unfinished", "wrong-command", "wrong-version", "duplicate", "error", "unknown"):
            data = events(); vertices = data[0]["vertexes"]
            if mutation == "missing": vertices.pop(0)
            if mutation == "uncached": vertices[0].pop("cached")
            if mutation == "bool": vertices[0]["cached"] = 1
            if mutation == "final-cached": vertices[1]["cached"] = True
            if mutation == "unfinished": vertices[1].pop("completed")
            if mutation == "wrong-command": vertices[0]["name"] += " --offline"
            if mutation == "wrong-version": vertices[1]["name"] = vertices[1]["name"].replace(gate.digest(("kanz-lake-joint-cache-probe:"+SOURCE).encode())[:40], SOURCE)
            if mutation == "duplicate": vertices.append({**vertices[0],"digest":"sha256:"+"3"*64})
            if mutation == "error": vertices[1]["error"] = "closed synthetic failure"
            if mutation == "unknown": data[0]["other"] = []
            with self.subTest(mutation=mutation), self.assertRaises(ValueError): gate.progress(wire(data), SOURCE)
        for raw in (wire(events()).rstrip(), b"x"*(gate.LIMIT+1), b'{"vertexes":[],"vertexes":[]}\n', b'{}\n'):
            with self.assertRaises(ValueError): gate.progress(raw, SOURCE)

    def test_restarted_internal_vertices_preserve_owned_witnesses(self):
        # Normalized from failed run42374756725 artifact13575722853: internal
        # Dockerfile/context/frontend vertices restart after a completed update.
        data = events()
        for i, name in enumerate(("[internal] load build definition from Dockerfile",
                                  "[internal] load .dockerignore", "[internal] load build context",
                                  "docker-image://docker.io/docker/dockerfile:1@sha256:"+"f"*64)):
            first = {"digest":"sha256:"+str(i+3)*64,"name":name,
                     "started":"2026-11-07T00:00:00Z","completed":"2026-11-07T00:00:01Z"}
            restarted = {**first,"started":"2026-11-07T00:00:02Z","cached":True}
            restarted.pop("completed")
            data += [{"vertexes":[first]}, {"vertexes":[restarted]},
                     {"vertexes":[{**restarted,"completed":"2026-11-07T00:00:03Z"}]}]
        self.assertEqual(gate.progress(wire(data),SOURCE),gate.progress(wire(events()),SOURCE))
        for index in (0,1):
            for mutation in ("reopen","cached","error"):
                bad=copy.deepcopy(data); changed=copy.deepcopy(bad[0]["vertexes"][index])
                if mutation=="reopen":changed.pop("completed")
                if mutation=="cached":changed["cached"]=not changed.get("cached",False)
                if mutation=="error":changed["error"]="closed synthetic failure"
                bad.append({"vertexes":[changed]})
                with self.subTest(target=index,mutation=mutation),self.assertRaises(ValueError):gate.progress(wire(bad),SOURCE)
        for mutation in ("error","identity"):
            bad=copy.deepcopy(data);changed=copy.deepcopy(bad[-1]["vertexes"][0])
            if mutation=="error":changed["error"]="closed synthetic failure"
            if mutation=="identity":changed["name"]="different internal operation"
            bad.append({"vertexes":[changed]})
            with self.subTest(internal=mutation),self.assertRaises(ValueError):gate.progress(wire(bad),SOURCE)

    def test_closed_report_binds_source_image_and_progress(self):
        raw = wire(events()); version = gate.digest(("kanz-lake-joint-cache-probe:"+SOURCE).encode())[:40]
        report = {"schema":1,"source":SOURCE,"tree":"b"*40,"run":"1","attempt":"2","image":"sha256:"+"c"*64,"builder":"builder-fixed",
                  **{k:"d"*64 for k in ("contextSHA256","dockerfileSHA256","sourceBeforeSHA256")},
                  "sourceAfterSHA256":"e"*64,"versionSHA256":gate.digest(version.encode()),"progressSHA256":gate.digest(raw),
                  **gate.progress(raw,SOURCE),"jointSourceAndVersion":True,"cookCached":True,"finalBuildExecuted":True,"syntheticOnly":True}
        expected_hashes={key:report[key] for key in gate.SOURCE_HASHES}
        def check(value): gate.validate(value,raw,SOURCE,"b"*40,"1","2","sha256:"+"c"*64,expected_hashes)
        check(report)
        for key in gate.SOURCE_HASHES:
            bad=copy.deepcopy(report);bad[key]="f"*64
            with self.subTest(false_hash=key),self.assertRaises(ValueError):check(bad)
        for key in report:
            bad=copy.deepcopy(report);bad.pop(key)
            with self.subTest(missing=key), self.assertRaises(ValueError): check(bad)
        for key in ("source","tree","run","attempt","image","progressSHA256","cook","build","versionSHA256"):
            bad=copy.deepcopy(report);bad[key]="wrong"
            with self.subTest(changed=key), self.assertRaises(ValueError): check(bad)
        bad=copy.deepcopy(report);bad["sourceAfterSHA256"]=bad["sourceBeforeSHA256"]
        with self.assertRaises(ValueError):check(bad)

    @unittest.skipUnless(sys.platform == "linux", "actual Linux process ownership required")
    def test_actual_stderr_capture_preserves_stdout_discard_bound(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/"progress"
            gate.bounded_command([sys.executable,"-c","import sys;sys.stderr.write('captured');sys.stdout.write('discarded')"],path,32,capture_stderr=True,timeout=5)
            self.assertEqual(path.read_bytes(),b"captured")
            for code in ("import sys;sys.stderr.write('x'*33)","import sys;sys.stdout.write('x'*65537)"):
                with self.assertRaises(ValueError): gate.bounded_command([sys.executable,"-c",code],Path(directory)/("bad"+str(len(code))),32,capture_stderr=True,timeout=5)


if __name__ == "__main__": unittest.main()
