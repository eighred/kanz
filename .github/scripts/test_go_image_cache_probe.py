import copy
import json
from pathlib import Path
import unittest
import tempfile
from types import SimpleNamespace

import go_image_cache_probe as p


class CacheControls(unittest.TestCase):
    source = "a" * 40
    tree = "b" * 40
    image = "sha256:" + "c" * 64
    repo = "ghcr.io/eighred/base/buildkit@sha256:" + "d" * 64

    def raw(self, case):
        _, version = p.variant(case, self.source)
        return (json.dumps({"vertexes": [
            {"digest": "sha256:" + "1" * 64, "name": "[build  7/10] RUN " + p.DOWNLOAD,
             "started": "2026-11-07T00:00:00Z", "completed": "2026-11-07T00:00:01Z", "cached": case in p.CASES[:2]},
            {"digest": "sha256:" + "2" * 64, "name": "[build 10/10] RUN " + p.COMPILE.format(version=version),
             "started": "2026-11-07T00:00:00Z", "completed": "2026-11-07T00:00:01Z", "cached": False},
        ]}) + "\n").encode()

    def fixture(self):
        inventory = {n: {"sha256": p.digest(n.encode()), "commentSHA256": p.digest(n.encode()+p.COMMENT)}
                     for n in (p.DOCKERFILE, p.SOURCE, p.MODULE, p.SDK_MODULE)}
        logs = {name: p.closed_progress(self.raw(name),name,self.source) for name in p.CASES}
        report = {"schema": 1, "source": self.source, "tree": self.tree, "run": "1", "attempt": "1",
                  "contextSHA256": p.projection(inventory), "dockerfileSHA256": inventory[p.DOCKERFILE]["sha256"],
                  "builderImage": self.image, "builderRepositoryDigest":self.repo,"normalContextUnchanged":True, "cacheScope": "accounting", "readOnly": True, "cleanupPassed": True, "cases": []}
        for i,name in enumerate(p.CASES):
            path,version=p.variant(name,self.source)
            report["cases"].append({"case":name,"builder":"kanz-go-cache-"+"d"*24+"-"+str(i),
                "versionSHA256":p.digest(version.encode()),"changedPath":path,
                "beforeSHA256":inventory[path]["sha256"] if path else None,
                "afterSHA256":inventory[path]["commentSHA256"] if path else None,
                "progressSHA256":p.digest(logs[name]),**p.projected_witnesses(logs[name],name,self.source)})
        return inventory,logs,report

    def test_four_controls_and_receipt_mutations(self):
        inv,logs,report=self.fixture()
        check=lambda r:p.validate(r,logs,self.source,self.tree,"1","1",inv,self.image,self.repo)
        check(report)
        for field in ("source","tree","contextSHA256","dockerfileSHA256","builderImage","builderRepositoryDigest","cacheScope"):
            r=copy.deepcopy(report);r[field]="e"*len(r[field])
            with self.subTest(field=field), self.assertRaises(ValueError):check(r)
        for field in ("readOnly","cleanupPassed","normalContextUnchanged"):
            r=copy.deepcopy(report);r[field]=False
            with self.subTest(field=field),self.assertRaises(ValueError):check(r)
        for field in ("versionSHA256","beforeSHA256","afterSHA256","progressSHA256","download","compile","globalCompletion","observedVertices","internalIncomplete","unobservedInputs"):
            r=copy.deepcopy(report);r["cases"][1][field]="f"*64
            with self.subTest(field=field),self.assertRaises(ValueError):check(r)
        r=copy.deepcopy(report);r["cases"][1]["builder"]=r["cases"][0]["builder"]
        with self.assertRaises(ValueError):check(r)

    def test_target_cache_error_reopen_and_missing_refuse(self):
        for case in p.CASES:
            raw=self.raw(case);p.witnesses(raw,case,self.source)
            event=json.loads(raw)
            for i in (0,1):
                bad=copy.deepcopy(event);bad["vertexes"][i]["cached"]=not bad["vertexes"][i]["cached"]
                with self.assertRaises(ValueError):p.witnesses((json.dumps(bad)+"\n").encode(),case,self.source)
            bad=copy.deepcopy(event);bad["vertexes"][0]["error"]="closed test error"
            with self.assertRaises(ValueError):p.witnesses((json.dumps(bad)+"\n").encode(),case,self.source)
            reopened=copy.deepcopy(event);del reopened["vertexes"][0]["completed"]
            with self.assertRaises(ValueError):p.witnesses(raw+(json.dumps(reopened)+"\n").encode(),case,self.source)
            with self.assertRaises(ValueError):p.witnesses(b'{"vertexes":[]}\n',case,self.source)

    def test_projection_discards_transport_and_refuses_errors(self):
        event=json.loads(self.raw("baseline"))
        event["vertexes"].append({"digest":"sha256:"+"3"*64,"name":"https://secret.invalid/TOKEN_SENTINEL", "started":"2026-11-07T00:00:00Z","completed":"2026-11-07T00:00:01Z"})
        event["logs"]=[{"data":"TOKEN_SENTINEL"}]
        raw=(json.dumps(event)+"\n").encode()
        closed=p.closed_progress(raw,"baseline",self.source)
        self.assertNotIn(b"TOKEN_SENTINEL",closed)
        self.assertNotIn(b"https://",closed)
        p.projected_witnesses(closed,"baseline",self.source)
        for mutation in ("error", "secret-name"):
            bad=json.loads(closed)
            v=bad["vertexes"][-1]
            if mutation=="error":v["errorPresent"]=True
            if mutation=="secret-name":v["name"]="TOKEN_SENTINEL"
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):p.projected_witnesses((json.dumps(bad)+"\n").encode(),"baseline",self.source)

    def test_internal_observation_may_be_partial_but_owned_witness_must_complete(self):
        event=json.loads(self.raw("baseline"))
        # c9 artifact13584634708 begins with this internal digest and neither
        # timestamp. Keep that observed shape; add an explicitly synthetic
        # unobserved input to discriminate the separate partial-graph rule.
        event["vertexes"].append({"digest":"sha256:2b599473b9f280223240454f6b5bd244f4c39ed6dee49ab53b223d7d52b72ae8",
            "name":"internal fixture","inputs":["sha256:"+"4"*64]})
        closed=p.closed_progress((json.dumps(event)+"\n").encode(),"baseline",self.source)
        result=p.projected_witnesses(closed,"baseline",self.source)
        self.assertEqual(result["globalCompletion"],"not-asserted")
        self.assertEqual(result["internalIncomplete"],1);self.assertEqual(result["unobservedInputs"],1)
        for index in (0,1):
            bad=copy.deepcopy(event);del bad["vertexes"][index]["completed"]
            with self.assertRaises(ValueError):p.closed_progress((json.dumps(bad)+"\n").encode(),"baseline",self.source)

    def test_lost_create_reply_owned_cleanup_and_foreign_refusal(self):
        owned="kanz-go-cache-"+"a"*24+"-0"
        names={owned,"foreign"};removed=[]
        def command(argv,*args,**kwargs):
            if argv[:3]==["docker","buildx","ls"]:return ("\n".join(sorted(names))+"\n").encode()
            if argv[:3]==["docker","buildx","rm"]:
                removed.append(argv[-1]);names.remove(argv[-1]);return b""
            return b""
        p.cleanup_builders(command,[owned])
        self.assertEqual(removed,[owned]);self.assertEqual(names,{"foreign"})
        with self.assertRaises(ValueError):p.cleanup_builders(command,["foreign"])
        def refused(argv,*args,**kwargs):
            if argv[:3]==["docker","buildx","ls"]:return (owned+"\n").encode()
            if argv[:3]==["docker","buildx","rm"]:raise ValueError("closed refusal")
            return b""
        with self.assertRaises(ValueError):p.cleanup_builders(refused,[owned])

    def test_failed_receipt_retains_only_closed_completed_case(self):
        args=SimpleNamespace(source=self.source,tree=self.tree,run="1",attempt="1")
        state={"stage":"progress","case":"module","error":None,"failedStage":None,"failedCase":None,"cleanup":"UNKNOWN","completed":{"baseline":"e"*64}}
        p.record_failure(state,ValueError("TOKEN_SENTINEL https://credential.invalid"))
        state["stage"]="cleanup";state["cleanup"]=False
        p.record_failure(state,p.ProbeFailure("cleanup"))
        with tempfile.TemporaryDirectory() as directory:
            output=Path(directory);p.write_status(output,args,state)
            raw=(output/"status.json").read_bytes();self.assertNotIn(b"TOKEN_SENTINEL",raw);self.assertNotIn(b"https://",raw)
            receipt=json.loads(raw)
            self.assertFalse(receipt["passed"]);self.assertEqual(receipt["stage"],"progress");self.assertEqual(receipt["case"],"module")
            self.assertEqual(receipt["error"],"progress");self.assertIs(receipt["cleanup"],False)
            self.assertEqual(receipt["completed"],{"baseline":"e"*64})
        for category in p.ERRORS:
            probe={**state,"error":None,"failedStage":None,"failedCase":None}
            p.record_failure(probe,p.ProbeFailure(category));self.assertEqual(probe["error"],category)

    def test_failed_workspace_joins_cleanup_before_status(self):
        state={"cleanup":"UNKNOWN"}
        with self.assertRaises(RuntimeError):
            with p.owned_workspace(state) as directory:
                owned=Path(directory);(owned/"temporary").write_bytes(b"fixture")
                state["builderCleanup"]=True
                raise RuntimeError("closed abort")
        self.assertFalse(owned.exists());self.assertIs(state["cleanup"],True)
        with p.owned_workspace(state):state["builderCleanup"]="UNKNOWN"
        self.assertEqual(state["cleanup"],"UNKNOWN")

    def test_variant_restores_owned_copy_without_changing_normal_context(self):
        with tempfile.TemporaryDirectory() as directory:
            normal=Path(directory)/"normal";owned=Path(directory)/"owned"
            for root in (normal,owned):
                for name in (p.SOURCE,p.MODULE,p.SDK_MODULE):
                    path=root/name;path.parent.mkdir(parents=True,exist_ok=True);path.write_bytes(b"original\n")
            for name in p.CASES:
                with self.assertRaises(RuntimeError):
                    with p.controlled_variant(owned,name,self.source) as (path,version):
                        for file in (p.SOURCE,p.MODULE,p.SDK_MODULE):self.assertEqual((normal/file).read_bytes(),b"original\n")
                        if path:self.assertEqual((owned/path).read_bytes(),b"original\n"+p.COMMENT)
                        raise RuntimeError("controlled abort")
                for file in (p.SOURCE,p.MODULE,p.SDK_MODULE):self.assertEqual((owned/file).read_bytes(),b"original\n")

    def test_actual_padded_counter_and_closed_negative_labels(self):
        # Exact retained c9 regulatory128481084988 DOWNLOAD custom name.
        good="[build  7/10] RUN "+p.DOWNLOAD
        self.assertTrue(p.command_name_matches(good,"build",p.DOWNLOAD,canonical_counter=True))
        self.assertFalse(p.command_name_matches(good,"build",p.DOWNLOAD))
        self.assertTrue(p.command_name_matches("[build 7/10] RUN "+p.DOWNLOAD,"build",p.DOWNLOAD))
        for label in ("[build 7/10]", "[build   7/10]", "[build 07/10]", "[build  0/10]", "[build 11/10]", "[build  7/010]", "[build\t7/10]", "[build 7/0]", "[other  7/10]"):
            bad=json.loads(self.raw("baseline"));bad["vertexes"][0]["name"]=label+" RUN "+p.DOWNLOAD
            with self.subTest(label=label),self.assertRaises(ValueError):p.closed_progress((json.dumps(bad)+"\n").encode(),"baseline",self.source)
        for command in (p.DOWNLOAD+" && true", "true", p.DOWNLOAD.replace("go-mod", "other-mod")):
            self.assertFalse(p.command_name_matches("[build  7/10] RUN "+command,"build",p.DOWNLOAD,canonical_counter=True))
        self.assertEqual(json.loads(p.closed_progress(self.raw("baseline"),"baseline",self.source))["vertexes"][0]["name"],good)

    def test_only_read_import_real_compile_target(self):
        argv=p.build_args("owned",Path("/owned"),self.source)
        self.assertEqual(argv.count("--cache-from=type=gha,scope=accounting"),1)
        self.assertFalse(any(a.startswith(("--cache-to","--push","--load","--no-cache")) for a in argv))
        self.assertEqual(argv[argv.index("--target")+1],"build")
        self.assertEqual(argv[argv.index("--file")+1],str(Path("/owned")/p.DOCKERFILE))
        self.assertIn("--output=type=cacheonly",argv)


if __name__ == "__main__":unittest.main()
