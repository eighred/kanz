import json
import unittest
import claim_probe as p

class Probe(unittest.TestCase):
    def claim(self):
        return {"critical": {"type": "https://sigstore.dev/cosign/sign/v1", "identity": {"docker-reference": p.c.IMAGE}, "image": {"docker-manifest-digest": p.c.DIGEST}}, "optional": {"source_sha": p.c.PRODUCER_SOURCE, "extra": "never expose"}}
    def test_projection_is_closed(self):
        claim = self.claim()
        result = p.project(json.dumps([claim]), "a"*40, "123", "1")
        self.assertNotIn("never expose", json.dumps(result))
        self.assertEqual(result["claims"], [{"critical_type": "https://sigstore.dev/cosign/sign/v1", "reference": "expected-immutable", "digest_matches": True, "source_matches": True}])
        for ref, expected in [(p.q.REPOSITORY, "bare-repository"), ("private unexpected reference", "other")]:
            claim["critical"]["identity"]["docker-reference"] = ref
            actual = p.project(json.dumps([claim]), "a"*40, "123", "1")
            self.assertEqual(actual["claims"][0]["reference"], expected)
            self.assertNotIn(ref, json.dumps(actual))
    def test_unknown_and_duplicate_refuse(self):
        for raw in ['[]', '{}', '[{"critical":{},"critical":{}}]']:
            with self.assertRaises(ValueError): p.project(raw, "a"*40, "123", "1")
        claim = self.claim(); claim["critical"]["type"] = "unreviewed"
        with self.assertRaises(ValueError): p.project(json.dumps([claim]), "a"*40, "123", "1")
        with self.assertRaises(ValueError): p.project(json.dumps([self.claim()]*17), "a"*40, "123", "1")

if __name__ == '__main__': unittest.main()
