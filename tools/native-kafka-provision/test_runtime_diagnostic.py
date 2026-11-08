import unittest
import json
import runtime_diagnostic as diagnostic


class RuntimeDiagnosticTest(unittest.TestCase):
    def test_memory_projection_is_numeric_and_closed(self):
        result=diagnostic.memory_events('low 0\nhigh 0\nmax 4\noom 3\noom_kill 2\noom_group_kill 0\n')
        self.assertEqual(result,{'oom':3,'oom_kill':2,'oom_group_kill':0})
        for raw in ('oom SECRET','oom -1','oom 1\noom 2','oom 1','x'*4097):
            with self.assertRaises(ValueError): diagnostic.memory_events(raw)

    def test_log_projection_never_retains_text(self):
        result=diagnostic.broker_log('SECRET seastar::memory::bad_alloc Assertion failed SECRET')
        self.assertEqual(result,{'allocationFailure':True,'assertionFailure':True})
        self.assertNotIn('SECRET',json.dumps(result))
        self.assertEqual(diagnostic.broker_log('SECRET'),{'allocationFailure':False,'assertionFailure':False})
        with self.assertRaises(ValueError):diagnostic.broker_log('x'*32769)

    def test_collection_error_drops_message(self):
        self.assertEqual(diagnostic.collection_error(ValueError('SECRET')),'shape')
        self.assertEqual(diagnostic.collection_error(RuntimeError('SECRET')),'command')
        self.assertEqual(diagnostic.collection_error(Exception('SECRET')),'unexpected')

    def test_eviction_drops_status_message(self):
        self.assertEqual(diagnostic.eviction({'reason':'Evicted','message':'Usage of EmptyDir volume SECRET exceeds limit'}),'storage')
        self.assertEqual(diagnostic.eviction({'reason':'Evicted','message':'node low on memory SECRET'}),'memory')
        self.assertEqual(diagnostic.eviction({'reason':'Evicted','message':'SECRET'}),'UNKNOWN')
