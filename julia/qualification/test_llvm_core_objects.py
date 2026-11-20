"""Checks for the exact LLVM member comparison and publisher-bound private labels."""
import hashlib
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent / 'application_bundle'))
import llvm_core_inputs
import llvm_core_objects


class LLVMCoreTests(unittest.TestCase):
    def test_exact_members_and_narrow_target_parser_exception(self):
        compare = llvm_core_objects.equivalent
        self.assertEqual(compare(b'normal member', b'normal member', 'libLLVMSupport.a', 'Path.cpp.o'), 'byte-identical')
        old = b'string table CSWTCH.1869 section table CSWTCH.1869'
        new = old.replace(b'1869', b'1868')
        a = hashlib.sha256(old).hexdigest()
        def check(candidate, owner='libLLVMTargetParser.a', member='Triple.cpp.o'):
            with patch.object(llvm_core_objects, 'TRIPLE_PUBLISHED', a), \
                    patch.object(llvm_core_objects, 'TRIPLE_RECOMPILED', hashlib.sha256(candidate).hexdigest()):
                return compare(old, candidate, owner, member)
        self.assertIn('two digits', check(new))
        for changed in (new + b'x', new.replace(b'1868', b'1867', 1)):
            with self.assertRaisesRegex(ValueError, 'publisher object differs'):
                check(changed)
        with self.assertRaisesRegex(ValueError, 'publisher object differs'):
            check(new, member='Other.cpp.o')
        with self.assertRaisesRegex(ValueError, 'publisher object differs'):
            check(new, owner='libLLVMSupport.a')

    def test_source_ledger_matches_link_map_source_members(self):
        from core_runtime import inputs
        records = llvm_core_inputs.ledger()
        owners = inputs()['llvmMembers']
        required = {'llvm/lib/' + owner[len('libLLVM'):-2] + '/' + member[:-2]
                    for owner, members in owners.items() for member in members}
        self.assertEqual(50, len(required))
        self.assertEqual(required, {p for p in records['fork'] if p.endswith('.cpp')})
        self.assertEqual(37, len(records['jllCMake']))
        self.assertFalse(records['fork'].keys() & records['jllCMake'].keys())
