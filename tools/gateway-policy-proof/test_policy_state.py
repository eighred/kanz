"""The shared state extraction preserves the frozen implementation exactly."""
import ast
from pathlib import Path
import hashlib
import unittest

import policy_state
import transaction

HERE = Path(__file__).resolve().parent
# Exact declaration bytes from frozen 72a4903b; no history dependency in shallow CI.
ORIGINAL = {'NAMESPACE': 'e12cf1718b6c8f7f9b7de6a3138efb3ee41347bd39634a64d127e2f87e13e2df',
 'NAMES': '33f0a2a7b5954bfa0276571b534b95cf7046cf24601dfdb34049dcf046c157d5',
 'TOKEN': 'c272fc12f3da4ed257692b353031a17e565c29b66119d548b5b5e1b7f98c29e0',
 'SERVER_METADATA': '42c15d1590cc9bb49188b99a522c9ad3649f2be490825f35b38a50e992c773da',
 'Refusal': 'fbf0f5298d889b4573c2d6d1597543cf8c432ed17ea85a4dc6b6a200e1f80f5d',
 'Receipts': '57c06b25305583a08c094a7add7ce84a52d5bd93e29cb6fab6b1e58f45fb7cfd',
 'encoded': 'f375aa1adb6b4457211fdc6bfcbb68b8564fcc03f75f28424098e712b6151f45',
 'fingerprint': 'd8e040c8f6b48a82db17dbfb9352e17f9ba24bdf25048371ded698ccc3746a0d',
 'semantic': 'cc83ce4a95114560759ffa078df3667515a44ecd653190bde3cd79c82b2ac9c1',
 'inventory': '33ae4ef74e8c70b8f328ca1a8c97f56c0df951dafa6337f4c202c5e02cc98d79',
 'version': '73209cca20aeb077fdc6485467a682d38b352289b0d787c986b240810f322ee1',
 'same_inventory': '2214c2a34c1549161bdefa5eeab8bf9d262bf5aeced39045021ae98e298f4244',
 'qualify_rendered_baseline': 'bfa125c3840359791c3903a2284a134ec45b0405a599cb1ffd312521ed9a7454',
 'Transaction': '4b30bf9fc77e6a3d173d18a77b60b9ceb600b74746c0e2a1bafcc5028dcaf7e1'}
NAMES = ("NAMESPACE", "NAMES", "TOKEN", "SERVER_METADATA", "Refusal", "Receipts",
         "encoded", "fingerprint", "semantic", "inventory", "version", "same_inventory",
         "qualify_rendered_baseline")


def declarations(source):
    result = {}
    for node in ast.parse(source).body:
        if isinstance(node, (ast.FunctionDef, ast.ClassDef)):
            result[node.name] = ast.get_source_segment(source, node)
        elif isinstance(node, ast.Assign) and isinstance(node.targets[0], ast.Name):
            result[node.targets[0].id] = ast.get_source_segment(source, node)
    return result


class PolicyStateTests(unittest.TestCase):
    def test_exact_original_bodies_and_public_alias_identity(self):
        shared = declarations((HERE / "policy_state.py").read_text(encoding="utf-8"))
        current = declarations((HERE / "transaction.py").read_text(encoding="utf-8"))
        self.assertEqual(hashlib.sha256(current["Transaction"].encode()).hexdigest(), ORIGINAL["Transaction"])
        for name in NAMES:
            with self.subTest(name=name):
                self.assertEqual(hashlib.sha256(shared[name].encode()).hexdigest(), ORIGINAL[name])
                self.assertEqual(current[name], name + " = state." + name)
                self.assertIs(getattr(transaction, name), getattr(policy_state, name))


if __name__ == "__main__":
    unittest.main()
