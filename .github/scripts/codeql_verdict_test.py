"""Offline structural/identity fixtures; no token or network required."""
import copy
import contextlib
import io
import json
import os
import sys
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import codeql_verdict as gate

SHA = "a" * 40
REF = "refs/heads/main"
SID = "11111111-2222-3333-4444-555555555555"


def report(language="go"):
    pack = "javascript" if language == "javascript-typescript" else language
    return {"version": "2.1.0", "runs": [{
        "automationDetails": {"id": f"/language:{language}/"},
        "tool": {"driver": {"name": "CodeQL", "semanticVersion": "2.27.4"},
                 "extensions": [{"name": f"codeql/{pack}-queries", "rules": [{"id": r} for r in gate.REQUIRED_RULES[language]]}]},
        "results": [], "versionControlProvenance": [{"branch": REF, "repositoryUri": "https://github.com/eighred/kanz", "revisionId": SHA}],
    }]}


class API:
    def __init__(self):
        self.status = {"processing_status": "complete", "analyses_url": gate.API + "/code-scanning/analyses?sarif_id=" + SID}
        self.report = report()
        self.analysis = {"id": 42, "url": gate.API + "/code-scanning/analyses/42", "sarif_id": SID,
                         "commit_sha": SHA, "ref": REF, "analysis_key": ".github/workflows/codeql.yml:analyze",
                         "category": "/language:go", "error": "", "warning": "", "results_count": 0,
                         "rules_count": len(gate.REQUIRED_RULES["go"]),
                         "environment": json.dumps({"language": "go", "build-mode": "autobuild"})}
        self.inventory = [self.analysis]

    def get(self, path, sarif=False):
        if path == "/code-scanning/sarifs/" + SID:
            return copy.deepcopy(self.status)
        if path == "/code-scanning/analyses?sarif_id=" + SID + "&per_page=100&page=1":
            return copy.deepcopy(self.inventory)
        if path == "/code-scanning/analyses/42":
            return copy.deepcopy(self.report if sarif else self.analysis)
        raise AssertionError("unexpected endpoint")


