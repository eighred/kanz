"""Exercise discovery protocol failures without launching a Codex session."""
import io
import json
import subprocess
import unittest
from unittest.mock import Mock, patch

from skill_discovery import discover_skills


class DiscoveryTests(unittest.TestCase):
    def process(self, result):
        process = Mock()
        messages = [{"id": 1, "result": {}}, {"id": 2, "result": result}]
        process.stdout = io.StringIO("startup log\n[]\n" + "\n".join(map(json.dumps, messages)))
        process.stdin = io.StringIO()
        process.wait.return_value = 0
        return process

    def test_success_ignores_logs_and_closes_pipes(self):
        skills = [{"name": "kanz-forge:commands", "path": "/plugin/skills/commands/SKILL.md", "enabled": True}]
        process = self.process({"data": [{"skills": skills, "errors": []}]})
        with patch("skill_discovery.subprocess.Popen", return_value=process):
            self.assertEqual(discover_skills("codex", "/repo"), skills)
        self.assertTrue(process.stdout.closed and process.stdin.closed)

    def test_partial_or_malformed_results_never_certify_discovery(self):
        cases = [None, {}, {"data": []}, {"data": [{"skills": [], "errors": ["failed to load skill"]}]},
                 {"data": [{"skills": []}, {"skills": []}]}, {"data": [{"skills": ["invalid"]}]},
                 {"data": [{"skills": [{"name": "x", "path": "/x"}]}]}]
        for result in cases:
            with self.subTest(result=result):
                process = self.process(result)
                with patch("skill_discovery.subprocess.Popen", return_value=process):
                    with self.assertRaises(ValueError):
                        discover_skills("codex", "/repo")
                self.assertTrue(process.stdout.closed and process.stdin.closed)

    def test_early_exit_does_not_wait_for_deadline(self):
        process = self.process({})
        process.stdout = io.StringIO("")
        with patch("skill_discovery.subprocess.Popen", return_value=process):
            with self.assertRaisesRegex(ValueError, "ended before"):
                discover_skills("codex", "/repo")

    def test_timeout_reaps_unresponsive_process(self):
        process = self.process({})
        process.wait.side_effect = [subprocess.TimeoutExpired("codex", 3),
                                    subprocess.TimeoutExpired("codex", 3), 0]
        with patch("skill_discovery.subprocess.Popen", return_value=process):
            with self.assertRaises(TimeoutError):
                discover_skills("codex", "/repo", timeout=0)
        process.terminate.assert_called_once()
        process.kill.assert_called_once()
        self.assertTrue(process.stdout.closed and process.stdin.closed)


if __name__ == "__main__":
    unittest.main()
