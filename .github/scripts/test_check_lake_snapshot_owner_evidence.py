"""Offline checker mutations; these synthetic events are never runtime proof."""
import copy
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest

import check_lake_snapshot_owner_evidence as gate


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
        p={"schema":1,"source":"a"*40,"tree":"b"*40,"run":"123","attempt":"2","versions":list(gate.VERSIONS),
           "binaries":{name:"d"*64 for name in gate.BINARIES},"streams":hashes,
           "cleanup":{"run":"123","attempt":"2","postgres_absent":True,"compose_absent":True},
           "dependencies":{"postgres":"sha256:"+"e"*64,"redpanda":"sha256:"+"f"*64}}
        def check(item):gate.check_manifest(item,"a"*40,"b"*40,"123","2",hashes)
        check(p)
        for key,value in (("source","z"*40),("tree","a"*40),("run","124"),("attempt","1"),("versions",["2.14.5"]),("schema",True)):
            changed=copy.deepcopy(p);changed[key]=value
            with self.assertRaises(ValueError):check(changed)
        for key in gate.BINARIES:
            changed=copy.deepcopy(p);del changed["binaries"][key]
            with self.assertRaises(ValueError):check(changed)
        for field,value in (("run","124"),("attempt","1"),("postgres_absent",False),("compose_absent",1)):
            changed=copy.deepcopy(p);changed["cleanup"][field]=value
            with self.assertRaises(ValueError):check(changed)
        changed=copy.deepcopy(p);changed["streams"]["2.12.6.jsonl"]="0"*64
        with self.assertRaises(ValueError):check(changed)
        changed=copy.deepcopy(p);changed["binaries"]["worker-swap"]="0"*64
        with self.assertRaises(ValueError):check(changed)

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