class VerdictTests(unittest.TestCase):
    def verify(self, api):
        return gate.verify_upload(api, SID, "go", SHA, REF, gate.report(report(), "go"))

    def test_actual_project_shape_all_languages_no_driver_rules_or_api_invocations(self):
        for language in gate.REQUIRED_RULES:
            self.assertEqual(gate.report(report(language), language), set(gate.REQUIRED_RULES[language]))
        self.assertEqual(self.verify(API()), 42)

    def test_additional_rules_allowed_but_missing_duplicate_or_wrong_pack_refused(self):
        value = report()
        rules = value["runs"][0]["tool"]["extensions"][0]["rules"]
        rules.append({"id": "go/new-reviewed-by-zero-verdict"})
        self.assertEqual(len(gate.report(value, "go")), 35)
        for kind in ("missing", "duplicate", "wrong-pack", "empty", "fake-healthy"):
            value = report(); run = value["runs"][0]; ext = run["tool"]["extensions"][0]
            if kind == "missing": ext["rules"].pop()
            if kind == "duplicate": ext["rules"].append(ext["rules"][0])
            if kind == "wrong-pack": ext["name"] = "codeql/python-queries"
            if kind == "empty": ext["rules"] = []
            if kind == "fake-healthy": run["tool"] = {"driver": {"name": "CodeQL", "semanticVersion": "2.27.4"}}
            with self.subTest(kind=kind), self.assertRaises(gate.Refused): gate.report(value, "go")

    def test_every_result_refused_including_critical_suppressed_and_absent(self):
        for finding in ({"ruleId": "go/example", "properties": {"security-severity": "9.8"}},
                        {"suppressions": [{"status": "accepted"}]}, {"baselineState": "absent"}, None):
            value = report(); value["runs"][0]["results"] = [finding]
            with self.assertRaises(gate.Refused): gate.report(value, "go")

    def test_missing_results_runs_category_external_data_and_failed_invocation(self):
        for kind in ("results", "runs", "multiple", "category", "external", "invocation", "exit", "notification"):
            value = report(); run = value["runs"][0]
            if kind == "results": del run["results"]
            if kind == "runs": value["runs"] = []
            if kind == "multiple": value["runs"].append(copy.deepcopy(run))
            if kind == "category": run["automationDetails"]["id"] = "/language:python/"
            if kind == "external": run["externalPropertyFileReferences"] = {"results": [{}]}
            if kind == "invocation": run["invocations"] = [{"executionSuccessful": False}]
            if kind == "exit": run["invocations"] = [{"executionSuccessful": True, "exitCode": 1}]
            if kind == "notification": run["invocations"] = [{"executionSuccessful": True, "toolExecutionNotifications": [{"level": "error"}]}]
            with self.subTest(kind=kind), self.assertRaises(gate.Refused): gate.report(value, "go")

    def test_declared_full_inventory_is_insufficient_without_processed_upload(self):
        for status in ("pending", "failed", "", None):
            api = API(); api.status["processing_status"] = status
            with self.assertRaises(gate.Refused): self.verify(api)
        api = API(); api.status["errors"] = ["failed"]
        with self.assertRaises(gate.Refused): self.verify(api)

    def test_upload_exact_source_and_shape_binding(self):
        for key, value in (("commit_sha", "b"*40), ("ref", "refs/pull/9/merge"), ("sarif_id", "stale"),
                           ("category", "/language:python"), ("analysis_key", "other:analyze"), ("url", "https://other/42"),
                           ("results_count", 1), ("results_count", False), ("rules_count", 1), ("id", True),
                           ("error", "failed"), ("warning", "partial"), ("environment", "{}")):
            api = API(); api.analysis[key] = value
            with self.subTest(key=key, value=value), self.assertRaises(gate.Refused): self.verify(api)

    def test_missing_duplicate_and_foreign_upload_inventory(self):
        for count in (0, 2, 100):
            api = API(); api.inventory = [api.analysis]*count
            with self.assertRaises(gate.Refused): self.verify(api)
        api = API(); api.status["analyses_url"] = "https://other/"
        with self.assertRaises(gate.Refused): self.verify(api)

    def test_remote_findings_provenance_and_query_mismatch(self):
        for kind in ("findings", "provenance", "rules"):
            api = API(); run = api.report["runs"][0]
            if kind == "findings": run["results"] = [{}]
            if kind == "provenance": run["versionControlProvenance"][0]["revisionId"] = "b"*40
            if kind == "rules": run["tool"]["extensions"][0]["rules"].append({"id": "go/extra"})
            with self.assertRaises(gate.Refused): self.verify(api)

    def test_refresh_race_refuses(self):
        api = API(); get = api.get; reads = 0
        def changing(path, sarif=False):
            nonlocal reads
            value = get(path, sarif)
            if path.startswith("/code-scanning/sarifs/"):
                reads += 1
                if reads == 2: value["processing_status"] = "pending"
            return value
        with patch.object(api, "get", side_effect=changing), self.assertRaises(gate.Refused): self.verify(api)

    @unittest.skipUnless(sys.platform == "linux", "Linux report reader proof")
    def test_bounded_complete_local_inventory_and_duplicate_json(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)/"go.sarif"
            path.write_text(json.dumps(report()), encoding="utf8")
            value, digest = gate.read_report(directory, "go")
            self.assertEqual(value, report()); self.assertEqual(len(digest), 64)
            extra = Path(directory)/"python.sarif"; extra.write_text("{}")
            with self.assertRaises(gate.Refused): gate.read_report(directory, "go")
            extra.unlink(); path.write_text('{"runs":[],"runs":[]}')
            with self.assertRaises(gate.Refused): gate.read_report(directory, "go")
            path.write_text('{"runs":')
            with self.assertRaises(gate.Refused): gate.read_report(directory, "go")
            path.write_text("x"*100)
            with patch.object(gate, "MAX_BYTES", 10), self.assertRaises(gate.Refused): gate.read_report(directory, "go")

    @unittest.skipUnless(sys.platform == "linux", "Linux no-follow/nonblocking file proof")
    def test_linux_symlink_and_fifo_refuse(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            target = root/"target"
            target.write_text(json.dumps(report()))
            reports = root/"reports"; reports.mkdir()
            path = reports/"go.sarif"; path.symlink_to(target)
            with self.assertRaises(gate.Refused): gate.read_report(reports, "go")
            path.unlink(); os.mkfifo(path)
            with self.assertRaises(gate.Refused): gate.read_report(reports, "go")

    @unittest.skipUnless(sys.platform == "linux", "Linux report reader proof")
    def test_javascript_report_uses_canonical_language_filename(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)/"javascript.sarif"
            path.write_text(json.dumps(report("javascript-typescript")))
            value, _ = gate.read_report(directory, "javascript-typescript")
            self.assertEqual(gate.report(value, "javascript-typescript"), set(gate.REQUIRED_RULES["javascript-typescript"]))

    def test_malformed_and_oversized_json_refuses_on_every_platform(self):
        for raw in (b'{"runs":[],"runs":[]}', b'{"runs":', b'{"value":NaN}', b'\xff'):
            with self.assertRaises(gate.Refused): gate.decode(raw)
        with patch.object(gate, "MAX_BYTES", 3), self.assertRaises(gate.Refused): gate.decode(b"null")

    def test_unsupported_platform_cannot_drop_file_safety_flags(self):
        with patch.object(gate.sys, "platform", "win32"), self.assertRaises(gate.Refused):
            gate.read_report("unused", "go")

    def test_source_execution_flags(self):
        env = {"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "eighred/kanz", "GITHUB_SHA": SHA,
               "GITHUB_REF": REF, "GITHUB_EVENT_NAME": "push", "ANALYZE_OUTCOME": "success",
               "GITHUB_RUN_ID": "1", "GITHUB_RUN_ATTEMPT": "2"}
        self.assertEqual(gate.source(env), (SHA, REF))
        for key, value in (("GITHUB_ACTIONS", "false"), ("GITHUB_REPOSITORY", "fork/kanz"),
                           ("GITHUB_SHA", "HEAD"), ("GITHUB_REF", "refs/tags/v1.0.0"),
                           ("ANALYZE_OUTCOME", "skipped"), ("CODEQL_ACTION_EXTRA_OPTIONS", "{}"),
                           ("GITHUB_RUN_ATTEMPT", "0"), ("GITHUB_EVENT_NAME", "pull_request_target")):
            with self.subTest(key=key), self.assertRaises(gate.Refused): gate.source({**env, key:value})

    def test_only_go_has_explicit_full_analysis_control(self):
        gate.overlay_mode("go", {"CODEQL_OVERLAY_DATABASE_MODE":"none"})
        for language in ("actions", "python", "javascript-typescript"):
            gate.overlay_mode(language, {})
            with self.assertRaises(gate.Refused):
                gate.overlay_mode(language, {"CODEQL_OVERLAY_DATABASE_MODE":"none"})
        for mode in ("", "overlay", "overlay-base", "NONE"):
            with self.assertRaises(gate.Refused):
                gate.overlay_mode("go", {"CODEQL_OVERLAY_DATABASE_MODE":mode})

    def test_refusal_diagnostics_never_emit_messages_or_locations(self):
        notes = [{"level":"error", "descriptor":{"id":"go/diagnostics/extraction-errors"},
                  "message":{"text":"private-marker"}, "locations":[{"uri":"private-marker"}]},
                 {"level":"error", "descriptor":{"id":"https://private-marker"}}]
        output = io.StringIO()
        with contextlib.redirect_stderr(output):
            gate.diagnostic_summary(notes)
        self.assertEqual(output.getvalue(), "CodeQL notification errors=2 ids=go/diagnostics/extraction-errors,unclassified\n")
        self.assertNotIn("private-marker", output.getvalue())
        notes *= 1000
        with contextlib.redirect_stderr(io.StringIO()) as output:
            gate.diagnostic_summary(notes)
        self.assertLess(len(output.getvalue()), 256)

    def test_go_extraction_generates_the_canonical_sdk_first(self):
        workflow = (Path(__file__).resolve().parents[1]/"workflows/codeql.yml").read_text(encoding="utf8")
        def valid(text):
            start = text.find("    - name: Set up canonical Go toolchain")
            end = text.find("    # Initializes the CodeQL tools for scanning.")
            if start < 0 or end < start:
                return False
            setup = text[start:end]
            return (setup.count("if: matrix.language == 'go'") == 3
                    and "actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e" in setup
                    and "go-version-file: kanz/go.mod" in setup
                    and "uses: ./.github/actions/setup-buf" in setup
                    and "version: 1.69.0" in setup
                    and "protoc-gen-go@v1.36.11" in setup
                    and "protoc-gen-go-grpc@v1.5.1" in setup
                    and 'export PATH="$(go env GOPATH)/bin:$PATH"' in setup
                    and "working-directory: kanz-schemas" in setup
                    and "buf generate" in setup
                    and "bash bootstrap-go-sdk.sh" in setup
                    and 'echo "CODEQL_OVERLAY_DATABASE_MODE=none" >> "$GITHUB_ENV"' in setup
                    and "continue-on-error" not in setup)
        self.assertTrue(valid(workflow))
        for old, new in (("buf generate", "echo skipped"),
                         ("CODEQL_OVERLAY_DATABASE_MODE=none", "CODEQL_OVERLAY_DATABASE_MODE=overlay"),
                         ("bash bootstrap-go-sdk.sh", "echo skipped"),
                         ("protoc-gen-go-grpc@v1.5.1", "protoc-gen-go-grpc@latest"),
                         ("version: 1.69.0", "version: latest"),
                         ("if: matrix.language == 'go'", "if: false"),
                         ("    # Initializes the CodeQL tools for scanning.", "    # removed")):
            with self.subTest(mutation=old):
                self.assertFalse(valid(workflow.replace(old, new, 1)))

    def test_workflow_wires_current_action_outputs_and_retains_default_queries(self):
        workflow = (Path(__file__).resolve().parents[1]/"workflows/codeql.yml").read_text(encoding="utf8")
        def valid(text):
            live = "\n".join(line.split("#", 1)[0].rstrip() for line in text.splitlines() if not line.lstrip().startswith("#"))
            marker = "    - name: Test zero-result verdict boundary"
            expected = """    - name: Test zero-result verdict boundary
      run: python3 -m unittest discover -s .github/scripts -p codeql_verdict_test.py

    - name: Perform CodeQL Analysis
      id: analyze
      uses: github/codeql-action/analyze@9b3254085494b55a0f7c4c2b3d2d52cc712ce370
      with:
        category: "/language:${{matrix.language}}"
        skip-queries: false
        wait-for-processing: true

    - name: Require zero CodeQL results from this analysis
      env:
        GH_TOKEN: ${{ github.token }}
        ANALYZE_OUTCOME: ${{ steps.analyze.outcome }}
        SARIF_DIRECTORY: ${{ steps.analyze.outputs.sarif-output }}
        SARIF_ID: ${{ steps.analyze.outputs.sarif-id }}
      run: python3 .github/scripts/codeql_verdict.py --language '${{ matrix.language }}'"""
            if live.count(marker) != 1 or live[live.index(marker):].strip() != expected.strip():
                return False
            prelude = live[:live.index(marker)]
            return not any(x in prelude for x in ("queries:", "config-file:", "config:", "packs:", "continue-on-error:"))
        self.assertTrue(valid(workflow))
        mutations = [workflow.replace("skip-queries: false", "skip-queries: true"),
                     workflow.replace("wait-for-processing: true", "wait-for-processing: false"),
                     workflow.replace("${{ steps.analyze.outcome }}", "success"),
                     workflow.replace("${{ steps.analyze.outputs.sarif-id }}", "old-upload"),
                     workflow.replace("${{ steps.analyze.outputs.sarif-output }}", "fixture-dir"),
                     workflow.replace("      id: analyze", "      id: analyze\n      continue-on-error: true"),
                     workflow.replace("    - name: Require zero CodeQL results from this analysis", "    - name: Require zero CodeQL results from this analysis\n      if: false"),
                     workflow.replace("        languages:", "        queries: ./fake.qls\n        languages:")]
        for text in mutations:
            self.assertFalse(valid(text))


if __name__ == "__main__":
    unittest.main()
